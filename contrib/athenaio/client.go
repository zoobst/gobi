package athenaio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// awsString is aws.String — one-line alias to keep call sites tight.
func awsString(s string) *string { return aws.String(s) }

// isGlueNotFound reports whether err is an EntityNotFoundException
// from Glue (or an equivalent shape wrapped by aws-sdk-go-v2). Used
// to make Close idempotent — dropping an already-dropped table isn't
// an error worth surfacing.
func isGlueNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "EntityNotFoundException"
	}
	return false
}

// defaultPollInterval is the initial GetQueryExecution poll spacing.
// The polling loop exponential-backs off from here up to
// defaultPollMax.
const (
	defaultPollInterval    = 500 * time.Millisecond
	defaultPollMax         = 5 * time.Second
	defaultMaxPollDuration = 5 * time.Minute
)

// Client is athenaio's entry point. Wraps aws-sdk-go-v2 clients +
// workgroup / result-location context, reused across many queries in
// a typical workflow. Analogous to pgio's *Client (which wraps a
// *pgxpool.Pool) — the shape is intentional across contrib packages.
//
// Zero value is not usable — construct via NewClient.
type Client struct {
	cfg    ClientConfig
	athena AthenaAPI
	s3     S3API
	glue   GlueAPI

	// Tracked tables created by UnloadAndRead. Consumed by
	// Client.Close to drop Glue catalog entries. Guarded by mu.
	mu             sync.Mutex
	createdTables  []trackedTable
	clientCloseErr error

	// hiveFallbackOnly is set to true after the first Iceberg CTAS
	// fails with an "Iceberg not supported" style error — subsequent
	// UnloadAndRead calls skip the Iceberg attempt and go straight to
	// the Hive shape. Sticky per Client to avoid retrying probes
	// every call once we've discovered the workgroup is engine-v2.
	// Guarded by mu.
	hiveFallbackOnly bool

	// warnLog is optional — invoked when the Iceberg → Hive fallback
	// kicks in. Nil suppresses the warning; tests inject a recorder
	// to assert the fallback path was taken. Production callers who
	// want visibility set it to (e.g.) log.Printf.
	warnLog func(format string, args ...any)
}

// trackedTable is one row of the created-tables ledger. Fields
// captured at CTAS time so Close doesn't need to re-derive them.
type trackedTable struct {
	Database        string
	Name            string
	Cleanup         Cleanup
	Format          TableFormat
	ExternalLocation string // s3://... for CleanupAll deletion (step 6b)
}

// NewClient builds a Client from cfg. Constructs Athena + S3 SDK
// clients from cfg.AWSConfig unless cfg.Athena / cfg.S3 are already
// set (test injection).
//
// Validates that Workgroup + ResultLocation are non-empty and
// applies defaults for PollInterval / MaxPollDuration.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Workgroup == "" {
		return nil, fmt.Errorf("athenaio: NewClient: Workgroup is required")
	}
	if cfg.ResultLocation == "" {
		return nil, fmt.Errorf("athenaio: NewClient: ResultLocation is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.MaxPollDuration <= 0 {
		cfg.MaxPollDuration = defaultMaxPollDuration
	}
	if cfg.Cleanup == CleanupInherit {
		cfg.Cleanup = CleanupCatalogOnly
	}

	if cfg.ClientID == "" {
		id, err := randomHex(8)
		if err != nil {
			return nil, fmt.Errorf("athenaio: NewClient: generate ClientID: %w", err)
		}
		cfg.ClientID = id
	}

	c := &Client{cfg: cfg, warnLog: cfg.WarnLog}
	if cfg.Athena != nil {
		c.athena = cfg.Athena
	} else {
		c.athena = athena.NewFromConfig(cfg.AWSConfig)
	}
	if cfg.S3 != nil {
		c.s3 = cfg.S3
	} else {
		c.s3 = s3.NewFromConfig(cfg.AWSConfig)
	}
	if cfg.Glue != nil {
		c.glue = cfg.Glue
	} else {
		c.glue = glue.NewFromConfig(cfg.AWSConfig)
	}
	return c, nil
}

// randomHex returns a hex-encoded random string of nBytes source
// bytes. Used for ClientID + per-query table-name suffixes; not
// security-sensitive but should be unique across concurrent Clients.
func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Workgroup returns the configured Athena workgroup. Read-only —
// change requires a new Client.
func (c *Client) Workgroup() string { return c.cfg.Workgroup }

// ResultLocation returns the configured S3 result prefix.
func (c *Client) ResultLocation() string { return c.cfg.ResultLocation }

// ClientID returns the stable identifier for this Client instance.
// Attached as a Glue tag on every table created by UnloadAndRead.
func (c *Client) ClientID() string { return c.cfg.ClientID }

// Close disposes of tables athenaio created during this Client's
// lifetime. Walks the tracked-table ledger; drops each per its
// Cleanup setting.
//
// Semantics per table:
//
//   - CleanupCatalogOnly (default): DROP TABLE via Glue DeleteTable.
//     External-table drop is metadata-only — S3 files stay. Iceberg
//     leaves manifest / snapshot files under external_location;
//     step 6a treats those as user-owned like the parquet output.
//   - CleanupAll: same DROP TABLE plus S3 DeleteObjects on the
//     external_location prefix. NOT IMPLEMENTED in step 6a — falls
//     back to CleanupCatalogOnly.
//   - CleanupNone: skipped entirely. Table remains in the catalog
//     for post-mortem inspection. Still tracked in-memory in case
//     downstream tooling wants to introspect.
//
// Idempotent — safe to call twice. Errors dropping individual
// tables are collected and joined into the return value; Close
// still attempts every table before returning. Missing tables
// (already dropped externally) don't produce errors — Glue returns
// EntityNotFoundException which is swallowed.
//
// Concurrent-safe with in-flight UnloadAndRead calls, but callers
// should typically Close after all queries have completed to avoid
// racing a Cleanup against an active scan.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	tables := c.createdTables
	c.createdTables = nil
	c.mu.Unlock()

	var errs []error
	for _, t := range tables {
		if t.Cleanup == CleanupNone {
			continue
		}
		if err := c.dropTable(ctx, t); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// dropTable issues Glue DeleteTable for t, then optionally deletes
// the S3 objects under t.ExternalLocation when Cleanup == CleanupAll.
// Swallows EntityNotFoundException on the Glue side so Close is
// idempotent. S3 deletion errors are wrapped but non-fatal — the
// catalog cleanup already succeeded, so a partial failure is better
// surfaced than rolled back.
func (c *Client) dropTable(ctx context.Context, t trackedTable) error {
	_, err := c.glue.DeleteTable(ctx, &glue.DeleteTableInput{
		DatabaseName: awsString(t.Database),
		Name:         awsString(t.Name),
	})
	if err != nil && !isGlueNotFound(err) {
		return fmt.Errorf("athenaio: DeleteTable %s.%s: %w", t.Database, t.Name, err)
	}
	if t.Cleanup == CleanupAll && t.ExternalLocation != "" {
		if err := c.deletePrefix(ctx, t.ExternalLocation); err != nil {
			return fmt.Errorf("athenaio: CleanupAll delete %s: %w", t.ExternalLocation, err)
		}
	}
	return nil
}

// deletePrefix removes every S3 object under the given s3://
// prefix. Walks ListObjectsV2 pages + issues DeleteObjects in
// batches of up to 1000 (the S3 API cap). Cost-aware: many
// DeleteObjects calls on large result sets are not free — document
// the CleanupAll tradeoff on ClientConfig.Cleanup.
func (c *Client) deletePrefix(ctx context.Context, prefix string) error {
	bucket, keyPrefix, err := parseS3URI(prefix)
	if err != nil {
		return err
	}
	var token *string
	for {
		resp, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            awsString(bucket),
			Prefix:            awsString(keyPrefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("ListObjectsV2 %s: %w", prefix, err)
		}
		if len(resp.Contents) > 0 {
			ids := make([]s3types.ObjectIdentifier, 0, len(resp.Contents))
			for _, obj := range resp.Contents {
				if obj.Key == nil {
					continue
				}
				k := *obj.Key
				ids = append(ids, s3types.ObjectIdentifier{Key: &k})
			}
			// S3's DeleteObjects accepts up to 1000 keys per call.
			// ListObjectsV2 pages return at most 1000, so one
			// DeleteObjects per page is safe.
			_, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: awsString(bucket),
				Delete: &s3types.Delete{Objects: ids},
			})
			if err != nil {
				return fmt.Errorf("DeleteObjects %s: %w", prefix, err)
			}
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			return nil
		}
		token = resp.NextContinuationToken
	}
}

// registerTable appends a trackedTable to the ledger under the
// client mutex. Called by UnloadAndRead after a successful CTAS
// completes so Close can find it.
func (c *Client) registerTable(t trackedTable) {
	c.mu.Lock()
	c.createdTables = append(c.createdTables, t)
	c.mu.Unlock()
}

package athenaio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/aws/aws-sdk-go-v2/aws"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"golang.org/x/sync/errgroup"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// icebergHashTag is the HashFn string emitted on the PartitionMetadata
// claim of an Iceberg CTAS result. See the tag registry in
// contrib/athenaio/PARTITION-METADATA.md.
const icebergHashTag = "athenaio/iceberg/murmur3-32/v1"

// hiveHashTag is the HashFn string for a Hive-format CTAS. Distinct
// from icebergHashTag so AlignedWith refuses cross-format claims —
// the hash functions are genuinely different (Java hashCode-based
// vs. Murmur3-32) and shouldn't be assumed interchangeable.
const hiveHashTag = "athenaio/hive/bucket/v1"

// UnloadAndRead is the T3 entry point: submit a CTAS wrapping
// spec.SQL with partitioning + bucketing, poll to completion, verify
// the resulting Glue table matches the spec, read the parquet result
// files, and return a LazyFrame carrying a PartitionMetadata claim
// that lets gobi's alignment predicate fire for shuffle-free .Over(K)
// / partition-wise Join / repartition-skip GroupBy on the join key.
//
// Step 6a scope: Iceberg format only, catalog-only cleanup. Hive
// fallback and CleanupAll's S3 side land in step 6b.
//
// The composed table stays in the Glue catalog until Close is called
// (or the user explicitly drops it) — track the LazyFrame's
// QueryStats.QueryExecutionID + TableName for out-of-band cleanup
// after crashes.
func (c *Client) UnloadAndRead(ctx context.Context, spec UnloadSpec) (*gobi.LazyFrame, error) {
	start := time.Now()

	// Opt-in LIMIT-0 prepass: confirm partition columns exist in
	// the SELECT projection before submitting the CTAS. Adds one
	// Athena round-trip but produces a clean fail-fast error
	// rather than a nested CTAS failure that surfaces the same
	// mistake with a much less readable stack.
	if spec.ValidatePartitionCols {
		cols, err := c.runPrepass(ctx, spec.SQL)
		if err != nil {
			return nil, fmt.Errorf("athenaio: UnloadAndRead prepass: %w", err)
		}
		if err := verifyPartitionColsPresent(spec.PartitionBy, cols); err != nil {
			return nil, fmt.Errorf("athenaio: UnloadAndRead prepass: %w", err)
		}
	}

	// Format resolution + fallback loop:
	//   - FormatHive forces Hive directly (no Iceberg attempt).
	//   - FormatIceberg forces Iceberg (no Hive fallback on failure).
	//   - FormatUnknown / FormatIceberg-default: try Iceberg first
	//     unless we've already learned this workgroup rejects it
	//     (hiveFallbackOnly sticks across calls per Client).
	useHive := spec.TableFormat == FormatHive
	if !useHive && c.getHiveFallbackOnly() {
		useHive = true
	}

	composed, queryID, exec, err := c.tryCTAS(ctx, spec, useHive)
	if err != nil {
		// If Iceberg-not-supported and the caller isn't forcing
		// Iceberg, retry with Hive. Log the fallback via warnLog.
		if !useHive && spec.TableFormat != FormatIceberg && isIcebergNotSupportedErr(err) {
			if c.warnLog != nil {
				c.warnLog("athenaio: workgroup %s rejected Iceberg CTAS; falling back to Hive format", c.cfg.Workgroup)
			}
			c.setHiveFallbackOnly()
			composed, queryID, exec, err = c.tryCTAS(ctx, spec, true)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// Read-back verify via Glue. Confirms the actual table matches
	// what we asked for; hard error on mismatch (never silently
	// narrow PartitionMetadata).
	if err := c.verifyCTASOutput(ctx, composed, spec); err != nil {
		// Register the table for cleanup even though verify failed —
		// otherwise a mismatch leaves the orphan in Glue forever.
		c.registerTable(trackedTable{
			Database:         c.cfg.Database,
			Name:             composed.TableName,
			Cleanup:          c.effectiveCleanup(spec),
			Format:           composed.Format,
			ExternalLocation: composed.ExternalLocation,
		})
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s read-back verify: %w", queryID, err)
	}

	// Register the table so Client.Close drops it.
	c.registerTable(trackedTable{
		Database:         c.cfg.Database,
		Name:             composed.TableName,
		Cleanup:          c.effectiveCleanup(spec),
		Format:           composed.Format,
		ExternalLocation: composed.ExternalLocation,
	})

	// List the parquet bucket files under the *actual* Glue-recorded
	// location — not the composed external_location, which the
	// workgroup may have silently overridden when
	// EnforceWorkGroupConfiguration=true. Concatenate in listing
	// order — Iceberg bucketing guarantees same-K rows only appear
	// in one bucket, so the concat is contiguous by K globally when
	// sorted_by is set.
	actualLoc, err := c.resolveActualLocation(ctx, c.cfg.Database, composed.TableName, composed.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, &NoResultFilesError{
			Op:       "UnloadAndRead",
			QueryID:  queryID,
			Location: actualLoc,
			Stats: &QueryStats{
				QueryExecutionID: queryID,
				ResultPrefix:     composed.ExternalLocation,
				ScannedBytes:     scannedBytes(exec),
				EngineTime:       engineTime(exec),
				TotalTime:        time.Since(start),
			},
		}
	}
	frame, err := c.readBucketFiles(ctx, files, readOptsFromSpec(spec.Columns, spec.Predicate))
	if err != nil {
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s: %w", queryID, err)
	}

	// Attach the PartitionMetadata claim. HashFn + SortEnforced
	// differ per format so the alignment predicate correctly
	// refuses cross-format claims (Iceberg's Murmur3-32 hash isn't
	// interchangeable with Hive's Java hashCode-based hash) and
	// downstream operators refuse to trust Hive's hint-only sort.
	meta := &gobi.PartitionMetadata{
		Columns:      append([]string(nil), spec.PartitionBy...),
		HashFn:       hashTagFor(composed.Format),
		SortedBy:     append([]gobi.SortKey(nil), spec.OrderBy...),
		SortEnforced: composed.Format == FormatIceberg && len(spec.OrderBy) > 0,
	}
	frame.WithPartitionMeta(meta)
	lf := frame.Lazy()

	// Wrap in a WithPartitionAssertion so the claim is carried
	// through the plan tree, not just on the root Frame. The
	// assertion narrowing rule accepts nil→any so this is always
	// valid on a fresh scanFrameNode.
	asserted, err := lf.WithPartitionAssertion(meta)
	if err != nil {
		return nil, fmt.Errorf("athenaio: attach partition assertion: %w", err)
	}

	registerStats(asserted, QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     composed.ExternalLocation,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        time.Since(start),
		RowCount:         int64(frame.NumRows()),
	})
	return asserted, nil
}

// RawCTAS submits a caller-composed CTAS statement. Escape hatch
// for advanced use cases — custom hash functions, Iceberg-specific
// properties athenaio's UnloadAndRead doesn't expose, composite
// partition transforms. Symmetric with LazyFrame.WithPartitionAssertion
// on the read side: gobi trusts the caller's Metadata claim without
// verification, and correctness is the caller's responsibility.
//
// Contract:
//
//   - spec.SQL is submitted verbatim. No wrapping, no LIMIT-0 prepass,
//     no composed outer clause. Include the full CREATE TABLE ... AS.
//   - spec.TableName + spec.ExternalLocation must match what SQL
//     actually creates — athenaio doesn't parse SQL to derive them.
//   - spec.Metadata (if non-nil) is attached to the returned
//     LazyFrame as-is. A wrong claim produces silently-wrong results
//     when downstream operators consume the alignment.
//   - The table is registered for cleanup on Client.Close, honoring
//     spec.Cleanup.
//
// Errors on submit / poll / read-back mirror UnloadAndRead's shape.
// The composed SQL is included in error messages for debuggability.
func (c *Client) RawCTAS(ctx context.Context, spec RawCTASSpec) (*gobi.LazyFrame, error) {
	lf, _, err := c.rawCTAS(ctx, spec)
	return lf, err
}

// CTASMetadata is the observability blob returned by
// RawCTASWithMetadata alongside the LazyFrame. Every field is
// derivable from what the Client already computes internally
// during a RawCTAS call — surfacing it here spares callers a
// second round-trip to Glue / Athena for correlation, logging,
// or downstream location-aware operations (e.g. reading extra
// non-CTAS-produced files that co-exist under the same prefix).
type CTASMetadata struct {
	// Location is the resolved S3 URI prefix Athena actually wrote
	// to, as reported by Glue. May differ from
	// RawCTASSpec.ExternalLocation when a workgroup with
	// EnforceWorkGroupConfiguration=true overrides the output
	// prefix. Callers listing / deleting the CTAS output should use
	// this value, not the spec's ExternalLocation.
	Location string
	// QueryID is the Athena execution ID for the CTAS statement.
	// Correlates with CloudTrail, Athena query history, and
	// QueryStats attached to the returned LazyFrame.
	QueryID string
	// Duration is the wall-clock time from submit to reader-open.
	// Same value stored in QueryStats.TotalTime.
	Duration time.Duration
}

// RawCTASWithMetadata is the observability-friendly form of
// RawCTAS — returns the resolved S3 location + Athena query ID
// alongside the LazyFrame. Identical semantics and cleanup
// contract otherwise. Same shape rationale as
// UnloadAndReadBucketsWithMetadata: gives callers per-call
// telemetry without a second Glue call.
func (c *Client) RawCTASWithMetadata(ctx context.Context, spec RawCTASSpec) (*gobi.LazyFrame, CTASMetadata, error) {
	return c.rawCTAS(ctx, spec)
}

// rawCTAS is the shared body of RawCTAS + RawCTASWithMetadata.
// Returns the LazyFrame + a fully-populated CTASMetadata; the
// public RawCTAS discards the metadata.
func (c *Client) rawCTAS(ctx context.Context, spec RawCTASSpec) (*gobi.LazyFrame, CTASMetadata, error) {
	if spec.SQL == "" {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTASSpec.SQL is empty")
	}
	if spec.TableName == "" {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTASSpec.TableName is required (athenaio doesn't parse SQL)")
	}
	if spec.ExternalLocation == "" {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTASSpec.ExternalLocation is required (athenaio doesn't parse SQL)")
	}
	database := spec.Database
	if database == "" {
		database = c.cfg.Database
	}
	if database == "" {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTAS requires Database (in spec or Client config)")
	}
	cleanup := spec.Cleanup
	if cleanup == CleanupInherit {
		cleanup = c.cfg.Cleanup
	}
	start := time.Now()

	// Steer the CTAS output via ResultConfiguration.OutputLocation
	// (the caller-provided ExternalLocation). Any external_location
	// clause the user embedded in spec.SQL still passes through
	// verbatim — athenaio doesn't parse it — but OutputLocation is
	// what a workgroup-enforced setup will honor.
	queryID, err := c.submitTo(ctx, spec.SQL, spec.ExternalLocation)
	if err != nil {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTAS submit:\n---\n%s\n---\n%w", spec.SQL, err)
	}
	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTAS %s failed:\n---\n%s\n---\n%w",
			queryID, spec.SQL, err)
	}

	// Register the table for cleanup on Close. Do this before the
	// read step so a read-side failure still lets Close reap the
	// orphan. Cleanup semantics: catalog-only by default; CleanupAll
	// also deletes S3 objects.
	c.registerTable(trackedTable{
		Database:         database,
		Name:             spec.TableName,
		Cleanup:          cleanup,
		Format:           FormatUnknown, // athenaio doesn't know for RawCTAS
		ExternalLocation: spec.ExternalLocation,
	})

	// Read the result files under the *actual* Glue-recorded location.
	// See resolveActualLocation for why the composed value is unsafe
	// (workgroup override with EnforceWorkGroupConfiguration=true).
	// No read-back verification of metadata — the user asserted the
	// claim; we don't second-guess.
	actualLoc, err := c.resolveActualLocation(ctx, database, spec.TableName, spec.ExternalLocation)
	if err != nil {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}
	if len(files) == 0 {
		return nil, CTASMetadata{}, &NoResultFilesError{
			Op:       "RawCTAS",
			QueryID:  queryID,
			Location: actualLoc,
			Stats: &QueryStats{
				QueryExecutionID: queryID,
				ResultPrefix:     spec.ExternalLocation,
				ScannedBytes:     scannedBytes(exec),
				EngineTime:       engineTime(exec),
				TotalTime:        time.Since(start),
			},
		}
	}
	frame, err := c.readBucketFiles(ctx, files, readOptsFromSpec(spec.Columns, spec.Predicate))
	if err != nil {
		return nil, CTASMetadata{}, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}

	lf := frame.Lazy()
	if spec.Metadata != nil {
		frame.WithPartitionMeta(spec.Metadata)
		asserted, err := lf.WithPartitionAssertion(spec.Metadata)
		if err != nil {
			return nil, CTASMetadata{}, fmt.Errorf("athenaio: attach partition assertion: %w", err)
		}
		lf = asserted
	}

	dur := time.Since(start)
	registerStats(lf, QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     spec.ExternalLocation,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        dur,
		RowCount:         int64(frame.NumRows()),
	})
	return lf, CTASMetadata{
		Location: actualLoc,
		QueryID:  queryID,
		Duration: dur,
	}, nil
}

// tryCTAS composes + submits + polls a CTAS in the given format
// (Iceberg if useHive=false, Hive if true). Returns the composed
// spec + query ID + completed execution on success, or an error
// containing the composed SQL for debuggability. Isolated as a
// helper so UnloadAndRead can call it twice (once per format) on
// the fallback path.
func (c *Client) tryCTAS(ctx context.Context, spec UnloadSpec, useHive bool) (*composedCTAS, string, *athenatypes.QueryExecution, error) {
	composed, err := c.composeCTAS(spec, useHive)
	if err != nil {
		return nil, "", nil, err
	}
	// Submit with the composed data location as OutputLocation —
	// that's the sole knob for CTAS data placement now that the
	// WITH-clause `external_location` / `location` properties are
	// gone. Workgroups with EnforceWorkGroupConfiguration=true may
	// still override this; resolveActualLocation surfaces the
	// override to callers.
	queryID, err := c.submitTo(ctx, composed.SQL, composed.ExternalLocation)
	if err != nil {
		return composed, "", nil, fmt.Errorf("athenaio: UnloadAndRead submit: composed SQL=%s: %w", composed.SQL, err)
	}
	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return composed, queryID, nil, fmt.Errorf("athenaio: UnloadAndRead query %s failed:\n---\n%s\n---\n%w",
			queryID, composed.SQL, err)
	}
	return composed, queryID, exec, nil
}

// getHiveFallbackOnly returns the sticky "this workgroup rejects
// Iceberg" flag. Read under the Client mutex.
func (c *Client) getHiveFallbackOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hiveFallbackOnly
}

// setHiveFallbackOnly latches the fallback flag. Called after the
// first Iceberg-not-supported error so subsequent UnloadAndRead
// calls skip the failed Iceberg attempt.
func (c *Client) setHiveFallbackOnly() {
	c.mu.Lock()
	c.hiveFallbackOnly = true
	c.mu.Unlock()
}

// isIcebergNotSupportedErr reports whether err's message contains
// markers that suggest the workgroup rejected the Iceberg-specific
// properties in the CTAS. Fragile substring match — Athena error
// text can drift between versions — but pragmatic: users can force
// Hive with spec.TableFormat=FormatHive to bypass the detection
// entirely, and callers get the original error surfaced when the
// pattern doesn't match.
//
// Common shapes observed on engine-v2 workgroups:
//   - "Iceberg tables are not supported by engine version 2"
//   - "table_type is not a valid property"
//   - "'table_type' does not exist"
//   - "NOT_SUPPORTED: ... iceberg ..."
func isIcebergNotSupportedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// "iceberg" appears in Athena's error for the engine-v2 case
	// AND in athenaio's own composed-SQL echo (which includes
	// `table_type = 'ICEBERG'`). Distinguish by checking for
	// error-state markers alongside — "not supported" or the
	// property-doesn't-exist / not-valid shapes.
	if strings.Contains(msg, "iceberg") && strings.Contains(msg, "not supported") {
		return true
	}
	if strings.Contains(msg, "table_type") &&
		(strings.Contains(msg, "not a valid") || strings.Contains(msg, "does not exist")) {
		return true
	}
	return false
}

// hashTagFor returns the PartitionMetadata HashFn tag for a given
// resolved table format. Distinct tags per format so AlignedWith
// refuses cross-format alignment claims — Iceberg's Murmur3-32 and
// Hive's Java hashCode-based hash are genuinely different functions.
func hashTagFor(f TableFormat) string {
	if f == FormatHive {
		return hiveHashTag
	}
	return icebergHashTag
}

// effectiveCleanup returns the Cleanup setting for a specific
// UnloadAndRead call — spec.Cleanup if explicitly set (non-zero),
// otherwise the Client default. NewClient normalizes the Client-
// level CleanupInherit → CleanupCatalogOnly, so cfg.Cleanup is
// always a concrete value here.
func (c *Client) effectiveCleanup(spec UnloadSpec) Cleanup {
	if spec.Cleanup == CleanupInherit {
		return c.cfg.Cleanup
	}
	return spec.Cleanup
}

// verifyCTASOutput calls Glue GetTable to confirm the table Athena
// wrote matches the composed CTAS. Hard-errors on mismatch —
// silently narrowing PartitionMetadata would cause correctness bugs
// months later when the alignment claim doesn't reflect reality.
//
// Dispatches on composed.Format because Iceberg + Hive tables have
// different Glue-parameter shapes:
//   - Iceberg: `table_type=ICEBERG` parameter present;
//     StorageDescriptor.Location matches external_location.
//   - Hive: `table_type` absent or non-ICEBERG (matching-ICEBERG
//     would indicate the Hive fallback got fooled); Location check
//     is the same.
//
// Deeper partition-spec verification (matching bucket count / sort
// keys) is still deferred — the current checks prove Athena honored
// the location + high-level format, which is enough to prevent
// silent PartitionMetadata narrowing under the Hive fallback.
func (c *Client) verifyCTASOutput(ctx context.Context, composed *composedCTAS, _ UnloadSpec) error {
	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(c.cfg.Database),
		Name:         aws.String(composed.TableName),
	})
	if err != nil {
		return fmt.Errorf("GetTable %s.%s: %w", c.cfg.Database, composed.TableName, err)
	}
	if out.Table == nil {
		return fmt.Errorf("GetTable %s.%s: nil Table", c.cfg.Database, composed.TableName)
	}
	switch composed.Format {
	case FormatIceberg:
		return verifyIcebergTable(out.Table, composed)
	case FormatHive:
		return verifyHiveTable(out.Table, composed)
	default:
		return fmt.Errorf("athenaio: verifyCTASOutput: unknown format %v", composed.Format)
	}
}

// verifyIcebergTable checks a Glue Table entry against the CTAS spec
// athenaio submitted. Isolated for testability.
func verifyIcebergTable(t *gluetypes.Table, composed *composedCTAS) error {
	// Iceberg tables in Glue carry a `table_type=ICEBERG` parameter.
	// Confirm it — a Hive fallback would surface as an absent or
	// different value.
	if t.Parameters["table_type"] != "ICEBERG" {
		return fmt.Errorf("expected ICEBERG table_type, got %q", t.Parameters["table_type"])
	}
	if err := verifyLocation(t, composed); err != nil {
		return err
	}
	return nil
}

// verifyHiveTable checks a Glue Table entry produced by a Hive-shape
// CTAS. Different from verifyIcebergTable: Hive tables don't carry
// `table_type=ICEBERG` (an unset or different value is expected).
// The same StorageDescriptor.Location check applies. Also asserts
// `table_type != ICEBERG` — an Iceberg table masquerading as Hive
// would cause the Hive-fallback path to emit hive/bucket/v1
// metadata for what's actually Iceberg data (a wrong claim, not a
// narrowed one).
func verifyHiveTable(t *gluetypes.Table, composed *composedCTAS) error {
	if tt := t.Parameters["table_type"]; tt == "ICEBERG" {
		return fmt.Errorf("expected Hive table but Glue reports table_type=%q", tt)
	}
	return verifyLocation(t, composed)
}

// verifyLocation checks StorageDescriptor.Location is present. The
// exact-match check against composed.ExternalLocation is no longer
// enforced: workgroups with EnforceWorkGroupConfiguration=true
// silently override the external_location hint and write to the
// workgroup's ResultConfiguration.OutputLocation instead — a Glue
// mismatch is expected in that mode, not a bug. Callers must
// subsequently read the ground-truth location from
// resolveActualLocation and pass it to listBucketFiles.
func verifyLocation(t *gluetypes.Table, _ *composedCTAS) error {
	if t.StorageDescriptor == nil || t.StorageDescriptor.Location == nil {
		return fmt.Errorf("athenaio: Glue table missing StorageDescriptor.Location")
	}
	return nil
}

// readGlueTableLocation returns *StorageDescriptor.Location — the
// ground truth of where Athena actually wrote CTAS output. Distinct
// from the caller-composed external_location because Athena
// workgroups with EnforceWorkGroupConfiguration=true silently
// override the external_location property and route writes to the
// workgroup's ResultConfiguration.OutputLocation. Every caller that
// needs to list result files must use this value — the composed
// value points at an empty prefix in that mode.
func (c *Client) readGlueTableLocation(ctx context.Context, database, tableName string) (string, error) {
	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(database),
		Name:         aws.String(tableName),
	})
	if err != nil {
		return "", fmt.Errorf("GetTable %s.%s: %w", database, tableName, err)
	}
	if out.Table == nil {
		return "", fmt.Errorf("GetTable %s.%s: nil Table", database, tableName)
	}
	sd := out.Table.StorageDescriptor
	if sd == nil || sd.Location == nil {
		return "", fmt.Errorf("GetTable %s.%s: missing StorageDescriptor.Location", database, tableName)
	}
	return *sd.Location, nil
}

// resolveActualLocation reads the Glue-recorded location for the
// newly-created table and warns via c.warnLog when it differs from
// the caller-composed value (the workgroup-override symptom). The
// return value is the location callers should pass to
// listBucketFiles — never the composed one.
func (c *Client) resolveActualLocation(ctx context.Context, database, tableName, requested string) (string, error) {
	actual, err := c.readGlueTableLocation(ctx, database, tableName)
	if err != nil {
		return "", err
	}
	if requested != "" && !locationMatches(actual, requested) && c.warnLog != nil {
		c.warnLog(
			"athenaio: workgroup silently overrode external_location:\n"+
				"  requested: %s\n"+
				"  actual:    %s\n"+
				"  (workgroup likely has EnforceWorkGroupConfiguration=true)",
			requested, actual)
	}
	return actual, nil
}

// locationMatches compares two s3:// URIs modulo trailing slash.
func locationMatches(a, b string) bool {
	trim := func(s string) string {
		for len(s) > 0 && s[len(s)-1] == '/' {
			s = s[:len(s)-1]
		}
		return s
	}
	return trim(a) == trim(b)
}

// openBucketFrame opens one s3:// URI, reads its parquet payload via
// parquetio.ReadReader, and returns the resulting Frame. Extracted
// so the per-bucket variants (UnloadAndReadBuckets, RawCTASBuckets)
// can call it once per bucket without duplicating the S3-read plumbing.
//
// opts is passed to parquetio.ReadReader — Columns projects, Predicate
// prunes row groups. nil means "read every column, no row-group
// pruning" (the pre-v0.3.7 behavior).
//
// Returns the object's ContentLength alongside the Frame. Existing
// bucket-file readers (readBucketFiles, populateBucketResults) discard
// it — they already have per-file sizes from the driving
// ListObjectsV2 response. BucketResultsFromS3URIs uses it to surface
// Size on the returned BucketResult without a second HeadObject
// round-trip.
func (c *Client) openBucketFrame(ctx context.Context, uri string, opts *parquetio.ReadOptions) (*gobi.Frame, int64, error) {
	bucket, key, err := parseS3URI(uri)
	if err != nil {
		return nil, 0, err
	}
	ra, size, err := newS3ReaderAt(ctx, c.s3, bucket, key)
	if err != nil {
		return nil, 0, err
	}
	f, err := parquetio.ReadReader(ra, size, opts)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", uri, err)
	}
	return f, size, nil
}

// readOptsFromSpec builds a *parquetio.ReadOptions carrying columns
// and predicate, or returns nil when neither is set — preserves the
// pre-existing "opts == nil ⇒ default read" contract exactly when
// callers leave both spec fields unset.
func readOptsFromSpec(columns []string, predicate gobi.Expr) *parquetio.ReadOptions {
	if len(columns) == 0 && predicate.Node() == nil {
		return nil
	}
	return &parquetio.ReadOptions{
		Columns:   columns,
		Predicate: predicate,
	}
}

// readBucketFiles opens each s3:// URI in files, reads it via
// parquetio.ReadReader, and concatenates the resulting Frames into
// a single-chunk output Frame. First file's schema is authoritative.
// Order preserved from files argument (typically lexicographic bucket
// order from ListObjectsV2).
//
// opts applies to every per-file ReadReader — spec-derived Columns
// projection and Predicate row-group pruning propagate identically
// across the bucket set.
//
// Uses array.Concatenate for a single-chunk output rather than
// gobi.Concat (which produces multi-chunk columns) because the
// streaming executor's frameToBatch reads only chunks[0] — a multi-
// chunk Frame reaching Collect silently drops rows past the first
// chunk.
func (c *Client) readBucketFiles(ctx context.Context, files []bucketFileInfo, opts *parquetio.ReadOptions) (*gobi.Frame, error) {
	frames := make([]*gobi.Frame, 0, len(files))
	for _, fi := range files {
		// Discard size: readBucketFiles callers already have fi.Size
		// from ListObjectsV2; this path aggregates all files into one
		// concat'd Frame anyway.
		f, _, err := c.openBucketFrame(ctx, fi.URI, opts)
		if err != nil {
			for _, prev := range frames {
				prev.Release()
			}
			return nil, err
		}
		frames = append(frames, f)
	}
	if len(frames) == 1 {
		return frames[0], nil
	}
	return concatFramesSingleChunk(frames, memory.DefaultAllocator)
}

// concatFramesSingleChunk consumes frames — Releases every input Frame
// as part of building the output — and returns a single Frame whose
// per-column data is one array.Concatenate of the inputs. All input
// frames must share the same schema. First frame's schema is
// authoritative.
//
// Ownership: on return (both success AND error) every input Frame has
// been Released exactly once. Callers must not use frames after the
// call. array.Concatenate copies the data into new buffers, so the
// output Frame's arrow arrays are independent of the inputs — dropping
// the sources immediately is safe and prevents the multi-GB reader
// leak that surfaced on multi-bucket UnloadAndRead workloads.
//
// Uses single-chunk output rather than gobi.Concat (which produces
// multi-chunk columns) because the streaming executor's frameToBatch
// reads only chunks[0]; a multi-chunk Frame reaching Collect silently
// drops rows past the first chunk.
func concatFramesSingleChunk(frames []*gobi.Frame, pool memory.Allocator) (*gobi.Frame, error) {
	defer func() {
		for _, f := range frames {
			f.Release()
		}
	}()

	schema := frames[0].Schema()
	numCols := len(schema.Fields())
	outCols := make([]arrow.Column, numCols)
	for ci := range numCols {
		chunks := make([]arrow.Array, 0, len(frames))
		for _, f := range frames {
			s, err := f.ColumnAt(ci)
			if err != nil {
				return nil, fmt.Errorf("col %d: %w", ci, err)
			}
			chunks = append(chunks, s.Column().Data().Chunks()...)
		}
		combined, err := array.Concatenate(chunks, pool)
		if err != nil {
			return nil, fmt.Errorf("concat col %d: %w", ci, err)
		}
		field := schema.Field(ci)
		chunked := arrow.NewChunked(combined.DataType(), []arrow.Array{combined})
		outCols[ci] = *arrow.NewColumn(field, chunked)
		combined.Release()
		chunked.Release()
	}
	return gobi.NewFrame(schema, outCols)
}

// -----------------------------------------------------------------------------
// Per-bucket variants: UnloadAndReadBuckets + RawCTASBuckets
//
// The mainline UnloadAndRead / RawCTAS concatenate every bucket file
// into one LazyFrame, forcing downstream callers back into a single-
// plan execution — even when CTAS bucketing was set up expressly to
// enable per-bucket parallelism. The per-bucket variants return one
// LazyFrame per bucket file so callers can dispatch parallel per-
// partition work (`errgroup.WithContext` + per-frame goroutine)
// without reimplementing the S3-list-and-read plumbing.
//
// Each returned LazyFrame carries the same PartitionMetadata claim
// as the mainline variant. Alignment holds within-bucket (same-K
// rows are contained in a single bucket, per the bucketing invariant)
// but NOT across bucket indices — bucket i on left and bucket i on
// right of two separate UnloadAndReadBuckets calls are alignment-
// compatible; bucket i on left and bucket j on right are not.
// -----------------------------------------------------------------------------

// BucketResult pairs a per-bucket LazyFrame with the S3 URI it was
// read from. Returned by the *WithMetadata variants when the caller
// wants per-bucket telemetry, logging, or correlation with Athena's
// query stats. Frame may be nil for skipped/missing bucket indices
// under strict PartitionBy+BucketCount contracts.
type BucketResult struct {
	// S3URI is the fully-qualified `s3://bucket/key` the frame was
	// read from. Empty when Frame is nil.
	S3URI string
	// Frame is the per-bucket LazyFrame. Independently readable via
	// Collect(); errors surface at Collect time on the specific
	// frame that failed (sibling frames unaffected).
	Frame *gobi.LazyFrame
	// Size is the S3 object size in bytes as reported by
	// ListObjectsV2 at read time. Zero for nil-Frame slots (no file
	// existed for that bucket). Useful for skew diagnostics and
	// downstream cost estimation without a per-file HEAD call.
	//
	// Callers computing average file size across the returned slice
	// should divide by the count of non-nil BucketResults, not by
	// len(results) — skew-empty buckets pull the average down
	// spuriously otherwise.
	Size int64
	// Location is the common S3 URI prefix (e.g. `s3://bucket/prefix/`)
	// the CTAS wrote to — the parent of every entry's S3URI in a
	// returned slice. Same value on every entry, including nil-Frame
	// slots, so callers can find the prefix without probing for a
	// non-nil sibling or maintaining a side channel.
	//
	// Populated from the resolved Glue-recorded location (see
	// resolveActualLocation), which may differ from spec.ExternalLocation
	// when a workgroup with EnforceWorkGroupConfiguration=true
	// overrides the output prefix.
	Location string
}

// UnloadAndReadBuckets is the bucket-aware variant of UnloadAndRead.
// Submits the same CTAS + verify + list flow, then returns one
// *gobi.LazyFrame per bucket file — each independently readable, each
// carrying the same PartitionMetadata claim. Callers can Collect them
// in parallel goroutines to exploit CTAS bucket parallelism directly.
//
// Contract:
//
//   - spec.PartitionBy must be non-empty and spec.BucketCount > 0.
//     Unbucketed CTAS output doesn't guarantee stable per-file
//     semantics; callers who want that should use UnloadAndRead.
//   - Returned slice length == spec.BucketCount. Empty bucket indices
//     (skew — a bucket ends up with no rows and Athena writes no
//     file) are represented by a nil slot at that index. Callers
//     iterating with `for i, lf := range results` should nil-check.
//   - Each LazyFrame's Collect() error is independent of siblings.
//     A bad Parquet file on bucket 3 doesn't invalidate bucket 4.
//   - Table cleanup lifecycle is unchanged from UnloadAndRead: one
//     Glue-catalog entry per CTAS, tracked on the Client for Close.
//
// Ordering of the returned slice matches S3's ListObjectsV2 output
// (lexicographic on key), which for Athena's bucket file naming
// puts bucket_00000 first. Callers should not assume the ordering
// carries semantic meaning beyond "bucket i in slice matches bucket
// i in a peer call with the same spec".
func (c *Client) UnloadAndReadBuckets(ctx context.Context, spec UnloadSpec) ([]*gobi.LazyFrame, error) {
	if len(spec.PartitionBy) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets requires non-empty spec.PartitionBy")
	}
	if spec.BucketCount <= 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets requires spec.BucketCount > 0")
	}
	results, err := c.unloadAndReadBucketsWithMeta(ctx, spec)
	if err != nil {
		return nil, err
	}
	out := make([]*gobi.LazyFrame, len(results))
	for i, r := range results {
		out[i] = r.Frame
	}
	return out, nil
}

// UnloadAndReadBucketsWithMetadata is the observability-friendly form
// of UnloadAndReadBuckets — returns per-bucket S3 URIs alongside the
// LazyFrames. Same contract otherwise.
func (c *Client) UnloadAndReadBucketsWithMetadata(ctx context.Context, spec UnloadSpec) ([]BucketResult, error) {
	if len(spec.PartitionBy) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBucketsWithMetadata requires non-empty spec.PartitionBy")
	}
	if spec.BucketCount <= 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBucketsWithMetadata requires spec.BucketCount > 0")
	}
	return c.unloadAndReadBucketsWithMeta(ctx, spec)
}

// unloadAndReadBucketsWithMeta is the shared implementation. Runs
// the full UnloadAndRead composition + submit + verify flow, then
// per-file constructs a LazyFrame with the same PartitionMetadata
// claim as the mainline variant. Missing bucket indices become nil
// slots.
func (c *Client) unloadAndReadBucketsWithMeta(ctx context.Context, spec UnloadSpec) ([]BucketResult, error) {
	prep, meta, err := c.prepareUnloadBuckets(ctx, spec)
	if err != nil {
		return nil, err
	}
	return c.hydrateBuckets(ctx, prep, meta, spec.Columns, spec.Predicate)
}

// UnloadAndReadBucketsManifest is the two-phase variant of
// UnloadAndReadBuckets. See RawCTASBucketsManifest for the shape and
// hand-off rationale; this method is the composed-CTAS-side
// counterpart.
func (c *Client) UnloadAndReadBucketsManifest(ctx context.Context, spec UnloadSpec) (
	manifest []BucketResult,
	meta CTASMetadata,
	hydrate func(context.Context) ([]BucketResult, error),
	err error,
) {
	if len(spec.PartitionBy) == 0 {
		return nil, CTASMetadata{}, nil, fmt.Errorf("athenaio: UnloadAndReadBucketsManifest requires non-empty spec.PartitionBy")
	}
	if spec.BucketCount <= 0 {
		return nil, CTASMetadata{}, nil, fmt.Errorf("athenaio: UnloadAndReadBucketsManifest requires spec.BucketCount > 0")
	}
	prep, pmeta, err := c.prepareUnloadBuckets(ctx, spec)
	if err != nil {
		return nil, CTASMetadata{}, nil, err
	}
	manifest, err = buildBucketManifest(prep)
	if err != nil {
		return nil, CTASMetadata{}, nil, fmt.Errorf("athenaio: UnloadAndReadBucketsManifest %s: %w", prep.queryID, err)
	}
	meta = CTASMetadata{
		Location: prep.actualLoc,
		QueryID:  prep.queryID,
		Duration: time.Since(prep.start),
	}
	hydrate = func(hctx context.Context) ([]BucketResult, error) {
		return c.hydrateBuckets(hctx, prep, pmeta, spec.Columns, spec.Predicate)
	}
	return manifest, meta, hydrate, nil
}

// prepareUnloadBuckets runs the composed-CTAS side of the Unload
// path up through file listing. Returns the shared bucketPrep +
// the derived PartitionMetadata (which needs the composed format
// hashTagFor tag — not derivable from the raw spec alone).
func (c *Client) prepareUnloadBuckets(ctx context.Context, spec UnloadSpec) (*bucketPrep, *gobi.PartitionMetadata, error) {
	start := time.Now()
	if spec.ValidatePartitionCols {
		cols, err := c.runPrepass(ctx, spec.SQL)
		if err != nil {
			return nil, nil, fmt.Errorf("athenaio: UnloadAndReadBuckets prepass: %w", err)
		}
		if err := verifyPartitionColsPresent(spec.PartitionBy, cols); err != nil {
			return nil, nil, fmt.Errorf("athenaio: UnloadAndReadBuckets prepass: %w", err)
		}
	}

	useHive := spec.TableFormat == FormatHive
	if !useHive && c.getHiveFallbackOnly() {
		useHive = true
	}
	composed, queryID, exec, err := c.tryCTAS(ctx, spec, useHive)
	if err != nil {
		if !useHive && spec.TableFormat != FormatIceberg && isIcebergNotSupportedErr(err) {
			if c.warnLog != nil {
				c.warnLog("athenaio: workgroup %s rejected Iceberg CTAS; falling back to Hive format", c.cfg.Workgroup)
			}
			c.setHiveFallbackOnly()
			composed, queryID, exec, err = c.tryCTAS(ctx, spec, true)
			if err != nil {
				return nil, nil, err
			}
		} else {
			return nil, nil, err
		}
	}

	if err := c.verifyCTASOutput(ctx, composed, spec); err != nil {
		c.registerTable(trackedTable{
			Database:         c.cfg.Database,
			Name:             composed.TableName,
			Cleanup:          c.effectiveCleanup(spec),
			Format:           composed.Format,
			ExternalLocation: composed.ExternalLocation,
		})
		return nil, nil, fmt.Errorf("athenaio: UnloadAndReadBuckets %s read-back verify: %w", queryID, err)
	}
	c.registerTable(trackedTable{
		Database:         c.cfg.Database,
		Name:             composed.TableName,
		Cleanup:          c.effectiveCleanup(spec),
		Format:           composed.Format,
		ExternalLocation: composed.ExternalLocation,
	})

	actualLoc, err := c.resolveActualLocation(ctx, c.cfg.Database, composed.TableName, composed.ExternalLocation)
	if err != nil {
		return nil, nil, fmt.Errorf("athenaio: UnloadAndReadBuckets %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, nil, err
	}

	meta := &gobi.PartitionMetadata{
		Columns:      append([]string(nil), spec.PartitionBy...),
		HashFn:       hashTagFor(composed.Format),
		SortedBy:     append([]gobi.SortKey(nil), spec.OrderBy...),
		SortEnforced: composed.Format == FormatIceberg && len(spec.OrderBy) > 0,
	}

	return &bucketPrep{
		files:       files,
		actualLoc:   actualLoc,
		bucketCount: spec.BucketCount,
		queryID:     queryID,
		exec:        exec,
		start:       start,
	}, meta, nil
}

// RawCTASBuckets is the bucket-aware variant of RawCTAS. Submits the
// caller-composed CTAS SQL (which must already encode bucketing in
// the DDL), verifies via Glue that the resulting table is bucketed
// with `bucket_count > 0`, then returns one LazyFrame per bucket file.
//
// Contract:
//
//   - spec.SQL must contain bucketing DDL. athenaio verifies the
//     Glue table's post-create bucket_count > 0; failure surfaces
//     BEFORE any LazyFrame is constructed. Callers who want to
//     bypass the check should use RawCTAS.
//   - spec.Metadata (if set) is attached to each returned LazyFrame
//     via WithPartitionAssertion — same claim per-bucket, matching
//     the mainline RawCTAS shape.
//   - Otherwise identical semantics to UnloadAndReadBuckets: per-file
//     LazyFrames, nil slots for empty buckets when possible,
//     independent Collect() errors.
func (c *Client) RawCTASBuckets(ctx context.Context, spec RawCTASSpec) ([]BucketResult, error) {
	prep, err := c.prepareRawCTASBuckets(ctx, spec)
	if err != nil {
		return nil, err
	}
	return c.hydrateBuckets(ctx, prep, spec.Metadata, spec.Columns, spec.Predicate)
}

// RawCTASBucketsManifest is the two-phase variant of RawCTASBuckets.
// Phase 1 (this call): submit + poll + verify + list — returns the
// per-bucket manifest (S3URIs + Sizes + common Location) and query
// metadata. Frame is nil on every entry.
// Phase 2 (hydrate closure): construct LazyFrames from the listed
// files on demand. Calling hydrate is optional — the manifest itself
// is durable and can be serialized, handed to another process, and
// fed into BucketResultsFromS3URIs later.
//
// The hydrator is same-process only: it captures a Client reference,
// so it can't cross process boundaries. Cross-process consumers
// should persist the manifest, then reconstruct via
// BucketResultsFromS3URIs on the receiving side.
//
// Cleanup: same as RawCTASBuckets. The Glue table is registered for
// cleanup on Client.Close, so the temp catalog entry gets dropped
// as soon as this Client shuts down — the manifest URIs may outlive
// the Glue entry, which is fine since BucketResultsFromS3URIs goes
// directly to S3 without touching Glue.
//
// Hydrate contract:
//
//   - Idempotent-ish — each call re-runs `populateBucketResults`,
//     returning FRESH LazyFrames. Two calls produce two independent
//     result slices; sibling LazyFrame errors and PartitionMetadata
//     assertions apply per-slice.
//   - Takes its own ctx so a slow S3 read can be bounded
//     independently of the ctx used for the submit/poll phase.
//   - Runs the same code path as RawCTASBuckets — spec.Metadata,
//     spec.Columns, spec.Predicate all applied identically.
func (c *Client) RawCTASBucketsManifest(ctx context.Context, spec RawCTASSpec) (
	manifest []BucketResult,
	meta CTASMetadata,
	hydrate func(context.Context) ([]BucketResult, error),
	err error,
) {
	prep, err := c.prepareRawCTASBuckets(ctx, spec)
	if err != nil {
		return nil, CTASMetadata{}, nil, err
	}
	manifest, err = buildBucketManifest(prep)
	if err != nil {
		return nil, CTASMetadata{}, nil, fmt.Errorf("athenaio: RawCTASBucketsManifest %s: %w", prep.queryID, err)
	}
	meta = CTASMetadata{
		Location: prep.actualLoc,
		QueryID:  prep.queryID,
		Duration: time.Since(prep.start),
	}
	hydrate = func(hctx context.Context) ([]BucketResult, error) {
		return c.hydrateBuckets(hctx, prep, spec.Metadata, spec.Columns, spec.Predicate)
	}
	return manifest, meta, hydrate, nil
}

// bucketPrep captures the CTAS-side artifacts shared between the
// eager and manifest RawCTAS/Unload variants. Populated by prepare*
// helpers; consumed by hydrateBuckets and buildBucketManifest.
type bucketPrep struct {
	files       []bucketFileInfo
	actualLoc   string
	bucketCount int
	queryID     string
	exec        *athenatypes.QueryExecution
	start       time.Time
}

// prepareRawCTASBuckets validates the spec, submits + polls the CTAS,
// registers the table for cleanup, verifies bucketing, resolves the
// actual Glue location, and lists the bucket files. All the work
// RawCTASBuckets and RawCTASBucketsManifest share up through the
// point where they diverge on hydration policy.
func (c *Client) prepareRawCTASBuckets(ctx context.Context, spec RawCTASSpec) (*bucketPrep, error) {
	if spec.SQL == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.SQL is empty")
	}
	if spec.TableName == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.TableName is required")
	}
	if spec.ExternalLocation == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.ExternalLocation is required")
	}
	database := spec.Database
	if database == "" {
		database = c.cfg.Database
	}
	if database == "" {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets requires Database (in spec or Client config)")
	}
	cleanup := spec.Cleanup
	if cleanup == CleanupInherit {
		cleanup = c.cfg.Cleanup
	}
	start := time.Now()

	queryID, err := c.submitTo(ctx, spec.SQL, spec.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets submit:\n---\n%s\n---\n%w", spec.SQL, err)
	}
	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s failed:\n---\n%s\n---\n%w",
			queryID, spec.SQL, err)
	}

	// Register the table for cleanup before the bucketing verification —
	// otherwise an unbucketed table would be orphaned in Glue when we
	// error out below.
	c.registerTable(trackedTable{
		Database:         database,
		Name:             spec.TableName,
		Cleanup:          cleanup,
		Format:           FormatUnknown,
		ExternalLocation: spec.ExternalLocation,
	})

	// Verify the table is actually bucketed. RawCTAS-side we don't
	// compose the SQL so we can't know the bucket count without
	// asking Glue. This check catches "caller forgot the bucketed_by
	// clause" before any LazyFrame is handed back.
	bucketCount, err := c.readGlueBucketCount(ctx, database, spec.TableName)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: verify bucketing: %w", queryID, err)
	}
	if bucketCount <= 0 {
		return nil, fmt.Errorf(
			"athenaio: RawCTASBuckets %s: table %s.%s is not bucketed (bucket_count=%d) — use RawCTAS for non-bucketed output",
			queryID, database, spec.TableName, bucketCount)
	}

	// Resolve to the actual Glue-recorded location before listing —
	// see resolveActualLocation for the workgroup-override rationale.
	actualLoc, err := c.resolveActualLocation(ctx, database, spec.TableName, spec.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: %w", queryID, err)
	}
	// Empty file set is a legitimate outcome — the CTAS succeeded
	// (readGlueBucketCount above confirmed bucket_count > 0) and
	// the caller-provided SELECT produced zero rows. Return a
	// bucketCount-length slice of nil-Frame results so caller code
	// iterating buckets stays uniform between partially-empty and
	// fully-empty results. Matches the UnloadAndReadBuckets shape.
	return &bucketPrep{
		files:       files,
		actualLoc:   actualLoc,
		bucketCount: bucketCount,
		queryID:     queryID,
		exec:        exec,
		start:       start,
	}, nil
}

// hydrateBuckets fills a fresh []BucketResult from the prep + spec-derived
// read options, registers QueryStats on every non-nil frame, and returns
// the result. Shared by the eager RawCTASBuckets/UnloadAndReadBuckets
// paths and by the manifest-hydrator closures. QueryID is the
// disambiguator in error messages — the calling method's name isn't
// needed since the QueryID uniquely identifies the failed CTAS.
func (c *Client) hydrateBuckets(
	ctx context.Context,
	prep *bucketPrep,
	metadata *gobi.PartitionMetadata,
	columns []string,
	predicate gobi.Expr,
) ([]BucketResult, error) {
	results := make([]BucketResult, prep.bucketCount)
	totalRows, err := c.populateBucketResults(ctx, prep.files, results, prep.actualLoc, metadata, readOptsFromSpec(columns, predicate))
	if err != nil {
		return nil, fmt.Errorf("athenaio: hydrateBuckets %s: %w", prep.queryID, err)
	}
	stats := QueryStats{
		QueryExecutionID: prep.queryID,
		ResultPrefix:     prep.actualLoc,
		ScannedBytes:     scannedBytes(prep.exec),
		EngineTime:       engineTime(prep.exec),
		TotalTime:        time.Since(prep.start),
		RowCount:         totalRows,
	}
	for _, r := range results {
		if r.Frame != nil {
			registerStats(r.Frame, stats)
		}
	}
	return results, nil
}

// buildBucketManifest constructs a Frame-nil []BucketResult from the
// prep. Each slot carries S3URI + Size + Location. Slotted via
// bucketSlotFor — same logic populateBucketResults uses on the eager
// path, so the manifest and any subsequent hydrate() call agree on
// which URI lands in which slot.
//
// Surfaces the same slotting errors populateBucketResults would
// (out-of-range slot, duplicate S3URI claim). Silently accepting
// them here would produce a manifest that misrepresents the CTAS
// output — the caller would then see hydrate() error on the same
// underlying data, with no upstream signal about which URI caused it.
func buildBucketManifest(prep *bucketPrep) ([]BucketResult, error) {
	manifest := make([]BucketResult, prep.bucketCount)
	for i := range manifest {
		manifest[i].Location = prep.actualLoc
	}
	for i, fi := range prep.files {
		slot, err := bucketSlotFor(fi.URI, i, prep.bucketCount)
		if err != nil {
			return nil, err
		}
		if manifest[slot].S3URI != "" {
			return nil, fmt.Errorf("athenaio: duplicate bucket slot %d: %s and %s",
				slot, manifest[slot].S3URI, fi.URI)
		}
		manifest[slot].S3URI = fi.URI
		manifest[slot].Size = fi.Size
	}
	return manifest, nil
}

// populateBucketResults reads each file into a Frame, wraps it in a
// LazyFrame, optionally attaches PartitionMetadata, and installs it
// at the appropriate slot in results. Returns the total row count
// across all bucket files (sum of frame.NumRows() at open time —
// derived from the parquet footer, no data-page cost beyond the
// full read already happening for the LazyFrame wrap).
//
// opts propagates to every per-bucket parquetio.ReadReader call —
// column projection + predicate row-group pruning derived from the
// caller's spec.
//
// Slotting: file path suffix `bucket_NNNNN` (or Athena's naming
// variant) is parsed to extract the bucket index; if parsing fails
// (RawCTAS output without a matching name), files fill slots in
// listing order. Missing bucket indices stay nil.
func (c *Client) populateBucketResults(ctx context.Context, files []bucketFileInfo, results []BucketResult, location string, meta *gobi.PartitionMetadata, opts *parquetio.ReadOptions) (int64, error) {
	// Stamp Location on every slot — including nil-Frame ones —
	// so callers can find the common prefix regardless of which
	// slot they inspect. Written before the file-population loop
	// so slots not touched by the loop still carry the value.
	for i := range results {
		results[i].Location = location
	}
	nSlots := len(results)
	var totalRows int64
	for i, fi := range files {
		// populateBucketResults uses fi.Size from ListObjectsV2 —
		// no need for the openBucketFrame size return here.
		frame, _, err := c.openBucketFrame(ctx, fi.URI, opts)
		if err != nil {
			return 0, err
		}
		totalRows += int64(frame.NumRows())
		if meta != nil {
			frame.WithPartitionMeta(meta)
		}
		lf := frame.Lazy()
		if meta != nil {
			asserted, err := lf.WithPartitionAssertion(meta)
			if err != nil {
				return 0, fmt.Errorf("attach partition assertion for %s: %w", fi.URI, err)
			}
			lf = asserted
		}

		slot, err := bucketSlotFor(fi.URI, i, nSlots)
		if err != nil {
			return 0, err
		}
		if results[slot].Frame != nil {
			// Two files claim the same slot — surface rather than
			// silently overwrite. Only fires on Athena writer bugs
			// or naming collisions.
			return 0, fmt.Errorf("athenaio: duplicate bucket slot %d: %s and %s",
				slot, results[slot].S3URI, fi.URI)
		}
		// Preserve Location that was pre-stamped above; set the
		// per-file fields inline instead of reassigning the whole
		// struct.
		results[slot].S3URI = fi.URI
		results[slot].Frame = lf
		results[slot].Size = fi.Size
	}
	return totalRows, nil
}

// bucketSlotFor returns the destination slot for a bucket file URI.
// Prefers the parsed bucket index from bucketIndexFromURI; falls back
// to listing order (`i`) for URIs whose basenames don't match Athena's
// bucket-index naming shapes. Errors when both parsed and listing-
// order indices exceed nSlots — that only happens with a
// bucket-count/file-count mismatch which the caller should surface,
// not silently drop.
//
// Shared by populateBucketResults (eager path, fills LazyFrames) and
// buildBucketManifest (manifest path, fills only S3URI+Size). Keeping
// the slotting logic in one place ensures the manifest and the
// eventual hydrate() call agree on which URI lands in which slot —
// otherwise a bucket-count-mismatch or duplicate-slot condition would
// go undetected until hydrate ran and errored on the same data.
func bucketSlotFor(uri string, i, nSlots int) (int, error) {
	slot := bucketIndexFromURI(uri)
	if slot < 0 || slot >= nSlots {
		slot = i
		if slot >= nSlots {
			return 0, fmt.Errorf("athenaio: file %s exceeds expected bucket range [0,%d)", uri, nSlots)
		}
	}
	return slot, nil
}

// bucketIndexFromURI extracts the bucket index from an Athena-shaped
// output filename. Athena writes bucketed CTAS output with the bucket
// index as a leading zero-padded numeric segment in the basename:
//
//   - Iceberg: `<external_location>/data/NNNNN-<part>-<uuid>.parquet`
//     (leading `NNNNN` = bucket index).
//   - Hive:    `<external_location>/NNNNNN_M.parquet`
//     (leading `NNNNNN` = bucket index, `M` = write-attempt suffix).
//
// Returns -1 when the basename doesn't start with digits.
func bucketIndexFromURI(uri string) int {
	// Take the basename (portion after the last '/').
	base := uri
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// Walk forward, collect leading digits.
	i := 0
	for i < len(base) && base[i] >= '0' && base[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1
	}
	n := 0
	for _, d := range base[:i] {
		n = n*10 + int(d-'0')
	}
	return n
}

// readGlueBucketCount asks Glue for the bucket count on a table
// produced by a Hive-style CTAS. StorageDescriptor.NumberOfBuckets
// carries the count; 0 or absent means the table is not bucketed.
// Used by RawCTASBuckets to catch "caller forgot bucketed_by" before
// handing back LazyFrames.
func (c *Client) readGlueBucketCount(ctx context.Context, database, tableName string) (int, error) {
	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(database),
		Name:         aws.String(tableName),
	})
	if err != nil {
		return 0, fmt.Errorf("GetTable %s.%s: %w", database, tableName, err)
	}
	if out.Table == nil {
		return 0, fmt.Errorf("GetTable %s.%s: nil Table", database, tableName)
	}
	sd := out.Table.StorageDescriptor
	if sd == nil {
		return 0, nil
	}
	return int(sd.NumberOfBuckets), nil
}

// bucketResultsFromS3URIsMaxParallel bounds the concurrent
// HeadObject+Parquet-footer round-trips inside
// BucketResultsFromS3URIs. Sized to avoid overwhelming S3's
// per-connection rate limits on large URI lists while still
// giving hand-off callers meaningful speedup vs a sequential
// walk. Adjust if a benchmark on a real workload shows the
// ceiling is too low or the concurrency triggers throttling.
const bucketResultsFromS3URIsMaxParallel = 32

// BucketResultsFromS3URIs wraps existing S3 parquet URIs — typically
// from a persisted RawCTASBucketsManifest — into a []BucketResult
// ready for downstream LazyFrame consumption. Skips CTAS submit,
// Glue reads, and cleanup registration entirely: these are borrowed
// files that the Client does NOT own.
//
// Per URI:
//   - S3URI populated verbatim from the input.
//   - Size discovered via a single HeadObject (implicit inside
//     openBucketFrame — one HTTP round-trip per URI).
//   - Frame is a LazyFrame reading via S3 GetObject + Range at
//     Collect() time; no local materialization.
//   - Location is the longest common `s3://bucket/prefix/` string
//     shared by every URI, ending at a `/`. Empty when the URIs
//     don't share such a prefix (e.g. mixed buckets).
//
// Concurrency: HeadObject + Parquet footer reads run in parallel
// with a bounded worker pool (see bucketResultsFromS3URIsMaxParallel).
// The output slice preserves input order regardless of completion
// order. Any single-URI failure aborts the whole call — partial
// results are less useful than a hard error for the hand-off use
// case.
//
// Slotting: listing-order, matching the input `uris` slice
// one-for-one. Callers wanting bucket-index slotting (bucket N
// at position N, with nil-Frame gaps for missing indices) should
// pre-sort + pad their URI list before calling.
//
// opts controls Parquet-side column projection and row-group pruning;
// nil means "read every column, no pruning". Mirrors the eager
// path's spec.Columns / spec.Predicate capability so cross-process
// hand-off consumers aren't strictly worse off than same-process
// hydrate() callers.
//
// Cleanup: does NOT register the source files or any Glue table for
// cleanup on Client.Close — these are borrowed files, deletion is
// the caller's responsibility. No QueryStats attached to the
// returned LazyFrames since no query was submitted; callers who
// want stats should persist them alongside the manifest from the
// original RawCTASBucketsManifest / RawCTASWithMetadata call.
func (c *Client) BucketResultsFromS3URIs(ctx context.Context, uris []string, opts *parquetio.ReadOptions) ([]BucketResult, error) {
	if len(uris) == 0 {
		return nil, nil
	}
	location := longestCommonS3Prefix(uris)
	results := make([]BucketResult, len(uris))

	// Bounded-parallel HeadObject + footer read. Each worker writes
	// to its own slot in `results` (distinct index per URI), so no
	// cross-worker synchronization is needed on the output. First
	// error wins via errgroup's ctx cancellation.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(bucketResultsFromS3URIsMaxParallel)
	for i, uri := range uris {
		i, uri := i, uri
		g.Go(func() error {
			frame, size, err := c.openBucketFrame(gctx, uri, opts)
			if err != nil {
				return fmt.Errorf("athenaio: BucketResultsFromS3URIs %s: %w", uri, err)
			}
			results[i] = BucketResult{
				S3URI:    uri,
				Frame:    frame.Lazy(),
				Size:     size,
				Location: location,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// Release any frames that landed before the error. LazyFrame
		// doesn't expose Release directly; the underlying Frame's ref
		// is held on the LazyFrame — dropping the slice is enough for
		// GC to reclaim.
		return nil, err
	}
	return results, nil
}

// longestCommonS3Prefix returns the longest `s3://bucket/prefix/`
// shared by every URI in uris, trimmed at the last `/` boundary.
// Returns "" when the URIs share no `/`-anchored prefix (e.g. mixed
// buckets, or a trivial common prefix like `s3://`).
//
// Comparison is byte-wise but each pair-wise step trims the running
// prefix to its last '/' inside the loop — that way `s3://bucket-a/x`
// vs `s3://bucket-aa/x` reduces to `s3://` early (and gets rejected
// as degenerate at the return check) rather than accidentally
// producing a mid-name substring that the final trim has to clean up.
func longestCommonS3Prefix(uris []string) string {
	if len(uris) == 0 {
		return ""
	}
	prefix := uris[0]
	for _, u := range uris[1:] {
		n := len(prefix)
		if len(u) < n {
			n = len(u)
		}
		i := 0
		for ; i < n; i++ {
			if prefix[i] != u[i] {
				break
			}
		}
		// Trim to the last '/' at or before the divergence point so
		// the running prefix always ends on a segment boundary.
		// Prevents `s3://bucket-a/x` vs `s3://bucket-aa/x` from
		// carrying `s3://bucket-a` (which trims to `s3://` at the
		// end) between iterations — with the segment trim, the
		// running prefix is `s3://` after the first comparison, and
		// subsequent iterations short-circuit on the empty check.
		cut := strings.LastIndex(prefix[:i], "/")
		if cut < 0 {
			return ""
		}
		prefix = prefix[:cut+1]
		if prefix == "" {
			return ""
		}
	}
	// Trim to last '/' — no-op after the loop's per-step trim on
	// multi-URI inputs, but handles the single-URI case where the
	// loop body didn't run.
	idx := strings.LastIndex(prefix, "/")
	if idx < 0 {
		return ""
	}
	prefix = prefix[:idx+1]
	// Guard against degenerate prefixes: `s3://` alone means the
	// URIs only share the scheme, which is not a useful "common
	// location". `s3://bucket/` with no key prefix is still valid
	// (all objects at bucket root), so allow that.
	if prefix == "s3://" || !strings.HasPrefix(prefix, "s3://") {
		return ""
	}
	return prefix
}

package athenaio

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"

	"github.com/zoobst/gobi"
)

// OpenPartitionedTable reads an existing Glue-cataloged table + its
// bucketing spec + returns a partition-aware LazyFrame. Distinguishes
// from UnloadAndRead in that athenaio didn't write the table — the
// user's ETL / Airflow / dbt / whatever already populated it.
// athenaio just introspects the catalog to build a PartitionMetadata
// claim, then streams the parquet files under external_location the
// same way UnloadAndRead does.
//
// Auto-detection covers the Hive-shape case exhaustively via Glue's
// native StorageDescriptor fields (`NumberOfBuckets`, `BucketColumns`,
// `SortColumns`). Iceberg tables need an explicit opts.Metadata
// override — Iceberg's partition spec lives in its own JSON metadata
// files under external_location rather than in the Hive-shaped Glue
// fields, and athenaio's step 6 didn't bring in an Iceberg spec
// parser. Users who want Iceberg reads today supply the metadata
// claim; a future slice can auto-detect once the Iceberg JSON parser
// lands.
//
// The returned LazyFrame is NOT tracked for cleanup — the table was
// created outside athenaio, so `Client.Close` won't touch it. Users
// who created the table via `UnloadAndRead` on this same Client
// already have it in the cleanup registry; opening it again through
// this API doesn't add a second entry.
//
// A nil opts.Metadata (or nil opts entirely) triggers auto-detection.
// A non-nil opts.Metadata skips detection and uses the user claim
// directly (user owns correctness — same contract as
// LazyFrame.WithPartitionAssertion).
func (c *Client) OpenPartitionedTable(ctx context.Context, database, tableName string, opts *OpenOptions) (*gobi.LazyFrame, error) {
	if database == "" {
		return nil, fmt.Errorf("athenaio: OpenPartitionedTable: database is required")
	}
	if tableName == "" {
		return nil, fmt.Errorf("athenaio: OpenPartitionedTable: tableName is required")
	}
	start := time.Now()

	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(database),
		Name:         aws.String(tableName),
	})
	if err != nil {
		return nil, fmt.Errorf("athenaio: GetTable %s.%s: %w", database, tableName, err)
	}
	if out.Table == nil {
		return nil, fmt.Errorf("athenaio: GetTable %s.%s: nil Table", database, tableName)
	}
	t := out.Table

	// Location: required for any read.
	if t.StorageDescriptor == nil || t.StorageDescriptor.Location == nil || *t.StorageDescriptor.Location == "" {
		return nil, fmt.Errorf("athenaio: %s.%s: Glue table has no StorageDescriptor.Location — can't read", database, tableName)
	}
	externalLoc := *t.StorageDescriptor.Location

	// Metadata source: user override wins; else auto-detect from
	// Glue's Hive-shaped fields.
	var meta *gobi.PartitionMetadata
	if opts != nil && opts.Metadata != nil {
		meta = opts.Metadata
	} else {
		if t.Parameters["table_type"] == "ICEBERG" {
			return nil, fmt.Errorf("athenaio: %s.%s is Iceberg format — auto-detect not implemented; supply OpenOptions.Metadata (see contrib/athenaio/DESIGN.md's deferred Iceberg partition-spec parsing)", database, tableName)
		}
		meta, err = deriveHivePartitionMetadata(t)
		if err != nil {
			return nil, fmt.Errorf("athenaio: %s.%s: %w", database, tableName, err)
		}
	}

	files, err := listBucketFiles(ctx, c.s3, externalLoc)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("athenaio: %s.%s: no result files under %s",
			database, tableName, externalLoc)
	}
	frame, err := c.readBucketFiles(ctx, files)
	if err != nil {
		return nil, fmt.Errorf("athenaio: %s.%s: %w", database, tableName, err)
	}

	frame.WithPartitionMeta(meta)
	lf := frame.Lazy()
	asserted, err := lf.WithPartitionAssertion(meta)
	if err != nil {
		return nil, fmt.Errorf("athenaio: attach partition assertion: %w", err)
	}

	// Register QueryStats — no QueryExecutionID or ScannedBytes
	// (no query ran), but ResultPrefix + TotalTime are still useful
	// for observability + debugging.
	registerStats(asserted, QueryStats{
		QueryExecutionID: "",
		ResultPrefix:     externalLoc,
		TotalTime:        time.Since(start),
	})
	return asserted, nil
}

// deriveHivePartitionMetadata builds a PartitionMetadata claim from
// Glue's Hive-shaped StorageDescriptor fields. Returns an error if
// the table isn't actually bucketed (users pointing at a non-
// partitioned table should use plain parquetio.ReadFile via S3 or
// wait for a future non-partitioned entry point).
//
// Fields consumed:
//   - StorageDescriptor.BucketColumns  → PartitionMetadata.Columns
//   - StorageDescriptor.NumberOfBuckets: sanity-checked >0
//   - StorageDescriptor.SortColumns     → PartitionMetadata.SortedBy
//
// SortEnforced is set to false regardless. Hive's sorted_by is a
// table-property hint that the writer may or may not have honored;
// gobi's alignment rule refuses to trust it for correctness-sensitive
// operators (Diff / Shift with fill-forward). If the user knows the
// table was written with matching ORDER BY, they should supply an
// explicit opts.Metadata claim with SortEnforced=true.
func deriveHivePartitionMetadata(t *gluetypes.Table) (*gobi.PartitionMetadata, error) {
	sd := t.StorageDescriptor
	if sd == nil {
		return nil, fmt.Errorf("athenaio: Glue table has no StorageDescriptor")
	}
	if len(sd.BucketColumns) == 0 {
		return nil, fmt.Errorf("athenaio: Glue table has no BucketColumns — table isn't hash-bucketed; auto-detect requires bucketed_by")
	}
	if sd.NumberOfBuckets <= 0 {
		return nil, fmt.Errorf("athenaio: Glue table has NumberOfBuckets=%d — expected > 0", sd.NumberOfBuckets)
	}

	sortedBy := make([]gobi.SortKey, 0, len(sd.SortColumns))
	for _, sc := range sd.SortColumns {
		if sc.Column == nil || *sc.Column == "" {
			continue
		}
		// Hive's SortColumns.SortOrder: 1=ASC, 0=DESC. Treat any
		// non-1 value as descending — Glue is inconsistent about
		// which value it uses in practice.
		sortedBy = append(sortedBy, gobi.SortKey{
			Column:     *sc.Column,
			Descending: sc.SortOrder != 1,
		})
	}

	return &gobi.PartitionMetadata{
		Columns:      append([]string(nil), sd.BucketColumns...),
		HashFn:       hiveHashTag,
		SortedBy:     sortedBy,
		SortEnforced: false, // Hive's sorted_by is a hint only — see doc above
	}, nil
}

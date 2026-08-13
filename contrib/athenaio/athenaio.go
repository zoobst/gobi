// Package athenaio provides a gobi LazyFrame source backed by AWS
// Athena. This is the T1 slice of the design in
// contrib/athenaio/DESIGN.md — the lifecycle wrapper that submits a
// query, polls for completion, downloads result Parquet files from S3
// via parquetio, and returns them as a LazyFrame with no partition
// claim.
//
// T3 (partition-aware UnloadAndRead via CTAS with hash bucketing +
// PartitionMetadata population) lands separately. RawQuery is the
// escape hatch for T3-shaped workflows and the entire T1 surface —
// keep it minimal here so T3 can compose without ripping T1 apart.
//
// Testing: unit tests mock the aws-sdk-go-v2 client interfaces
// (AthenaAPI, S3API in this package). Real-Athena tests live behind
// //go:build integration and require ATHENAIO_TEST_WORKGROUP +
// ATHENAIO_TEST_RESULT_LOCATION environment variables. No localstack
// equivalent for Athena's query engine — CI coverage is mock-only.
package athenaio

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zoobst/gobi"
)

// ErrQueryFailed is returned when Athena reports the query in a
// terminal-non-success state (FAILED / CANCELLED). Wraps the
// state-change reason from GetQueryExecution.
var ErrQueryFailed = errors.New("athenaio: query failed")

// ErrQueryTimeout is returned when the poll loop exceeds
// ClientConfig.MaxPollDuration without seeing a terminal state.
var ErrQueryTimeout = errors.New("athenaio: query polling timed out")

// AthenaAPI is the subset of the aws-sdk-go-v2 Athena client that
// athenaio uses. Defined here (rather than depending on the SDK's
// concrete client) so unit tests can supply mocks without touching
// AWS. The real *athena.Client from the SDK satisfies this interface.
//
// GetQueryResults is used only by the LIMIT-0 prepass path
// (UnloadSpec.ValidatePartitionCols) — the main read path streams
// result parquet from S3 via S3API rather than paginating through
// Athena's rows API.
type AthenaAPI interface {
	StartQueryExecution(ctx context.Context, in *athena.StartQueryExecutionInput, opts ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error)
	GetQueryExecution(ctx context.Context, in *athena.GetQueryExecutionInput, opts ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error)
	GetQueryResults(ctx context.Context, in *athena.GetQueryResultsInput, opts ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error)
}

// S3API is the subset of the aws-sdk-go-v2 S3 client that athenaio
// uses. Real *s3.Client satisfies this. GetObject is called with
// Range headers to stream individual byte ranges of result Parquet
// files (backing parquetio.ReadReader's io.ReaderAt).
//
// HeadObject is used to discover result file sizes before opening
// the reader — Parquet needs the total size at read start to seek
// the footer.
//
// ListObjectsV2 is used to enumerate result bucket files under a
// CTAS's external_location prefix. A T3 UnloadAndRead writes N files
// (one per hash bucket); athenaio lists them, opens an s3ReaderAt
// per file, concatenates the resulting Frames.
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// GlueAPI is the subset of the aws-sdk-go-v2 Glue Data Catalog
// client that athenaio uses. Real *glue.Client satisfies this. Used
// for two things:
//
//   - GetTable: read-back verification after CTAS. Confirms the
//     actual bucketing config Athena wrote matches what UnloadSpec
//     requested; a mismatch produces an error rather than a silently-
//     narrowed PartitionMetadata claim.
//   - DeleteTable: catalog cleanup on Client.Close. Drops entries
//     for tables athenaio created (namespaced gobi_athenaio_<hex>_<epoch>).
//     External-table drops are metadata-only; the underlying S3 data
//     files persist for the user's bucket lifecycle policy.
type GlueAPI interface {
	GetTable(ctx context.Context, in *glue.GetTableInput, opts ...func(*glue.Options)) (*glue.GetTableOutput, error)
	DeleteTable(ctx context.Context, in *glue.DeleteTableInput, opts ...func(*glue.Options)) (*glue.DeleteTableOutput, error)
}

// ClientConfig configures a Client. All fields except Workgroup and
// ResultLocation are optional; zero-values fall through to sensible
// defaults.
type ClientConfig struct {
	// AWSConfig is the aws-sdk-go-v2 config used to build Athena +
	// S3 clients. Typically produced via config.LoadDefaultConfig.
	// Ignored when Athena / S3 are provided directly (test-injection
	// path).
	AWSConfig aws.Config

	// Athena / S3 / Glue override the SDK clients built from
	// AWSConfig. Real callers leave all three nil and let NewClient
	// construct them from AWSConfig; tests inject mocks satisfying
	// the AthenaAPI / S3API / GlueAPI interfaces.
	Athena AthenaAPI
	S3     S3API
	Glue   GlueAPI

	// Workgroup is the Athena workgroup queries run against. Required.
	// Determines engine version (v2 vs v3, which gates Iceberg CTAS),
	// result-location defaults, and query billing.
	Workgroup string

	// ResultLocation is the S3 URI prefix (s3://bucket/prefix/) where
	// Athena writes query result files. Required. Must be readable
	// by the credentials in AWSConfig. The workgroup config may
	// override this — check the Athena console if results land
	// elsewhere than expected.
	ResultLocation string

	// Database is the default database context for user SQL. Also the
	// Glue database where CTAS creates its tables. Optional for T1
	// (RawQuery) — empty falls back to the workgroup default. Required
	// for T3 (UnloadAndRead) because Glue table lookups need a
	// database name.
	Database string

	// ClientID is a stable identifier for the Client instance. Used
	// as a Glue tag on every created table so out-of-band sweeps can
	// attribute orphans. Zero value falls back to a UUID generated
	// in NewClient.
	ClientID string

	// Cleanup selects the default disposal strategy for tables
	// created by UnloadAndRead. Individual UnloadSpec.Cleanup values
	// override this on a per-call basis. Zero value = CleanupCatalogOnly.
	Cleanup Cleanup

	// PollInterval is the sleep between GetQueryExecution polls.
	// Default 500ms with a cap at 5s via internal exponential backoff.
	PollInterval time.Duration

	// MaxPollDuration is the wall-clock ceiling before RawQuery
	// returns ErrQueryTimeout. Zero-value falls through to the
	// default (5 minutes) — matches the PollInterval pattern.
	// Callers who need unbounded polling should either set this to a
	// large value or rely on ctx cancellation for the ceiling.
	MaxPollDuration time.Duration

	// WarnLog is invoked when UnloadAndRead falls back from Iceberg
	// to Hive because the workgroup doesn't support Iceberg (Athena
	// engine v2). Signature matches log.Printf. Nil = silent.
	// Production callers typically set log.Printf; tests inject a
	// recorder to assert the fallback fired.
	WarnLog func(format string, args ...any)
}

// TableFormat selects the physical table shape CTAS writes. Iceberg
// is preferred where the workgroup supports it (Athena engine v3);
// Hive is the fallback for engine-v2 workgroups. See
// contrib/athenaio/DESIGN.md's "Table format: Iceberg default, Hive
// fallback" section for the tradeoffs (Iceberg: uniform Murmur3-32
// hash, enforced sorted_by, no bucket count ceiling; Hive: non-
// uniform Java hashCode-based hash, hint-only sorted_by, ~100-bucket
// ceiling).
//
// Zero-value FormatUnknown falls back to FormatIceberg in
// UnloadAndRead. Callers that want to force Hive set FormatHive
// explicitly.
type TableFormat int

const (
	FormatUnknown TableFormat = iota
	FormatIceberg
	FormatHive
)

func (f TableFormat) String() string {
	switch f {
	case FormatIceberg:
		return "iceberg"
	case FormatHive:
		return "hive"
	}
	return "unknown"
}

// Cleanup selects the disposal strategy for CTAS-created objects
// when Client.Close is invoked. Files (S3 objects under
// external_location) and Glue table entries are two distinct
// domains — CleanupCatalogOnly (the Client default) keeps the
// "user owns S3 files, athenaio owns catalog" separation clean;
// CleanupAll adds S3 DeleteObjects on top for callers whose bucket
// has no lifecycle policy; CleanupNone leaves both for debugging.
//
// The zero-value CleanupInherit only makes sense on UnloadSpec —
// it means "use the Client's default." NewClient normalizes the
// Client-level zero-value to CleanupCatalogOnly so
// effectiveCleanup always resolves to a concrete strategy.
type Cleanup int

const (
	// CleanupInherit is the zero-value sentinel: on UnloadSpec, means
	// "inherit from the Client's Cleanup setting." NewClient rejects
	// this value at the Client level (normalized to CleanupCatalogOnly).
	CleanupInherit Cleanup = iota

	// CleanupCatalogOnly (Client default): DROP TABLE on Close. S3
	// result files stay for the user's bucket lifecycle policy to
	// reap.
	CleanupCatalogOnly

	// CleanupAll: DROP TABLE plus explicit S3 DeleteObjects on the
	// result prefix. Ergonomic for Lambda-shaped workloads where
	// files have no reuse value. Not implemented in step 6a — treated
	// as CleanupCatalogOnly until 6b lands.
	CleanupAll

	// CleanupNone: leave both catalog and files intact. For debugging
	// weird correctness issues where inspecting the raw CTAS output
	// after Close matters.
	CleanupNone
)

// OpenOptions configures Client.OpenPartitionedTable. All fields
// optional; nil opts means "auto-detect everything from Glue."
type OpenOptions struct {
	// Metadata overrides the auto-detected PartitionMetadata. When
	// set, athenaio skips Glue-based inference and attaches this
	// claim directly to the returned LazyFrame. User owns
	// correctness — symmetric with gobi's LazyFrame.WithPartitionAssertion.
	//
	// Required for Iceberg tables in the current implementation:
	// Glue's Hive-shaped GetTable doesn't expose Iceberg's partition
	// spec natively (it lives in Iceberg's own JSON metadata files
	// under external_location, which athenaio doesn't parse). Users
	// pointing at an Iceberg table must supply this override — the
	// auto-detect path errors out with a hint.
	Metadata *gobi.PartitionMetadata

	// Columns projects each bucket parquet to a subset of top-level
	// columns at read time — passed through to parquetio.ReadReader
	// as ReadOptions.Columns. See RawCTASSpec.Columns for semantics.
	Columns []string

	// Predicate is a row-group pruning hint applied to every bucket
	// parquet at read time — passed through to parquetio.ReadReader
	// as ReadOptions.Predicate. See RawCTASSpec.Predicate for the
	// pruning-vs-filtering distinction.
	Predicate gobi.Expr
}

// RawCTASSpec configures a Client.RawCTAS call — the escape hatch
// for callers who need to compose their own CTAS SQL (custom hash
// functions, Iceberg-specific properties athenaio doesn't expose,
// composite partition transforms, etc.). User owns correctness of
// the Metadata claim; athenaio never verifies the assertion against
// actual data.
//
// The Client still tracks the created table for cleanup on Close.
// TableName + ExternalLocation must match what SQL creates; athenaio
// does not parse SQL to derive them.
type RawCTASSpec struct {
	// SQL is the full CREATE TABLE ... AS statement to submit. No
	// wrapping — athenaio hands this to StartQueryExecution verbatim.
	SQL string

	// Database is the Glue database where the CTAS lands. Empty
	// falls back to Client.Database.
	Database string

	// TableName must match the CREATE TABLE ... AS in SQL. Used for
	// read-back GetTable + eventual DeleteTable on Close.
	TableName string

	// ExternalLocation is the s3://... URI where the CTAS writes
	// its data files. athenaio walks this via ListObjectsV2 to find
	// the parquet output.
	ExternalLocation string

	// Metadata is the PartitionMetadata claim to attach to the
	// returned LazyFrame. User owns correctness. A nil Metadata is
	// legal — the returned LazyFrame carries no partition claim
	// (equivalent to RawQuery for T3-shaped code paths).
	Metadata *gobi.PartitionMetadata

	// Cleanup overrides the Client-level Cleanup for this call.
	// Zero-value CleanupInherit means "use Client default."
	Cleanup Cleanup

	// Columns projects each bucket parquet to a subset of top-level
	// columns at read time — passed through to parquetio.ReadReader
	// as ReadOptions.Columns. nil or empty reads every column.
	//
	// Named columns must be present in the CTAS output schema.
	// Skipping columns avoids fetching, decompressing, and
	// materializing them into arrow arrays — meaningful on wide
	// bucket parquets where downstream code only touches a handful.
	Columns []string

	// Predicate is a row-group pruning hint applied to every bucket
	// parquet at read time — passed through to parquetio.ReadReader
	// as ReadOptions.Predicate. Same semantics as
	// parquetio.ReadOptions.Predicate: row groups whose footer
	// stats prove no row could satisfy the predicate are skipped
	// wholesale; rows within surviving groups are NOT filtered.
	//
	// If callers want row-level filtering, they should also apply
	// `.Filter(pred)` on the returned per-bucket LazyFrame — the
	// Predicate here is a fast-path hint that avoids downloading +
	// decoding irrelevant row groups from S3.
	Predicate gobi.Expr
}

// UnloadSpec configures a single UnloadAndRead call: the user's
// SELECT body plus how athenaio should partition + bucket the CTAS
// output. Only PartitionBy + BucketCount are required for a T3
// (partition-aware) write; leaving them zero produces a T1-shaped
// unpartitioned CTAS instead — but users wanting T1 should just
// call Client.RawQuery.
type UnloadSpec struct {
	// SQL is the user-provided SELECT body. No CREATE TABLE, no CTAS
	// keywords, no outer ORDER BY — athenaio composes the outer
	// query. Rejecting a user-level top ORDER BY is a step-6b
	// concern; step 6a trusts callers to follow the contract.
	SQL string

	// PartitionBy names the hash-bucketing partition columns in
	// order (hash(a, b) ≠ hash(b, a) in Trino/Iceberg, so order
	// matters). Empty falls back to unpartitioned CTAS — same
	// alignment guarantee as RawQuery (none).
	PartitionBy []string

	// BucketCount is the number of hash buckets. Required when
	// PartitionBy is non-empty. Iceberg has no practical ceiling;
	// Hive caps around 100.
	BucketCount int

	// OrderBy names within-partition sort keys. On Iceberg this
	// becomes a first-class sorted_by table property (enforced by
	// the writer). On Hive it's a hint only — step 6b will add the
	// ORDER BY in the composed SELECT so at least the physical
	// write is sorted.
	OrderBy []gobi.SortKey

	// TableFormat picks Iceberg (default) vs Hive. Step 6a supports
	// Iceberg only — non-Iceberg specs error out. Hive fallback lands
	// in 6b.
	TableFormat TableFormat

	// Cleanup overrides the Client-level Cleanup setting for this
	// call. Zero-value means "inherit from Client config" — the
	// intended common case.
	Cleanup Cleanup

	// ValidatePartitionCols runs a LIMIT-0 prepass against the
	// user's SELECT body before submitting the CTAS. Confirms every
	// PartitionBy column is present in the projection; fail-fast
	// with a clear error naming the missing column. Adds one Athena
	// query per UnloadAndRead call (extra billing + latency), so
	// opt-in only — the composed CTAS surfaces the same error
	// eventually via its own failure state, just wrapped in nested
	// SQL that's harder to read.
	ValidatePartitionCols bool

	// Columns projects each bucket parquet to a subset of top-level
	// columns at read time — passed through to parquetio.ReadReader
	// as ReadOptions.Columns. See RawCTASSpec.Columns for the
	// column-projection semantics; identical here.
	Columns []string

	// Predicate is a row-group pruning hint applied to every bucket
	// parquet at read time — passed through to parquetio.ReadReader
	// as ReadOptions.Predicate. See RawCTASSpec.Predicate for the
	// pruning-vs-filtering distinction; identical here.
	Predicate gobi.Expr
}

// QueryStats reports Athena's per-query metadata surfaced back to
// callers alongside the returned LazyFrame. Retrieved via
// StatsFor(lf) after a RawQuery / UnloadAndRead call.
type QueryStats struct {
	// QueryExecutionID is Athena's internal query ID. Correlates
	// with CloudTrail entries + the Athena console history view for
	// incident forensics.
	QueryExecutionID string

	// ResultPrefix is the s3://bucket/prefix/ where result files
	// landed. Useful for `aws s3 ls` when a query produced
	// unexpected output.
	ResultPrefix string

	// ScannedBytes is Athena's DataScannedInBytes — the number of
	// input bytes Athena billed for. Users should log this if cost
	// visibility matters.
	ScannedBytes int64

	// EngineTime is query execution time on Athena's side (not
	// including queue wait).
	EngineTime time.Duration

	// TotalTime includes queue wait.
	TotalTime time.Duration

	// RowCount is the total number of rows produced by the query.
	// Populated on the CTAS paths (UnloadAndRead / UnloadAndReadBuckets
	// / RawCTAS / RawCTASBuckets) by summing per-file row counts from
	// the parquet footers read at open time — zero-data-page cost.
	// Left at zero on RawQuery (Athena's GetQueryExecution doesn't
	// expose output row count; callers who need it after RawQuery
	// should count the returned Frame post-Collect).
	//
	// For the bucket variants, the value is the CTAS's total across
	// every bucket — the same value is registered against every
	// non-nil bucket's LazyFrame. Per-bucket counts are recoverable
	// via Frame.NumRows() after Collect on that bucket's LazyFrame.
	RowCount int64
}

// queryStateTerminal reports whether an Athena query state is a
// terminal state that stops polling. SUCCEEDED = normal completion;
// FAILED / CANCELLED = terminal errors that RawQuery surfaces as
// ErrQueryFailed.
func queryStateTerminal(s athenatypes.QueryExecutionState) (done bool, ok bool) {
	switch s {
	case athenatypes.QueryExecutionStateSucceeded:
		return true, true
	case athenatypes.QueryExecutionStateFailed, athenatypes.QueryExecutionStateCancelled:
		return true, false
	}
	return false, false
}

# athenaio Design

**Status:** design in progress, unreleased. Lives under `contrib/`
(gitignored) until the shape is validated and dependencies are
committed. See `PARTITION-METADATA.md` in this directory for the gobi
core work this depends on.

## What it is

A gobi subpackage providing a `LazyFrame`-shaped read path over AWS
Athena. Sibling to `pgio` (SQL-first read path with `ExprToSQL`
pushdown) and `parquetio` (since Athena results are Parquet in S3,
athenaio chains them internally). Distinguishing feature vs. a
"just run the query and download" wrapper: the returned `LazyFrame`
carries **`PartitionMetadata`** so gobi's optimizer can prove
alignment on `.Over(K)` / partition-wise `Join` / repartition-skip
`GroupBy` and eliminate cross-worker shuffle.

## Package location

`contrib/athenaio/` with its own `go.mod`. Rationale:

- Isolates the `aws-sdk-go-v2` dep weight from consumers who don't
  need Athena.
- Iterates independently of gobi core's semver.
- Sets the precedent for future `bigqueryio`, `snowflakeio`,
  `redshiftio` — all vendor packages land under `contrib/` with their
  own module boundary.
- Sibling repo was considered; discoverability hit and publication
  overhead aren't worth the small benefit over an in-repo submodule.

## Tiered scope

Three tiers, ascending in value:

- **T1 — lifecycle wrapper.** `StartQueryExecution` → poll →
  `GetQueryResults` (or read the result manifest + Parquet files
  directly from S3 via parquetio). Saves boilerplate. Not
  architecturally interesting; any Go project doing Athena has this.
- **T2 — streaming result download.** Overlap S3 GET with row-group
  processing as Athena writes result files. *Collapses into T3* —
  partitioned CTAS writes N files, workers stream their share via
  parquetio's parallel scan, and the overlap falls out. Not a
  separate deliverable.
- **T3 — partition-aware `LazyFrame`.** The anchor. Write path uses
  CTAS with hash bucketing; returned `LazyFrame` carries
  `PartitionMetadata` matching the bucket spec; gobi optimizer knows
  same-K rows never cross a worker boundary. Enables shuffle-free
  `.Over(K)`, partition-wise `Join(K)`, and repartition-skip
  `GroupBy(K)`.

**Sequencing.** T1 first (unblocks any Athena workflow immediately),
T3 second (once gobi core has `PartitionMetadata` infrastructure —
see `PARTITION-METADATA.md`).

## Write path: CTAS, not UNLOAD

UNLOAD's `partitioned_by` is Hive-style directory-per-unique-value
— fine for low-cardinality keys (region, country), catastrophic for
high-cardinality keys (user_id → millions of directories → S3
listing hell). CTAS's `bucketed_by=ARRAY[K], bucket_count=N`
provides bounded hash bucketing, which is what T3's alignment proof
was designed around.

### Table format: Iceberg default, Hive fallback

Athena CTAS emits different physical outputs by table format:

| Property                 | Hive (default)                     | Iceberg (engine v3)             |
|--------------------------|------------------------------------|---------------------------------|
| Hash function            | Java `hashCode`-based; non-uniform on structured varchars | Murmur3-32; spec-defined, uniform |
| `sorted_by` semantics    | Hint only; needs explicit `ORDER BY` in SELECT for physical sort | First-class table property, enforced by writer |
| Bucket count ceiling     | ~100 historically                  | No practical ceiling            |
| Metadata files on drop   | None beyond Glue                   | Iceberg manifests under `external_location` |

For T3 correctness, prefer Iceberg:

- Uniform hash → alignment proof less likely to encounter pathological
  skew from structured-key hash collisions.
- Enforced `sorted_by` → "one CTAS write gets both alignment and
  sortedness for free" property holds without the `ORDER BY` trick.
- No bucket-count ceiling → parallel scan can scale wide without
  hitting Hive's limit.

**Detection:** Iceberg support depends on Athena engine v3 *and*
workgroup config, which isn't a single flag to probe. First cut:
attempt-Iceberg, fall back on specific error with a warn log naming
the downgrade. Verify capability from `GetWorkGroup` if the probe
proves fragile in practice.

**Iceberg cleanup nuance:** Iceberg leaves manifest / snapshot files
under `external_location` after `DROP TABLE`. Either explicitly
purge them at drop time or document them as user-owned like the
parquet files. Purge is cleaner and worth the extra call — the
"drop is metadata-only, user's bucket lifecycle reaps files"
contract stays clean for the primary parquet output; Iceberg-internal
metadata files don't belong under that contract.

### SQL composition

athenaio owns the outer query. User provides the SELECT body via a
string; athenaio wraps it as:

```sql
CREATE TABLE <name> WITH (
  format = 'PARQUET',
  external_location = 's3://<bucket>/<result-location>/gobi-athenaio/<uuid>/',
  bucketed_by = ARRAY['<K>'],
  bucket_count = <N>,
  sorted_by = ARRAY['<sort_col>']   -- optional, for Iceberg only
) AS
<user-provided SELECT body>
```

**Contract:**

1. **Reject user-level `ORDER BY` at the top of the SELECT.** Silent
   drop inside a CTAS subquery is a footgun. Return an error:
   `"athenaio owns the top-level ORDER BY; use UnloadSpec.OrderBy"`.
   Detection is a lightweight tokenizer scan, not a full SQL parser.
2. **Validate partition key exists in the user's SELECT projection.**
   Run a `LIMIT 0` prepass to pull the projection schema before
   composing the CTAS. Fail fast with a clear error naming the
   missing column. Cache the prepass result keyed on (sql body,
   workgroup, database) so a re-Collect of the same LazyFrame
   doesn't re-validate.
3. **Namespace the internal table name.** `gobi_athenaio_<8-hex>_<unix-epoch>`
   — greppable, time-sortable, unlikely to collide with user tables.
4. **Log the composed SQL on any Athena `FAILED` state.** Nesting
   makes error messages confusing; logging both the user body and
   the composed outer query is cheap and huge for debugging.

### Read-back verification

After the CTAS returns, call `GetTable` on the Glue catalog to
confirm Athena actually wrote what was requested. Compare:

- Table format (Iceberg vs. Hive) matches what was requested / probed.
- Bucketing columns match (ordered).
- Bucket count matches (or is at least ≥ the request for Hive, since
  Hive silently caps).
- `sorted_by` present (if requested).

**On mismatch:** return an error and refuse to hand back the
`LazyFrame`. Do **not** silently narrow `PartitionMetadata` to match
what actually got written — silent narrowing causes correctness
degradation months later that's nearly impossible to diagnose
("my `Over(K)` suddenly went shuffle-full"). Hard error at write
time lets the user retry with a looser spec or fix their workgroup
config. One extra `GetTable` call per CTAS is negligible cost.

## `PartitionMetadata` emitted by athenaio

athenaio populates the metadata based on the confirmed catalog spec
(post-verification):

```go
gobi.PartitionMetadata{
    Columns:      []string{"K"},
    HashFn:       "athenaio/iceberg/murmur3-32/v1", // or "athenaio/hive/bucket/v1"
    SortedBy:     []gobi.SortKey{{Column: "ts", Descending: false}},
    SortEnforced: true, // Iceberg: true; Hive: false unless verified via ORDER BY
}
```

**Hash function tags athenaio owns:**

- `"athenaio/iceberg/murmur3-32/v1"` — Iceberg CTAS, Murmur3-32 hash.
- `"athenaio/hive/bucket/v1"` — Hive CTAS, Java hashCode-based hash.

These are distinct from gobi runtime tags (`"gobi/xxhash64/v1"`) and
other-vendor tags (`"pgio/hash/v1"`, etc.). Alignment proof rejects
cross-source claims by design — cross-source shuffle-aligned join is
a v2 concern that requires gobi's runtime to compute
source-compatible hashes on demand.

## Cleanup model

Files are the user's responsibility (bucket lifecycle policy).
Glue catalog objects are athenaio's responsibility.

```go
type Cleanup int

const (
    // Default: DROP TABLE on Client.Close(), files remain for
    // user's bucket lifecycle policy to reap.
    CleanupCatalogOnly Cleanup = iota

    // DROP TABLE + explicit S3 DeleteObjects on the result prefix.
    // Ergonomic for Lambda-style workloads where files have no
    // reuse value and no bucket lifecycle policy is configured.
    // Note: expensive on large result sets (many DeleteObjects
    // requests + LIST scan). Document the cost.
    CleanupAll

    // Leave both catalog + files intact. For debugging weird
    // correctness issues where inspecting the raw CTAS output
    // matters. Tables are still tracked on the Client for
    // observability ("N uncleaned tables from CleanupNone spec").
    CleanupNone
)
```

**Iceberg cleanup:** for CleanupCatalogOnly with Iceberg format,
issue `DROP TABLE ... PURGE` (or the equivalent Iceberg spec call)
so Iceberg's manifest / snapshot files under `external_location` are
cleaned up alongside the Glue entry. Parquet result files remain for
the user's bucket lifecycle.

**Cleanup timing:** `Client.Close(ctx)` walks the tracked table list
and drops each. Idempotent — safe to call twice; safe on tables
already dropped externally (Glue `DeleteTable` returns
`EntityNotFoundException` on missing; wrapper swallows it).
Concurrent `Close()` calls on separate clients are independent
(distinct random-hex names, no collision).

## Naming + tagging

**Table names:** `gobi_athenaio_<8-hex>_<unix-epoch>`. Greppable,
time-sortable, orphan-friendly.

**Glue tags on every created table:**

- `athenaio_created_at` — RFC 3339 timestamp.
- `athenaio_client_id` — UUID of the Client instance.
- `athenaio_query_id` — Athena query execution ID (available at
  `StartQueryExecution` return, before waiting for completion).

Enables an out-of-band sweep Lambda (EventBridge cron →
`ListTables` filtered by tag → drop tables older than N hours). Ship
a reference sweep implementation under
`contrib/athenaio/examples/sweep/`. athenaio ships the convention;
users own the sweep policy.

## Client type + config

```go
type Client struct {
    // internal: athena + glue + s3 SDK clients, workgroup, result
    // location, created-table list, config
}

type ClientConfig struct {
    AWSConfig       aws.Config    // via aws-sdk-go-v2's config loader
    Workgroup       string        // Athena workgroup, required
    ResultLocation  string        // s3://bucket/prefix/ for CTAS external_location root
    Database        string        // default database context for the SELECT body
    Cleanup         Cleanup       // default CleanupCatalogOnly
    PollInterval    time.Duration // default 500ms with exponential backoff to 30s
    MaxConcurrent   int           // client-level query concurrency limit; 0 = no limit
}
```

**Rationale for a Client type** (vs. per-call config): workgroup +
result location + AWS config are session-scoped, reused across many
queries in a typical workflow. Matches pgio's `*Client` (which
wraps a `*pgxpool.Pool`) — consistent shape across vendor packages.

## Primary API

```go
// UnloadAndRead is the T3 entrypoint. Composes a partition-aware
// CTAS, submits it, waits for completion, verifies the resulting
// Glue table matches the spec, and returns a LazyFrame with
// PartitionMetadata populated.
func (c *Client) UnloadAndRead(ctx context.Context, spec UnloadSpec) (*gobi.LazyFrame, error)

type UnloadSpec struct {
    // SELECT body only — no CREATE TABLE, no CTAS keywords, no
    // outer ORDER BY. athenaio composes the outer query.
    SQL             string

    // Hash-bucketing partition columns. Ordered; hash(a, b) ≠ hash(b, a).
    // Empty = no partitioning (T1 behavior).
    PartitionBy     []string

    // Number of hash buckets. Required if PartitionBy is set.
    // Iceberg: no practical ceiling. Hive: caps around 100.
    BucketCount     int

    // Within-partition sort keys (Iceberg only for enforcement).
    // On Hive tables, becomes an ORDER BY in the composed SELECT
    // and SortEnforced=false in the returned metadata.
    OrderBy         []gobi.SortKey

    // Table format hint. FormatIceberg (default) attempts Iceberg,
    // falls back to Hive on unsupported workgroup with a warn log.
    // FormatHive forces Hive even where Iceberg is available.
    TableFormat     TableFormat

    // Optional: override the default parquet compression codec.
    // Defaults to SNAPPY.
    Compression     string
}

type TableFormat int
const (
    FormatIceberg TableFormat = iota
    FormatHive
)
```

## Escape hatches

Symmetric with `WithPartitionAssertion` on the core-side (see
`PARTITION-METADATA.md`). Advanced users need to bypass the wrapper
for composite partition columns, custom hash functions, or
Athena-specific options athenaio doesn't expose.

```go
// RawCTAS submits a user-composed CREATE TABLE ... AS statement
// verbatim (no wrapping, no read-back verification, no partition
// spec inference). The user provides the PartitionMetadata
// explicitly and owns correctness.
func (c *Client) RawCTAS(ctx context.Context, sql string, meta gobi.PartitionMetadata) (*gobi.LazyFrame, error)

// RawQuery submits a SELECT / non-CTAS statement and returns a
// LazyFrame with no partition claim. For T1-shaped workflows
// (Athena query → parquet download → LazyFrame) where partitioning
// isn't needed.
func (c *Client) RawQuery(ctx context.Context, sql string) (*gobi.LazyFrame, error)
```

## Errors + observability

**Composed SQL logged on FAILED state.** Include both:
- User-provided SELECT body.
- athenaio-composed outer CTAS.

**QueryStats on the returned LazyFrame:**

```go
type QueryStats struct {
    QueryExecutionID string        // Athena's execution ID
    ResultPrefix     string        // s3://bucket/prefix/gobi-athenaio/<uuid>/
    TableName        string        // gobi_athenaio_<hex>_<epoch>
    ScannedBytes     int64         // Athena's DataScannedInBytes
    EngineTime       time.Duration // execution time, not including queue
    TotalTime        time.Duration // including queue
}

// Accessible via a helper:
stats, ok := athenaio.StatsFor(lf)
```

Cost visibility matters — Athena bills per-TB scanned. Users should
be able to see what a run cost them.

**Cancellation semantics:** cancelling `ctx` mid-`Collect()` does
**not** roll back the Athena query — by the time gobi is streaming
S3 files, the Athena query has completed and the bill has landed.
Different from parquetio where cancellation is free. Document this
explicitly.

## Testing

- **Unit tests:** mock the aws-sdk-go-v2 client interfaces
  (`AthenaAPI`, `GlueAPI`, `S3API`). SDK is interface-driven at
  that layer, so most of athenaio's logic (SQL composition, prepass
  validation, metadata assembly, read-back verification) is
  testable without touching AWS.
- **Integration tests:** `//go:build integration` gated, requires
  `ATHENAIO_TEST_WORKGROUP` / `ATHENAIO_TEST_RESULT_LOCATION`
  environment. Same pattern as pgio's `PGIO_TEST_DSN`. Include a
  cost warning in the docs — integration test suite may scan tens
  of MB per run.
- **No local Athena equivalent.** localstack has partial coverage
  but the query engine itself isn't replicable. Accept mocks-only
  for CI; real-Athena tests behind a build tag with dedicated
  credentials.

## Deferred to a later slice

- **`Client.OpenPartitionedTable(ctx, tableName)`** — read an
  existing bucketed Glue table's partition spec and construct
  `PartitionMetadata` from catalog introspection. Same alignment
  proof machinery, fed by `GetTable` instead of by athenaio's own
  write. Falls out for almost free once `PartitionMetadata` is
  defined right and the alignment proof is a pure function of
  metadata + query plan.
- **Cross-source shuffle alignment.** Making athenaio's Trino/Iceberg
  hash tags interoperate with gobi's runtime shuffle hash requires
  reproducing Trino's type-aware combination hash in Go — tractable
  but expensive (decimals, timestamps with zone, arrays). Punt to
  v2.
- **BigQuery / Snowflake / Redshift sibling packages.** All land
  under `contrib/` with the same shape:
  - Client type wrapping vendor SDK/driver.
  - SQL body + partition spec → LazyFrame.
  - `PartitionMetadata` written by the source, consumed by gobi's
    optimizer.
  - Raw* escape hatch for hand-crafted queries.
  - Assertion mechanism for opaque sources.
  Not a shared `sqlio` interface — the vendor-specific bits diverge
  too much (BigQuery `EXPORT DATA`, Snowflake `COPY INTO`, Redshift
  `UNLOAD` all differ in syntax and semantics). But the
  `PartitionMetadata` + alignment machinery in gobi core is uniform.

## Open questions before implementation

- **Iceberg detection strategy.** Attempt-and-fallback vs.
  `GetWorkGroup` probe. Attempt is simpler but produces one failed
  query per Client's first Iceberg-defaulted call in a non-v3
  workgroup. Probe is cleaner but requires reading `EngineVersion`
  from the workgroup config.
- **Result manifest vs. LIST-prefix.** Athena writes a
  `<query-id>-manifest.csv` alongside the result files listing the
  Parquet paths. Using the manifest is more reliable than listing
  the prefix (avoids eventual-consistency edge cases). Adds one
  S3 GET per query — negligible.
- **Concurrent query limits.** Athena's per-account concurrent
  DML limit defaults to 25 but is workgroup-configurable. Do we
  rate-limit inside the Client (respect `MaxConcurrent`) or leave
  it to user? Leaning user-owned for v1 with `MaxConcurrent` as a
  hint; document the AWS quota shape.
- **Prepass caching key.** Cache prepass results by
  (SQL body, workgroup, database). Invalidation? Time-bounded (5m)
  seems fine given typical workflow — user re-Collects within
  session, catalog schema doesn't shift that fast. Document.

# Changelog — athenaio

All notable changes to `github.com/zoobst/gobi/contrib/athenaio`. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [SemVer](https://semver.org). Pre-1.0 minor versions
may introduce breaking changes.

athenaio has its own `go.mod` and versions independently of the core
gobi module. Tags for this module are prefixed with the module path —
see [Versioning](#versioning) below.

## [v0.1.5]

### Added

- **`QueryStats.RowCount`** — total number of output rows produced
  by the query. Populated on the CTAS paths (`UnloadAndRead`,
  `UnloadAndReadBuckets`, `RawCTAS`, `RawCTASBuckets`) by summing
  per-file row counts from parquet footers read at open time —
  zero-data-page cost beyond the read already happening for the
  returned LazyFrame. Left at zero on `RawQuery` (Athena's
  `GetQueryExecution` doesn't expose output row count; call
  `Frame.NumRows()` post-Collect there).

  For the bucket variants the value is the CTAS-wide total (same
  value registered against every non-nil bucket's LazyFrame); per-
  bucket sizes are recoverable via `Frame.NumRows()` after
  `Collect` on that bucket's LazyFrame.

## [v0.1.4]

### Fixed

- **`listBucketFiles` now normalizes trailing slash on the prefix
  before calling `ListObjectsV2`.** Glue's
  `StorageDescriptor.Location` for a CTAS-created table comes back
  as `.../tables/<queryID>` — no trailing slash. S3's `Prefix`
  parameter is a byte-prefix match with no path-boundary awareness,
  so a listing at `.../tables/abc` also picks up sibling keys like
  `.../tables/abc-extra/*` (and, more subtly, would leak keys from
  any longer-UUID neighbor sharing the first N bytes). One-line
  guard in `listBucketFiles` appends `/` when the prefix has a
  non-empty path and doesn't already end in one — no-op on
  already-slashed callers, and the empty-prefix (whole-bucket)
  case skips the normalization to avoid a stray `//`.

- **`isCTASDataKey` rejects `.csv`.** Athena writes a
  `<queryID>-manifest.csv` adjacent to CTAS output (and query-
  result CSVs live in the same neighborhood for non-CTAS queries).
  A CSV is never a CTAS data payload; excluding the suffix wholesale
  is defense-in-depth against Athena ever landing manifest CSVs
  inside the data directory too. Test coverage extended to
  `abc-manifest.csv` + `query-result.csv`.

## [v0.1.3]

### Fixed

- **Athena engine v2 CTAS output was invisible to athenaio.**
  `listBucketFiles` filtered results with `HasSuffix(key, ".parquet")`,
  but engine v2 writes parquets *without* an extension (files look
  like `20240101_120000_00000_asdfg_bucket-00000`). Every v2 data
  file got dropped at list time and callers saw a spurious "no
  result files under ..." error — the same error text as the
  workgroup-override case in v0.1.1, but a completely unrelated
  root cause.

  Fix: switch to a negative filter (`isCTASDataKey`). Exclude the
  paths that are provably non-data — Iceberg `metadata/*` files
  (`*.metadata.json`, `*.avro`), Hive symlink manifests
  (`_symlink_format_manifest/*`), Hadoop-style job markers
  (`_SUCCESS`, `_committed_*`, `_started_*`), checksum sidecars
  (`*.crc`), directory markers (keys ending in `/`) — and accept
  everything else. Anything mis-classified as data slips through to
  `parquetio.ReadReader` which fails loudly, so a stray file
  surfaces at read time with a clear parse error rather than
  silently vanishing at list time.

## [v0.1.2]

### Changed

- **CTAS output location is now steered exclusively via
  `ResultConfiguration.OutputLocation`; the CTAS WITH-clause
  `external_location` / `location` property is no longer emitted.**
  Workgroups with `EnforceWorkGroupConfiguration=true` strip the
  WITH-clause hint anyway (that's the entire root cause of the
  v0.1.1 workgroup-override bug), so relying on it was a coin flip:
  honoring workgroups respected it, enforcing workgroups silently
  dropped it. `OutputLocation` is the one knob Athena treats
  consistently — honoring workgroups use it, enforcing workgroups
  override it — and the v0.1.1 Glue `StorageDescriptor.Location`
  lookup already reconciles the override case.

  Implementation:

  - `composeIcebergSQL` no longer emits `location = '...'`.
  - `composeHiveSQL` no longer emits `external_location = '...'`.
  - New `(*Client).submitTo(ctx, sql, outputLocation)` is the
    per-query submit primitive; the old `submit` becomes a thin
    wrapper that uses `c.cfg.ResultLocation`.
  - `tryCTAS`, `RawCTAS`, `RawCTASBuckets` all use `submitTo` with
    their per-CTAS `ExternalLocation` — that's the single
    location signal now.
  - `resolveActualLocation` from v0.1.1 still applies: Glue
    remains the ground truth when `OutputLocation` gets overridden.

  RawCTAS callers who currently embed `external_location` in their
  SQL string keep working — athenaio doesn't parse RawCTAS SQL, so
  the property passes through verbatim. The workgroup either
  ignores it (managed) or honors it alongside `OutputLocation`
  (open). Either way athenaio reads the actual location from Glue
  before listing files.

## [v0.1.1]

### Fixed

- **Workgroups with `EnforceWorkGroupConfiguration=true` silently
  overrode `external_location`, causing every CTAS-based read to
  error with "no result files under ...".** Affects `UnloadAndRead`,
  `UnloadAndReadBuckets`, `RawCTAS`, `RawCTASBuckets` — all four
  variants used the caller-composed `ExternalLocation` when listing
  S3 result files, but the workgroup override made Athena write to
  its own `ResultConfiguration.OutputLocation` instead. athenaio
  then listed the composed prefix (empty) and errored.

  Fix: after CTAS submit + poll succeeds, all four paths now call
  the new `Client.resolveActualLocation` helper, which looks up the
  ground-truth `StorageDescriptor.Location` from Glue and uses it
  for `listBucketFiles`. When the actual location differs from the
  composed one, athenaio invokes `ClientConfig.WarnLog` with a
  message identifying the workgroup-override symptom, so subsequent
  callers don't have to rediscover the root cause.

  The old read-back verification's exact-match Location check
  (`Location mismatch: got X, expected Y` — a hard error) is
  softened to a presence check: verifyLocation now only errors when
  `StorageDescriptor.Location` is absent. The mismatch case is the
  legitimate workgroup-override shape, not a bug — enforcing exact
  match was blocking every managed-workgroup deployment.

  API-visible behavior change: `TestUnloadAndRead_ReadBackVerifyFailure`
  → `TestUnloadAndRead_WorkgroupOverrideWarns`. Callers who
  previously matched on the "Location mismatch" error string get a
  clean "no result files under <actual location>" message instead,
  which is more actionable — it names where athenaio actually
  looked, so operators can inspect that S3 prefix directly.

## [v0.1.0] — 2026-07-26

### Added

- **`Client.UnloadAndReadBuckets(ctx, spec)` — per-bucket parallelism
  variant of `UnloadAndRead`.** Returns `[]*gobi.LazyFrame` (one per
  bucket file) instead of concatenating into a single frame. Enables
  callers to `Collect()` each bucket in parallel goroutines without
  reimplementing the S3 list + read plumbing.

  Requires non-empty `spec.PartitionBy` and `spec.BucketCount > 0`
  (unbucketed CTAS output doesn't guarantee stable per-file semantics).
  Returned slice length equals `spec.BucketCount`; empty bucket
  indices come through as nil slots so `results[i]` maps to bucket
  `i` consistently across peer calls. Each returned LazyFrame carries
  the same `gobi.PartitionMetadata` claim as `UnloadAndRead` would
  attach.

- **`Client.UnloadAndReadBucketsWithMetadata(ctx, spec)` —
  observability companion.** Returns `[]BucketResult{S3URI, Frame}`
  pairs so callers can correlate per-bucket work with S3 provenance
  (logging, telemetry, per-bucket timing). Same contract as
  `UnloadAndReadBuckets` otherwise.

- **`Client.RawCTASBuckets(ctx, spec)` — per-bucket variant of
  `RawCTAS`.** For caller-composed CTAS SQL that includes bucketing
  DDL. Post-CTAS `Glue.GetTable` check verifies
  `StorageDescriptor.NumberOfBuckets > 0` before returning any
  LazyFrame — catches "caller forgot the `bucketed_by` clause" cases
  early with a clear error. Returns `[]BucketResult` sized by the
  reported bucket count.

- **`BucketResult{S3URI, Frame}` struct.** Companion type for the
  metadata variants.

### Notes

- Sibling error isolation: per-frame `Collect()` errors don't affect
  peers. A bad Parquet file on bucket 3 doesn't invalidate bucket 4.
- Cleanup lifecycle unchanged: one Glue-catalog entry per CTAS,
  tracked on the `Client` for `Close`. Not per-bucket.
- Alignment claim holds *within-bucket* (same-K rows are contained in
  one bucket by the bucketing invariant) but NOT across bucket
  indices from two separate calls. Peer alignment applies only when
  bucket `i` on left is joined with bucket `i` on right of two calls
  with the same `spec`.

## Versioning

athenaio publishes its own semver tags. To reference a specific
version:

```
go get github.com/zoobst/gobi/contrib/athenaio@vX.Y.Z
```

Go's module system resolves subdirectory modules via prefixed git
tags: a tag named `contrib/athenaio/vX.Y.Z` marks version `vX.Y.Z` of
this module. See [go.dev/ref/mod#vcs-version](https://go.dev/ref/mod#vcs-version)
for details.

Example release workflow:

```
# From the repository root, after CHANGELOG entries are ready:
git tag contrib/athenaio/v0.1.0
git push origin contrib/athenaio/v0.1.0
```

The main gobi module's tags (`v0.2.7`, etc.) are separate — no
coordination required beyond keeping the `require github.com/zoobst/gobi`
line in athenaio's go.mod consistent with a compatible gobi version.

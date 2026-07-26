# Changelog — athenaio

All notable changes to `github.com/zoobst/gobi/contrib/athenaio`. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [SemVer](https://semver.org). Pre-1.0 minor versions
may introduce breaking changes.

athenaio has its own `go.mod` and versions independently of the core
gobi module. Tags for this module are prefixed with the module path —
see [Versioning](#versioning) below.

## [Unreleased]

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

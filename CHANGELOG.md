# Changelog

All notable changes to gobi are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org). Pre-1.0 minor versions may
introduce breaking changes; check this file when upgrading.

## [v0.2.3]

### Added

- **`Expr.IsNull()` / `Expr.IsNotNull()`.** Return a Boolean
  expression that's true where the inner evaluates to null / non-
  null. Output is never itself null. Composes with `And` / `Or` /
  `Not` and works anywhere an Expr does — `Filter` predicates,
  `WithColumn` derived columns, `Select` projections. Default
  output name via `Namer` is `<inner>_is_null` /
  `<inner>_is_not_null` when the inner has a stable name (e.g.
  `Col("x").IsNull()` → `x_is_null`).

## [v0.2.2]

### Added

- **Shape-preserving `.Over(K)` + `Expr.OverOrdered(cols, keys...)`.**
  `.Over(...)` now accepts shape-preserving inner expressions
  (`Shift`, arithmetic chains, custom row-order-preserving
  ExprNodes) in addition to the existing scalar aggregate shape.
  The inner is evaluated separately on each partition's mini-Frame
  and scattered back to original row positions — input row order is
  preserved (polars-parity). `OverOrdered(partitionCols, orderBy...)`
  is the ordered-window variant: rows within each partition are
  sorted by `orderBy` (stable, per-key `Descending`, nulls-last)
  before the inner runs, so
  `Col("v").Shift(1).OverOrdered([]string{"K"}, SortKey{Column: "t"})`
  gives the previous `v` within each `K` ordered by `t`. Aligned
  fast path fires when `PartitionMetadata` proves both contiguity
  and matching within-partition sort (columns + `Descending` flags)
  — skips the hash-bucket build and the per-partition sort. Scalar-
  aggregate Over is unchanged; `orderBy` is silently ignored on
  order-invariant aggregates (Sum / Mean / Min / Max / Count).

### Changed

- **`Expr.Over(K)` with a non-aggregate inner no longer errors.**
  Previously `Col("v").Over("K")` (or any chain whose immediate
  inner wasn't `*scalarAggNode`) failed with
  `ErrExprTypeMismatch: Over requires a scalar aggregate`.
  It now dispatches to the shape-preserving path. Downstream users
  who relied on the error to catch mistyped expressions should
  switch to explicit type checks or reject in a lint layer — the
  eval-time gate is no longer available.

## [v0.2.1]

### Added

- **`LazyFrame.Explode(col)`.** Exposes the eager `Frame.Explode`
  through the lazy plan surface. Same semantics — geometry columns
  expand multi-part geometries to their constituents; `List<T>`
  columns expand one row per element (empty and null lists both
  produce a single output row with a null element, polars-parity);
  non-exploded columns duplicate across the expansion. Plan-time
  schema propagates the list element type so downstream lazy
  operators see the post-explode column shape. Partition metadata
  drops `SortedBy` unconditionally (row cardinality changes), drops
  the whole claim if the exploded column is itself a partition
  column. Wired through the optimizer walkers, `CascadeEmpty`, and
  the projection-pushdown rule; the executor compiles it via
  `materializeExecOp` since the WKB-decode / element-scatter needs
  the whole batch at hand.
- **`Expr.Shift(n)`.** First-class expression wrapper around
  `Series.Shift` — positive `n` shifts forward (lag), negative
  shifts back (lead). Composes with `WithColumn` / `Select` and
  other Expr combinators (e.g.
  `Col("price").Sub(Col("price").Shift(1))` for period-over-period
  deltas). Works on any column type `Series.Shift` supports
  (numeric, string, timestamp, ...). Per-group shifts via `.Over(K)`
  don't chain in v0.2.1 — see v0.2.2 where the shape-preserving
  Over lands.

### Fixed

- **`AggCount` on non-numeric hashable columns.** The eager
  `GroupBy.Agg` path called `Series.numericAt` in the AggCount
  branch, which rejects UINT64 / UINT32 / STRING / TIMESTAMP (and any
  other type outside the numeric shortlist). Counting non-null values
  in those columns produced `ErrNotNumeric`. Now routes through
  `isNullAtSeries`, so AggCount works on any hashable column type.
  The streaming executor's `countAcc` already had a fallback for this
  case, so `LazyFrame.Collect()` was unaffected — the bug hit only the
  eager `Frame.GroupBy(...).Agg(...)` path.
- **Custom aggregators returning `List<T>` or `Struct<...>`.**
  `builderForType` has supported `arrow.LIST` and `arrow.STRUCT`
  since Track 1a and Track 4 (respectively), but `appendCustomValue`
  had no matching dispatch arm — every custom Aggregator declaring
  one of those output types crashed at emit time with
  `"unhandled builder type *array.ListBuilder"` (or `*array.StructBuilder`).
  Added `appendCustomListValue` (accepts typed slices — `[]string`,
  `[]int64`, `[]float64`, `[]bool`, `[]uint64`, ... — plus `[]any`
  for heterogeneous elements) and `appendCustomStructValue` (accepts
  `[]any` of positional field values, dispatched recursively through
  `appendCustomValue`). Unblocks patterns like "distinct providers
  per group as `List<String>`" and "summary struct per group as
  `Struct<Count, First>`".

## [v0.2.0]

### Added

- **List<T> column support (Level 3).** `arrow.ListType` columns are
  now first-class. `FromStructs` / `ToStructs` round-trip slice fields
  (`[]T` for non-nullable elements, `[]*T` for nullable) as List
  columns; `Frame.Explode` accepts any List column in addition to
  geometry columns; new expression ops `Expr.ListLen()`, `.ListGet(i)`,
  `.ListSlice(start, stop)`, `.ListContains(elem)`, `.ListSum()`,
  `.ListMean()`, `.ListMin()`, `.ListMax()`, `.ListFirst()`,
  `.ListLast()`. `builderForType` covers LIST + Int8/16 + Uint8/16.
  Nested slices and `*[]T` fields are rejected with a clear error.
- **`Expr.Over(partitionCols...)` window functions.** Polars-style
  windowed aggregations that compute a group aggregate and broadcast
  it back to every input row. Chains with the new scalar aggregate
  methods `Expr.Sum()`, `.Mean()`, `.MinAgg()`, `.MaxAgg()`, `.Count()`
  — e.g. `Col("v").Sum().Over("group")` broadcasts each group's sum
  back to its rows. Row order preserved (unlike GroupBy which
  collapses). Multi-key partitions supported. Composes with arithmetic
  for mean-centering, z-score, and similar per-group transforms.
- **Struct-typed columns.** `builderForType` handles `arrow.STRUCT`,
  so Custom ExprNodes can construct and return `Struct<...>` columns
  end-to-end (see [struct_column_test.go](struct_column_test.go) for
  a `Struct<List<Uint64>, Bool>` UDF pattern matching road-snap-shaped
  outputs). `List<Struct<...>>` also carries through Frame
  construction and `ListLen`/other list ops. FromStructs/ToStructs
  don't yet round-trip nested struct fields — a Custom ExprNode is
  the current path for producing Struct columns.
- **Verified: Custom ExprNodes returning List columns.** The Expr
  framework's `Eval(input) (Series, error)` contract accommodates any
  Arrow type on the output side — including variable-length List
  columns produced by user UDFs (H3 GridPath / KRing / PolyfillCells
  patterns). Added a reference test so this stays wired.
- **Partition-aware `LazyFrame` infrastructure.** New
  `gobi.PartitionMetadata{Columns, HashFn, SortedBy, SortEnforced}`
  type attached to scan nodes + propagated through every plan
  operator (`Filter`, `Project`, `WithColumn`, `Drop`, `Limit`,
  `Tail`, `Sort`, `Aggregate`, `Join` — full propagation rule table
  covered by 16 unit-tested subtests). Two alignment predicates:
  `Aligned(meta, cols)` for single-source shuffle-skip checks (Over,
  aligned GroupBy) and `AlignedWith(l, r)` for two-source checks
  (partition-wise Join). `HashFn` uses versioned, source-namespaced
  string tags (`"gobi/xxhash64/v1"` etc.); cross-tag comparisons
  always fail so different sources can't be accidentally assumed
  hash-compatible.
- **`LazyFrame.WithPartitionAssertion(meta)`** escape hatch for
  opaque sources (custom UDFs, hand-crafted parquet scans, external
  ETL). Narrowing-only rule — refuses to widen an existing claim.
  User owns correctness (gobi never verifies against actual data).
- **`parquetio.ReadReader` / `ReadReaderChunksFunc`** — new public
  entry points that accept `io.ReaderAt + int64` size alongside the
  existing path-based API. Enables non-file sources (S3, HTTP,
  in-memory `bytes.Reader`) to feed parquetio without disk
  round-trips. Same options/geo-metadata/predicate-pushdown surface
  as the path-based API. Internal refactor: `openReader` split into
  path-based + reader-based paths that share `openReaderFromRS`.

### Changed

- **BREAKING: `gobi.Aggregator` interface gained a `Merge(other
  Aggregator) error` method.** Every custom aggregator must now
  implement Merge, which combines a peer instance's state into the
  receiver (used by future parallel/window paths — v0.2 hash-partition
  aggregate never splits a group across workers, so the current
  executor never invokes Merge). Aggregator implementations must also
  reset internal state at the start of Aggregate; the eager engine
  reuses a single instance across every group.

### Performance

Four alignment-based fast paths land on top of the new
`PartitionMetadata` infrastructure. All fire only when the input
carries a matching partition claim (via a scan source's
`WithPartitionMetadata` or a user's `WithPartitionAssertion`) —
un-annotated inputs run the existing paths unchanged.

Hardware for measurements: Apple M3 Pro (11 cores), Go 1.26,
`go test -bench -benchtime=3s -count=3`. Median across 3 runs.

- **`.Over(K)` aligned linear-scan fast path — 34% faster.**
  `overFastPathApplicable` (aligned + `SortedBy` starts with
  partition cols + `SortEnforced=true`) triggers a single-pass
  linear-scan reducer in place of the row→group-id hash-map path.
  3.73ms → 2.47ms on 100k-row / 100-group `Sum().Over("group")`.
  Composes end-to-end through the streaming executor —
  `withColumnExecOp` propagates the input plan node's
  `PartitionMetadata` to per-batch Frames.
- **Sort-merge Inner join fast path — 31% faster.**
  `canMergeJoin` (both sides `AlignedWith` + `SortedBy` starts with
  join key + `SortEnforced=true`) selects `sortMergeJoinExec` in
  place of `streamingJoinExec`. Two-pointer merge over encoded keys
  eliminates the hash-index build. 1.98ms → 1.36ms on 10k×10k
  Int64-keyed Inner join. Inner-only for v1; Left/Semi/Anti stay on
  the hash path.
- **Aligned `GroupBy.Agg` linear-scan fast path — 74% faster.**
  `groupByFastPathApplicable` triggers a linear-scan aggregate that
  detects group boundaries via composite-key comparison of
  consecutive rows — skips the `rowKeys []string` alloc, the
  `map[string][]int` group buffer, and the terminal `sort.Strings`
  order pass. 12.4ms → 3.25ms on 100k-row / 1k-group two-column-key
  `AggSum`. Sits after the existing `aggFast` single-primitive-key
  hot path (single-string-key 1BRC workloads keep their tuned
  performance); the new fast path lights up on shapes `aggFast`
  bails on (multi-column keys, First/Last, custom Fn).
- **Streaming hash-join build-side index cache — 49% faster.**
  `streamingJoinExec.buildIfNeeded` now builds the right-side hash
  index once alongside materializing the right Frame; `Next` reuses
  it per probe batch via the new `Frame.joinHashRightWithIndex`
  helper. Previously the exec called `Frame.Join` per batch, which
  rebuilt the full hash index each time (a real bug on any
  workload where the probe side spans multiple `defaultBatchRows`-
  sized batches). 48.1ms → 24.6ms on 200k probe × 100k build
  (4-probe-batch workload). Fires for every Inner/Left/Semi/Anti
  hash join — no alignment claim required.

## [0.1.1] — 2026-07-23

### Added

- **`kmlio` KMZ (zipped KML) support.** `ReadFile` / `WriteFile`
  auto-detect the format from the file extension (`.kmz` → zip
  archive with `doc.kml`); `Read` / `Write` accept
  `Format: FormatKMZ` for io.Reader / io.Writer flows. The reader
  prefers `doc.kml` but falls back to the first `.kml` entry so
  KMZs produced by other tools still parse.
- **`gobi.FromStructs[T]` / `gobi.ToStructs[T]`.** Round-trip
  between a slice of Go structs and a Frame using the same
  struct-tag conventions as csvio (`csv:"col"`, `geom:"true"`,
  `time:"layout"`). Supports every arrow-mappable Go type plus
  pointer-wrapped nullability.

### Changed

- `kmlio.ReadOptions` / `kmlio.WriteOptions` gained a `Format`
  field (`FormatAuto` / `FormatKML` / `FormatKMZ`). Previously
  empty stubs.

## [0.1.0] — 2026-07-22

First tagged release. Everything below is what a caller sees when
`go get`ting this module at `v0.1.0`.

### Core

- `Frame` + `Series` on top of Apache Arrow (`arrow-go/v18`), with
  eager and lazy execution paths sharing the same operator set.
- Expression IR (`gobi.Expr` / `gobi.ExprNode`) with fluent
  combinators (`Col`, `Lit`, `Custom`, arithmetic, comparison,
  logical, `Not`, `Alias`) — data, not code.
- `LazyFrame` with a rule-based optimizer (nine rules: FoldConstants,
  RemoveTrivialFilter, CombineFilters, PushFilterBelowProject,
  PushFilterBelowSort, ProjectionPushdown, PushPredicateToScan,
  CascadeEmpty, plus expression-simplification variants).
- Native streaming executor (`Compile` → `ExecOperator` tree):
  Filter / Project / WithColumn / Drop / Limit / ScanFrame / ScanFile
  stream natively. Streaming hash aggregate for built-in kinds.
  Streaming hash join for Inner / Left / Semi / Anti (Right / Full
  still materialize).
- Data-parallel parquet scan across row-groups (`parquetio.ReadOptions.ScanWorkers`).
- Partitioned parallel streaming aggregate (`resolveWorkers()` at
  Compile time). Fast paths for single-string-key and single-int-key
  groupings.

### DataFrame + Series ops

- Aggregations: `Count`, `Sum`, `Mean`, `Min`, `Max`, `First`,
  `Last`, `Std`, `Var`, `NUnique` — plus custom `Aggregator`
  interface for user-defined reductions.
- `Frame.Unique`, `Frame.ValueCounts`, `Series.Unique`, `Series.NUnique`.
- `Frame.Pivot(index, columns, values, agg)`.
- `Series.Shift(n)`, `Series.Diff(n)`.
- Set ops: `Frame.Concat` (variadic), `gobi.Concat`, `Frame.Union`,
  `Frame.Intersect`, `Frame.Difference`, plus `Series` counterparts.
  Null-equal semantics; type-mismatch errors carry a cast hint.
- Sort (multi-key, stable, nulls-last), Join (all six kinds),
  Explode, Head, Tail, Filter, Take, WithColumn, DropColumn.

### Geometry

- `Point`, `LineString`, `Polygon`, `MultiPoint`, `MultiLineString`,
  `MultiPolygon`, `GeometryCollection`. 2D + optional XYZ.
- Own WKB / WKT codec — no cgo, no GEOS.
- WGS84 ↔ Web Mercator ↔ all 120 UTM zones via Snyder / Redfearn
  formulas.
- Static Sort-Tile-Recursive R-tree with bbox + k-NN queries.
- Area, length, centroid, convex hull, Simplify (Douglas-Peucker),
  Buffer.

### IO subpackages

Every subpackage exposes a consistent `ReadOptions` + `WriteOptions`
struct and (where applicable) `ReadFile` / `WriteFile` / `ScanFile`
entry points. Options field naming is documented in CLAUDE.md.

- **`parquetio`** — Parquet + GeoParquet 1.1 read/write. Column
  projection, row-group predicate pushdown via footer stats,
  bloom-filter write. Parallel scan. `ScanFile` returns a LazyFrame
  with projection + predicate pushdown.
- **`csvio`** — Typed CSV via struct tags. `.gz` / `.zst` / `.bz2`
  auto-detect. Streaming callback API. `ScanFile[T]` returns a
  LazyFrame.
- **`geojsonio`** — Full RFC 7946 GeoJSON: every geometry type
  (Point, LineString, Polygon, MultiPoint, MultiLineString,
  MultiPolygon, GeometryCollection) with optional XYZ. Frame-level
  `ReadFile` / `WriteFile` / `ScanFile`. `.geojsonl` / `.ndjson`
  line-delimited streaming, auto-detected by extension.
- **`gpkgio`** — OGC GeoPackage 1.3 read/write via pure-Go
  `modernc.org/sqlite`. Batch-inserted (`pgx.CopyFrom` alternative
  via transactions), RTree spatial index maintained inline,
  spec-compliant metadata (`application_id`, `user_version`,
  `gpkg_spatial_ref_sys`, `gpkg_contents`, `gpkg_geometry_columns`,
  `gpkg_extensions`). Multi-layer files supported. `ScanFile`
  supports predicate pushdown via `gobi.ExprToSQL`.
- **`pgio`** — **BETA.** PostgreSQL + PostGIS via `pgx/v5` in
  native mode. `ReadQuery` / `ReadTable` / `ScanTable` for reads;
  `WriteTable` uses `pgx.CopyFrom` for 10-100× bulk-insert
  throughput. Geometry columns wrapped in `ST_AsEWKB` on read to
  preserve SRID. Integration tests are `//go:build integration`
  gated — run with `PGIO_TEST_DSN=postgres://...` against a
  PostGIS instance.
- **`kmlio`** — KML (OGC 12-007r2) read/write with Placemarks +
  ExtendedData. Empty `ReadOptions` / `WriteOptions` stubs
  reserved for future config.
- **`shpio`** — ESRI Shapefile read/write (`.shp` + `.shx` + `.dbf`
  + optional `.prj`). Empty `ReadOptions` / `WriteOptions` stubs.

### Cross-format

- `gobi.ExprToSQL(expr) (sql, args, ok)` — translates gobi.Expr
  trees to parameterized SQL fragments. Handles arithmetic +
  comparison + logical + NOT + Alias unwrap; rejects `Custom`
  nodes as untranslatable. Null-safe rewrite for `= NULL` →
  `IS NULL`. Consumed by gpkgio + pgio for predicate pushdown.
- `gobi.SplitConjuncts(expr)` — breaks an expression at top-level
  ANDs so translatable parts can be pushed while untranslatable
  parts stay in the executor.

### Design constraints

- **Pure Go, no cgo.** No GDAL, no GEOS, no libproj. SQLite via
  `modernc.org/sqlite` (pure-Go port). PostgreSQL via `pgx/v5`
  (pure-Go, no libpq).
- **No disk spilling.** If a working set doesn't fit in RAM, the
  process OOMs — that's the accepted failure mode.

### Known limitations (planned, not blocking v0.1.0)

- Polygon set operations (union / intersection / difference),
  dissolve, clip — not implemented. Blocked on a pure-Go polygon
  clipping decision (Martinez-Rueda hand-roll vs adopting an
  existing library).
- Streaming Right / Full joins — materialize their build side
  today. A two-phase streaming variant is possible but hasn't
  landed.
- Vectorized numeric accumulator kernels — deferred until Go
  1.27's stdlib `simd` package ships arm64 support (August 2026).
- Pooled arrow decoder buffers for parallel scan — documented as
  future work in CLAUDE.md; would drop 1BRC peak RSS from ~1.3 GB
  toward ~400 MB.

[Unreleased]: https://github.com/zoobst/gobi/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/zoobst/gobi/releases/tag/v0.1.1
[0.1.0]: https://github.com/zoobst/gobi/releases/tag/v0.1.0

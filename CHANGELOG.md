# Changelog

All notable changes to gobi are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org). Pre-1.0 minor versions may
introduce breaking changes; check this file when upgrading.

## [v0.3.11]

### Fixed

- **parquetio: GeoParquet 1.1 file-level geometry declarations now
  reach `Series.IsGeometry()`.** GeoParquet 1.1 declares geometry
  columns exclusively in a file-level `"geo"` JSON blob (encoding,
  geometry types, CRS as PROJJSON). gobi's own writer additionally
  stamps per-field arrow metadata via `GeometryField` — so gobi ↔
  gobi round-trips saw the field-level tag `IsGeometry` reads. Files
  written by pyarrow / geopandas / DuckDB spatial / Overture Maps
  don't carry that per-field tag, so every geometry-aware operator
  (`s.IsGeometry()`, `Buffer`, `SJoin`, `HilbertSort`, spatial
  predicates) reported the column as non-geometry after read.

  Fix in [parquetio/parquetio.go](parquetio/parquetio.go): `attachGeoKey`
  now parses the "geo" JSON blob and, for each BINARY-typed top-level
  column declared in `meta.Columns` that isn't already tagged, stamps
  `gobi:geometry_type` (from the blob's `encoding`, defaulting to
  `"WKB"`) and `gobi:crs_epsg` (extracted from the PROJJSON
  `id.{authority:"EPSG", code:N}`; `null`/missing CRS resolves to
  4326 per GeoParquet's OGC:CRS84 default) onto the schema field.
  `frameFromTable` now rebuilds each output `arrow.Column` against
  the stamped schema field via `arrow.NewColumn(field, src.Data())`
  so `Column.Field()` — the source of `Series.field` in `NewSeries`
  — carries the tag; `frameFromRecord` already used the schema-side
  field so no change was needed there. Refcount contract preserved
  (`NewColumn` retains the Chunked once, matching the pre-fix
  `Data().Retain()`).

  Pre-existing per-field tags win over the file-level blob — files
  written by gobi still round-trip byte-identically. Non-BINARY
  fields declared as geometry (WKT / GeoArrow-native encodings)
  aren't stamped: gobi's WKB-only geometry path wouldn't recognise
  them anyway, and silently tagging them would misroute downstream
  operators.

  Regression test in
  [parquetio/chunks_test.go](parquetio/chunks_test.go)
  (`TestReadFile_GeoParquet_RecognizesFileLevelMetadata`) writes a
  fixture directly via `pqarrow.NewFileWriter` +
  `AppendKeyValueMetadata` — file-level "geo" blob only, no per-field
  metadata on the geometry column, `crs: null` — then verifies
  `IsGeometry()` on both the full-file read path and the
  Columns-projected read path.

### Verified

Both the v0.3.10 projection fix and this release's geometry-recognition
fix were exercised end-to-end against the original bug report's
fixture: Overture Maps `release/2026-08-19.0` transportation-segment
`part-00000-*.zstd.parquet` (~449 MB, 2,728,305 rows, 21 top-level
columns; `bbox` is a 4-leaf struct, 12 of the 21 columns are
list-of-struct types, so top-level arrow field indices diverge from
parquet leaf indices by 40+ positions before reaching `geometry` at
arrow index 18). `ReadReader` with `Columns: {"id", "subtype",
"class", "geometry"}` returns exactly those four columns in the
requested order, and `projected.Column("geometry").IsGeometry()`
reports true — the two symptoms called out in the report.

## [v0.3.10]

### Fixed

- **parquetio: `ReadOptions.Columns` returned the wrong columns on
  files with nested top-level fields.** `resolveColumns` in
  [parquetio/parquetio.go](parquetio/parquetio.go) was treating each
  requested name's arrow top-level field index as a parquet **leaf**
  column index and passing that straight into
  `pqarrow.FileReader.ReadRowGroups` / `GetRecordReader`. Those two
  indices coincide only for fully flat schemas — as soon as a nested
  top-level field (struct, list-of-struct, map) widens the leaf count,
  every field past it is misrouted onto whatever leaf happened to sit
  at the same slot number.

  Reported against Overture Maps 2026-08-19.0 transportation segments
  (`bbox: struct<xmin,xmax,ymin,ymax>` plus several list-of-struct
  fields): `ReadReader(..., &ReadOptions{Columns: {"id", "subtype",
  "class", "geometry"}})` returned a Frame with `[id, names,
  road_surface]` — three of four requested columns silently absent,
  two unrequested columns present, and the surviving `id` column was
  the only one on the correct leaf. Files written by gobi itself were
  unaffected because the writer only emits flat schemas; the bug was
  strictly a reader-of-third-party-files issue.

  Fix walks the pqarrow `SchemaManifest.Fields` tree for each
  requested name and appends every leaf `ColIndex` beneath it in
  declaration order (new `appendLeafColIndices` helper). Selecting a
  nested top-level field by name pulls in its complete child leaf
  set — matches pyarrow / DuckDB / Polars behavior. Selecting a
  nested descendant by dotted path (`"bbox.xmin"`) remains
  unsupported; only top-level names resolve.

  Regression test in
  [parquetio/chunks_test.go](parquetio/chunks_test.go)
  (`TestReadFile_ColumnProjection_NestedSchema`) writes a
  struct-containing fixture directly via `pqarrow.NewFileWriter` —
  gobi's own writer is flat, so covering this case required reaching
  past it. Verifies both schema and row-value integrity after
  projection past the struct, and that selecting the struct alone
  round-trips all four child leaves.

- **`ReadOptions.Columns` doc clarifies nested-field behavior.**
  Docstring now spells out that struct / list-of-struct / map
  top-level fields pull their full child tree when selected by name,
  and that dotted-path descendants are not a supported selector.

## [v0.3.9]

### Added

- **`Lit(time.Time)` and end-to-end Timestamp comparison support.**
  Prior to this, `gobi.Lit(t)` returned an "unsupported literal type"
  error and Timestamp columns had no expression-layer comparison path
  at all — `Col("ts").Ge(anything)` failed at `promoteForComparison`
  regardless of the RHS type. Fixed in three layers:

  1. `Lit(time.Time)` at [expr_eval.go](expr_eval.go) now stores the
     value as `arrow.Timestamp` (int64 UnixNano) with dtype
     `Timestamp[ns]`, matching the type from_structs.go produces for
     `time.Time` struct fields. `broadcastLiteral` gains a TIMESTAMP
     case using `array.NewTimestampBuilder`.

  2. `promoteForComparison` now allows Timestamp vs Timestamp when
     units match; unit mismatch (e.g. millisecond col vs nanosecond
     Lit) errors with `"Timestamp unit mismatch — cast one side first"`
     rather than silently reinterpreting.

  3. Two new kernels in [series_ops.go](series_ops.go): `cmpScalarTs`
     for Timestamp-col vs Timestamp-lit and `cmpTsTs` for col-vs-col,
     both operating directly on int64 nanosecond values (no float64
     widening — nanoseconds past 2^53 stay exact). Wired via
     `tryScalarTimestampFastPath` in `binOpNode.Eval` and a
     Timestamp/Timestamp branch in `applyBinaryOp`.

  What now works: `Col("ts").{Eq,Ne,Lt,Le,Gt,Ge}(Lit(t))` on
  Timestamp columns, `Col("start").Lt(Col("end"))` on two Timestamp
  columns, `WithColumnExpr("cutoff", Lit(t))` to broadcast a
  Timestamp scalar to every row.

- **Fused-filter path recognizes Timestamp leaves.**
  `parseFusedLeaf` at [filter_fused.go](filter_fused.go) previously
  rejected `*array.Timestamp` outright ("rare filter shape, keep
  fused semantics tight"). Now it accepts Timestamp columns with
  matching-unit Timestamp literals via a new kind=3 leaf. The
  scaffolding (`fusedFilterLeaf.kind = 3`, both `evalRow` and
  `evalRowNoNull` already routing kind=3 through `cmpI64`) was
  already in place — this wire-up completes it.

  Zero-copy int64 view via `tsValuesAsInt64` using `unsafe.Slice` —
  sound because `arrow.Timestamp` is declared `type Timestamp int64`
  at the arrow-go layer (identical size/alignment/layout). The
  F64/I64 leaves get zero-copy views via `Float64Values` /
  `Int64Values`; the Timestamp leaf needs the same treatment to
  keep the fused-vs-general contract on 100M+ row filters where an
  O(n) leaf-setup copy would dominate. First `unsafe` in the root
  package, tightly scoped to one helper with a stated safety
  argument.

  What now fuses: `Col("ts").Ge(Lit(lo)).And(Col("ts").Lt(Lit(hi)))`
  goes through the single fused row-loop with short-circuit AND,
  rather than materializing two Boolean Series and a separate AND.
  Unit-mismatched leaves fall through to the general path (which
  errors clearly at Type()).

## [v0.3.8]

### Performance

- **[compute/](compute/) gains reduction kernels: `SumF64`, `SumI64`,
  `MinF64`, `MaxF64`, `MinI64`, `MaxI64`.** Portable scalar +
  Go 1.27 SIMD (arm64 NEON, amd64 AVX2/AVX-512) paths behind the
  same `//go:build goexperiment.simd` gate as the existing compare
  kernels. Fills a gap arrow-go/compute doesn't cover at all
  (arrow-go has no aggregate/reduction kernels).

  Measured on arm64 (Apple M3 Pro, n=10M, GOEXPERIMENT=simd
  vs default build):

  ```
                  scalar         SIMD           delta
  SumF64          1409 Mrows/s   3005 Mrows/s   2.1×
  MinF64           906 Mrows/s   3726 Mrows/s   4.1×   ← branch-free lane-Min
  MaxF64           977 Mrows/s   3726 Mrows/s   3.8×
  SumI64          3603 Mrows/s   3639 Mrows/s   ~flat  (compiler ILP saturates)
  MinI64          1715 Mrows/s   1813 Mrows/s   ~flat  (scalar in both — no Int64s.Min)
  MaxI64          1728 Mrows/s   1828 Mrows/s   ~flat
  ```

  Float64 Min/Max benefits most because `simd.Float64s.Min` /
  `Float64s.Max` are lane-parallel branch-free operations, versus
  scalar's `if v < m { m = v }` which the compiler can't vectorize.
  Float64 Sum is 2× because scalar `s += v` already extracts some
  ILP via the compiler. Int64 doesn't have `Min` / `Max` methods
  in Go 1.27's `simd` stdlib, so those stay scalar in the SIMD build.

  Wired into `Series.Min` / `Series.Max` / `Series.Sum` null-free
  fast paths ([series_ops.go](series_ops.go) `minMaxF64` + the
  `sumF64Kernel` fallback). Default builds get the identical
  scalar inner loop (no regression); `GOEXPERIMENT=simd` builds
  on arm64/amd64 pick up the SIMD path automatically.

  Also wired into the aligned GroupBy fast path
  ([groupby_aligned.go](groupby_aligned.go) `tryEmitContiguousSIMD`):
  when the input carries `PartitionMetadata` proving per-group row
  contiguity (aligned + sorted + SortEnforced=true) AND an agg is
  Sum/Min/Max/Mean on a null-free single-chunk Float64 or Int64
  column, the group's `vals[start:end]` slice goes directly to
  `compute.SumF64` / `MinF64` / `MaxF64`. Skips the per-group
  `rowsBuf []int` construction + the per-row indexed-access
  Welford loop.

  Pipeline-level impact caveat: on typical GroupBy workloads
  (hundreds-to-thousands of groups, ~100 rows each), the boundary-
  detection cost over 1M+ rows dominates the reduce cost, so
  wall-time savings are within noise. The wire-in delivers real
  savings only when per-group cardinality is large (thousands+
  rows per group) and the reduce is a meaningful fraction of the
  work. The Series.Sum/Min/Max wire-in above is where the
  measurable per-op wins land today (2-4× on null-free Float64
  columns).

  Streaming aggregate integration is NOT covered here — the
  `map[key]*aggGroup` + per-batch bucket → per-group Update path
  walks scattered row indices, not contiguous slices. Adding SIMD
  there would require either sort-then-reduce (O(n log n) sort
  dwarfs the reduce win) or per-batch per-group gather into
  scratch (memory-bandwidth-limited on arm64, no gather
  instruction). Neither is on the roadmap.

- **`Expr.Cast` dispatches through `arrow.compute.CastArray`.**
  Pre-fix, gobi's Cast walked chunks with a per-target builder
  loop (`castToFloat64`, `castToInt64`, `castToFloat32`,
  `castToInt32`, `castToUint64`, `castToUint32` — six ~50-line
  helpers, ~300 lines total). Every element paid a null-check +
  arrow-array typed `Value(i)` accessor + builder `Append`. Scalar.
  Slow.

  arrow-go ships hand-written NEON SIMD (`internal/kernels/
  cast_numeric_neon_arm64.s`) and AVX2/SSE4 (`cast_numeric_avx2_
  amd64.s`) for numeric casts. Routing gobi's Cast through
  `arrow.compute.CastArray` gets us the SIMD kernel on both
  architectures for free.

  Measured on arm64 (Apple M3 Pro, arrow-go v18.7.0, Go 1.27rc1)
  via `benchmarks/arrow_compute/main.go`:

  ```
                                  before      after       delta
  Cast Float64→Int64, n=10M       38.4 ms     2.83 ms     -13.6×
  Cast Float64→Int64, n=1M         3.54 ms    0.30 ms     -11.8×
  Cast Float64→Int64, n=100K       0.475 ms   0.052 ms    -9.1×
  ```

  gobi's Cast now matches `arrow.compute.CastArray` throughput
  directly (3530 vs 3547 Mrows/s at 10M — the ~0.5% delta is
  gobi's chunked-wrapper overhead). Removed ~300 lines of
  hand-rolled scalar cast loops.

  Contract preserved: same supported target types (Float32/64,
  Int32/64, Uint32/64), same silent-wrap / silent-truncate
  overflow semantics (matches Go's built-in numeric conversion),
  same null propagation. Broader arrow-go cast capabilities
  (String↔Int, Date↔Timestamp, etc.) stay out of gobi's public
  Cast API until each path is deliberately exposed with its own
  semantic contract.

  This is the FIRST place gobi calls into `arrow.compute` — see
  the `compute/` subpackage doc for the arrow-go delta table
  (what to route through arrow-go, what to keep bespoke, why).

- **Streaming aggregate: pooled `[]int` payloads flow as `*[]int`
  through the channel path.** Follow-up to the previous item.
  `sync.Pool.Put([]int)` boxes the 3-word slice header into an
  `any` interface — a per-Put heap allocation. And a naive fix
  (`p := s; pool.Put(&p)`) still allocates because the fresh `&p`
  escapes to the heap. The real fix: change `partitionMsg.rows`
  from `[]int` to `*[]int` so the pool's pointer identity flows
  Get → dispatch → channel → worker → Put unchanged. Threaded
  through the append sites via `*partRows[w] = append(*partRows[w],
  row)` and through the worker via a one-time `rows := *msg.rows`
  deref at the top of `workerConsume`.

  Additional measured impact on 1BRC beyond the earlier boxed-slice
  pool: aggregation wall 13.6 s → 12.72 s (−6%). Cumulative across
  this session: 16.6 s → 12.72 s (**−23%**), gobi allocations
  13 GB → 540 MB (**−24× less**).

- **Streaming aggregate: pooled the parallel dispatcher's per-worker
  `[]int` row-index buffers.** Every batch the reader partitions
  rows into `workers` slices and hands each to a worker via
  `partitionMsg.rows`. Pre-fix, each dispatch call allocated
  `workers` fresh `[]int` slices at capacity ~`nRows/workers+8`
  — profiled at **12.75 GB of allocations on 1BRC** (from ~500K
  batches × 11 workers), dominating gobi's total allocation
  footprint.

  Added a `sync.Pool` (`rowsPool`) on the exec. Dispatcher `Get`s
  slices at dispatch time (falling back to `make` on cold cache);
  workers `Put` slices back after `workerConsume` returns; empty-
  partition slices bypass the channel and go straight back. Steady-
  state in-flight pool size caps at `workers × chanBuf` ≈ 44 for
  the default config. `Put`s hand back `s[:0]` so the underlying
  array's capacity is preserved for the next dispatch.

  Measured on the 1BRC fixture (1B rows, Apple M3 Pro, 11
  GOMAXPROCS):

  ```
                          before      after       change
  aggregation wall time   15.5 s      13.6 s      −12%
  gobi allocations        13.06 GB    564 MB      −23×
  peak OS RSS             724 MB      611 MB      −16%
  user CPU time           102 s       88 s        −14%
  ```

  Wall-time win comes from reduced GC pressure — with 23× less
  allocation churn, GC background workers do 14% less work, and
  the CPU freed up serves useful compute. Peak Go heap in-use
  is nearly flat (~553 MB → ~569 MB) because the live working
  set didn't change; only the allocation *rate* dropped.

  Combined with the `buckets` map elimination (previous entry),
  1BRC agg wall drops from 16.6 s → 13.6 s (−18% overall) with
  peak RSS from 1.06 GB → 611 MB (−42% overall).

- **Streaming aggregate: eliminated per-row `map[*aggGroup][]int`
  bucket lookup.** The old `consumeBatch` / `workerConsume` hot
  path did TWO map lookups per row on high-cardinality groupby
  workloads: the primary key→group lookup (`groups[key]`) plus a
  secondary bucket write (`buckets[g] = append(buckets[g], row)`).
  Profiling 1BRC (1B rows, 413 groups) showed the second lookup
  alone consumed ~10 s of CPU time (9% of the profile) with real
  memory-allocation churn from the ephemeral per-batch map.

  Replaced the buckets map with per-`aggGroup` `batchRows []int`
  scratch + a per-batch `touched []*aggGroup` list. Every row now
  does exactly one map lookup (the primary key→group probe) and
  then appends to the group's own reusable slice — no second
  hash. After the row walk, the loop iterates `touched` once,
  calls `Update` on each accumulator, and resets `touched` + each
  group's `batchRows[:0]` for the next batch. `batchRows`
  capacity is retained across batches so steady-state per-row
  cost is one append into an already-allocated slice.

  Measured on the 1BRC fixture (1B rows, Apple M3 Pro, 11
  GOMAXPROCS):

  ```
                          before      after      change
  aggregation wall time   16.6 s      15.5 s     −7%
  peak Go heap in-use     606 MB      553 MB     −9%
  peak OS RSS             1.06 GB     724 MB     −32%
  ```

  The RSS drop dwarfs the wall-time drop because the buckets map
  was allocation-heavy (fresh per batch, keyed on pointer, values
  were growing slices) — removing it drops the working set
  substantially. Wall-time win is modest because the buckets cost
  was already parallelizing across 11 workers; the ~10 s CPU
  saved is ~0.9 s wall on 11 cores.

  Applies to every LazyFrame aggregation, not just 1BRC. Any
  `GroupBy(...).Agg(...)` streaming path benefits — the win
  scales with (rows × groups-touched-per-batch), so highest on
  high-cardinality groupby workloads.

- **Filter fusion for AND-chained scalar comparisons.** Predicates
  of the shape `Col(a) OP lit AND Col(b) OP lit AND …` now evaluate
  in a single row-loop with short-circuit on the first false, rather
  than materializing one Boolean Series per comparison + one more
  per AND. On a 2M-row bbox filter (`lat >= a AND lat <= b AND lon
  >= c AND lon <= d`), filter cost dropped from ~98 ms to ~44 ms
  (~55% faster) on Apple M3 Pro / Go 1.26. Predicates that don't
  match the fused shape (column-vs-column comparisons, string
  equality, OR branches, single leaves) fall through to the general
  `Expr.Eval` path unchanged. Null propagation contract preserved:
  any leaf null on a row surfaces as a null in the mask, which
  `Frame.Filter` treats as false — same behavior as the pre-fusion
  path.

- **Scalar-comparison fast path covers `Ne` / `Le` / `Ge`.** The
  `binOpNode.Eval` fast path that skips literal-broadcasting for
  `(col OP lit)` previously only fired on `Eq` / `Lt` / `Gt` — the
  other three comparisons fell through to the general path, which
  broadcasts the literal to N rows before comparing (allocates one
  N-element float64 slice per literal). Added `Series.NeScalar` /
  `LeScalar` / `GeScalar` and extended `tryScalarFastPath` to
  dispatch them. Independent of the AND-fusion above — helps every
  filter using those operators, even single-leaf predicates.

- **Group-by fast path (`aggFast`) now covers `AggNUnique` and
  `AggMin` / `AggMax` on Timestamp columns.** Previously the fast
  path bailed to the slow per-row `numericAt` loop whenever any
  aggregation was `NUnique` or when Min/Max targeted a Timestamp
  column — the "one bad agg disables the whole call" pattern.
  `aggView` is now a discriminated union covering count-star,
  numeric, timestamp, and hashable shapes, one per aggregation.
  NUnique dispatches to per-group direct-hash maps
  (`map[string]` / `map[int64]` / `map[float64]`) instead of the
  byte-encoded `keyOfAppend` fallback. Timestamp Min/Max compares
  raw int64 (no lossy float64 cast) and emits back as
  `arrow.Timestamp` preserving the source's TimeUnit + TimeZone.

- **Streaming `nUniqueAcc` type-specializes on first Update.** The
  LazyFrame aggregate executor's per-group NUnique accumulator used
  to run `keyOfAppend` — a chunk walk + type switch + byte encoding
  — for every row. Now it inspects the source column once on first
  non-empty Update and switches to a native `map[string]struct{}` /
  `map[int64]struct{}` / `map[float64]struct{}` path. Falls through
  to the byte-encoded generic path for multi-chunk batches or
  non-primitive types. Bit-equal numerics still collapse identically
  to the pre-fix behavior.

- **Null-check hoisting in the fused filter.** When every leaf's
  source column has `NullN() == 0` at setup (typical for numeric
  columns from parquet reads), the row-loop skips the per-row per-
  leaf `IsNull` calls and the validity-bitmap bookkeeping
  entirely. 8M `IsNull` calls on the 2M-row × 4-leaf bbox filter
  turn into zero. Null-carrying columns transparently fall
  through to the general fused evaluator with validity
  propagation preserved.

- **Combined effect on the h3-agg benchmark:** LazyFrame per-iter
  time on the 2M-row filter+groupby+agg pipeline dropped from
  ~152 ms → ~86 ms (**-43%**), landing gobi_lazy within 0.1 ms of
  pandas (86.01 ms). Filter cost alone: 98 → 40 ms (~-59%).
  Bench methodology: 30 iterations, warmup 3, Apple M3 Pro,
  Go 1.26.

  Final four-way ranking on the h3-agg workload:
  ```
  polars      15.21 ms   (Rust SIMD)
  gobi_lazy   85.90 ms   ← tied with pandas
  pandas      86.01 ms   (compiled C loops)
  gobi_pool  154.64 ms   (hand-written Go worker pool)
  ```

  Note that gobi_lazy also uses ~36% less peak RSS than pandas
  (501 MB vs 792 MB) — same throughput, tighter memory footprint.

### Added

- **`parquetio.Write(f, w, opts)` and `geojsonio.Write(f, w, opts)` /
  `geojsonio.Read(r, opts)` / `geojsonio.ReadChunksFunc(r, opts, fn)`.**
  `io.Writer` / `io.Reader` entry points to complement the existing
  path-based `WriteFile` / `ReadFile` / `ReadFileChunksFunc`. The
  caller owns the stream and is responsible for closing it — enabling
  non-filesystem sinks and sources: object-storage uploads / downloads,
  tar-stream entries, in-memory `bytes.Buffer` round-trips, HTTP
  request/response bodies. `WriteFile` / `ReadFile` are now thin
  wrappers that open the file and delegate.

  `parquetio.Write` wraps the caller's writer in a `writeOnly`
  adapter that hides `io.Closer` from `pqarrow.FileWriter.Close`
  — pqarrow's `Close` calls `Close` on the underlying writer when
  it implements `io.Closer`, which would violate the caller-owns-w
  contract (e.g. a `*gzip.Writer` wrapping a `*os.File`, or a
  caller who intends to append more bytes after the parquet
  payload).

  `geojsonio.Read` / `ReadChunksFunc` default to
  `FormatFeatureCollection` when `opts.Format` is `FormatAuto`
  (path-based readers still sniff `.geojsonl` / `.ndjson`). Set
  `Format: FormatLineDelimited` explicitly to stream line-delimited
  GeoJSON from a reader.

  This brings the write-side API of `parquetio` and `geojsonio` in
  line with `kmlio` (which already had both variants). `csvio`
  has no writer at all today; `gpkgio` needs a seekable file on
  disk (SQLite backend), and `shpio` is a multi-file format —
  neither fits a single `io.Writer` cleanly.

### Shelved

- **Pooled `memory.Allocator` for arrow-go's parquet decode buffers.**
  Prototyped as `parquetio.NewPooledAllocator` — a size-bucketed
  pool implementing arrow-go's `memory.Allocator` interface, opt-in
  via `ReadOptions.Allocator`. Both an unbounded `sync.Pool[[]byte]`
  variant and a bounded `chan *[]byte` variant were measured
  against `-workers=11` on the 1BRC fixture; both **increased**
  peak RSS by 30-50% versus the default `memory.GoAllocator`
  (measured: 659 MB → 950-1046 MB). Alloc-size histogram
  confirmed 100% of arrow-go's requests fell inside the bucketed
  range — the pool was catching allocations, but arrow-go's
  per-batch fresh-output-buffer pattern means the pool retention
  piles up ON TOP of the fresh in-flight allocations rather than
  substituting for them. The earlier "700-900 MB RSS reduction"
  prediction in the Future Work doc was based on a mental model
  that didn't match arrow-go's actual allocation shape.
  Implementation deleted (both `allocator_pool.go` and its test
  file) rather than shipping something that misleads opt-in users
  into worse RSS.

## [v0.3.7]

### Added

- **`AggMin` / `AggMax` on Timestamp columns.** The eager `GroupBy.Agg`
  path, the streaming `LazyFrame.GroupBy(...).Agg(...)` executor, the
  aligned/fast group-by builder, and the `.Over(...)` scalar-agg path
  all now accept Timestamp columns for min/max aggregation. Output
  preserves the source column's arrow.TimestampType — same TimeUnit
  and TimeZone come through, so downstream code doesn't have to
  interpret raw int64 nanoseconds. Nulls skipped; all-null groups
  emit null (parity with the numeric path).

  Under the hood: `minMaxAcc` detects Timestamp on first `Update`
  and switches to an int64 comparison track (no lossy float64 cast).
  A new `timestampAt(s, i)` helper mirrors `numericAt` but for
  Timestamp columns. `newAggregateNode` and both `buildAggBuilders`
  paths declare the output column as the source's TimestampType
  when the aggregation is `AggMin` / `AggMax` on a Timestamp source.

  Before this, `f.Lazy().GroupBy(k).Agg(Aggregation{Column: "dt",
  Kind: AggMin})` on a Timestamp column errored with
  `series is not numeric: timestamp[ns, tz=UTC]`. Workaround was
  `WithColumn(UnixNano)` before the GroupBy; that hoop-jump is
  gone. Removing it from benchmarks cut LazyFrame per-
  iter time from ~266 ms to ~152 ms on a 2M-row fixture (Apple M3
  Pro), because the workaround required a full frame-sized cast
  pass.

  `AggSum` / `AggMean` / `AggStd` / `AggVar` on Timestamp still
  error — those have no obviously-correct semantics without a
  Duration return type, and pandas / polars split on the details.

### Fixed

- **Parallel scan executor: race in worker-error path.** In
  `exec_scan_parallel.go`, `e.errs` is buffered at `len(subs)` so
  multiple workers could all successfully send an error, and each
  then called `close(e.done)` — a second-arriving worker panicked
  with `close of closed channel`. Guarded `close(done)` with a
  `sync.Once` (`doneOnce`) shared between the worker error path and
  `Close()`. Also simplified `Close()` since `doneOnce` subsumes
  its check-then-close dance.
- **parquetio: `ErrChunksAborted` wrap dropped the inner sentinel.**
  `ReadFileChunksFunc` / `ReadReaderChunksFunc` wrapped callback
  errors as `fmt.Errorf("%w: %v", ErrChunksAborted, cbErr)` — the
  `%v` verb formatted `cbErr` into a string and severed the wrap
  chain, so `errors.Is(err, cbErr)` returned false. During normal
  parallel-scan teardown (`errScanClosed` returned from the cb after
  `done` fires), the outer scan executor didn't recognize the
  wrapped sentinel and routed it through the "real worker error"
  branch instead of the shutdown branch. Switched to
  `fmt.Errorf("%w: %w", ErrChunksAborted, cbErr)` (Go 1.20+
  multi-`%w`); both sentinel identities now propagate through
  `errors.Is`. The existing `ErrChunksAborted`-in-chain test still
  passes.

## [v0.3.6]

### Added

- **String operations** — 13 row-wise string ops on Arrow String
  columns, at both the Series and Expr layers. Composes with
  LazyFrame.Filter / WithColumn / Select the same way the
  numeric and spatial ops do. All null-propagating.

  Case / trim / length:
  - `StrLower` / `StrUpper` — Unicode-aware case conversion
  - `StrTrim` — strip whitespace both ends (`strings.TrimSpace`)
  - `StrTrimLeft(cutset)` / `StrTrimRight(cutset)` — cutset-based
  - `StrLen` — Unicode codepoint count (not byte length)

  Predicates (Boolean output):
  - `StrContains(substr)` — literal substring match
  - `StrStartsWith(prefix)` / `StrEndsWith(suffix)`
  - `StrRegexMatch(pattern)` — RE2 semantics, pattern compiled once

  Transforms (String output):
  - `StrReplace(old, new)` — literal replace all
  - `StrRegexReplace(pattern, replacement)` — RE2, capture-group
    references (`$1`, `$2`, `${name}`) supported
  - `StrSlice(start, end)` — substring by codepoint index with
    Python-style negative indexing (end=0 → "to end", out-of-range
    clamps)
  - `StrConcat(suffix)` — append a constant suffix to every value

  Example (case-insensitive substring filter):

  ```go
  lf.Filter(gobi.Col("city").StrLower().StrContains("angeles")).Collect()
  ```

  Under the hood: shared `strMapString` / `strMapInt64` /
  `strMapBool` drivers walk each chunk once, apply a Go stdlib
  string function per row, emit a new column. Regex patterns
  compile once, not per row.

- **Datetime operations** — 13 row-wise datetime ops at the Expr
  layer, delegating to Series-level methods (9 extractors already
  existed in `series_time.go`; this release adds the missing
  `Nanosecond` / `DateTruncate` / `DateFormat` at the Series layer
  and lifts all of them to Expr).

  Extractors (Int64 output, timezone-aware):
  - `Year`, `Month`, `Day`, `Hour`, `Minute`, `Second`,
    `Nanosecond`, `Weekday`, `DayOfYear`

  Transforms:
  - `DateTruncate(unit)` — calendar-aware for `year` / `month` /
    `day` (start-of-year at 00:00:00 in the value's timezone) and
    wall-clock aligned for `hour` / `minute` / `second`.
  - `DateFormat(layout)` — Go time layout, empty → RFC3339
  - `AddDuration(d)` / `SubDuration(d)` — Timestamp arithmetic
    preserving the source column's TimeUnit and TimeZone

  Example (rows in Q4 of 2026):

  ```go
  lf.Filter(
      gobi.Col("ts").Year().Eq(gobi.Lit(int64(2026))).
          And(gobi.Col("ts").Month().Ge(gobi.Lit(int64(10)))),
  ).Collect()
  ```

### Changed (post-review)

Six items from two rounds of v0.3.6 code review, addressed before
tagging. First five landed together; the sixth (sub-unit precision
rounding) came from a follow-up round after the type-preservation
fix landed.

- **`AddDuration` / `SubDuration` / `TruncateTo` / `TruncateToCalendar`
  now genuinely preserve the source column's TimeUnit and TimeZone.**
  Previously all four routed through an internal
  `buildTimestampNsSeries` helper that hardcoded
  `{Unit: Nanosecond, TimeZone: ""}` — so a `Timestamp[ms,
  "America/New_York"]` source became `Timestamp[ns]` with an empty
  TZ tag, and the plan-declared schema (which claimed preservation)
  disagreed with runtime output. New `buildTimestampSeriesLike`
  helper inherits the source type. Regression coverage: two tests
  build a `Millisecond`-unit source and a `America/New_York`-tagged
  source, run each transform, and assert both the type and the row
  values.

- **Regex patterns on `Expr.StrRegexMatch` / `Expr.StrRegexReplace`
  compile once at Expr-build time**, not per batch at Eval.
  `strOpNode` caches the `*regexp.Regexp` alongside any
  `regexp.Compile` error; the error surfaces at Type() / Eval() —
  matches the deferred-error style of `LitNull` on unsupported
  types. Streaming Filter chains that scan many parquet row groups
  now pay one compile, not N.

- **`Series.StrReplace` renamed parameters** from `(old, new string)`
  to `(find, replacement string)`. Old form was legal Go but shadowed
  the built-in `new` inside the function body — a future intra-
  function `new(T)` call would silently pick up the shadow. Public
  API impact is nil (positional args; identifier names aren't part
  of the signature).

- **Awkward error messages** — `Str%s` and `dt %s` prefixes on
  nil-inner / type-mismatch paths produced strings like
  `"gobi: Strlower on nil inner expression"`. Renamed to
  `"gobi: str.%s"` / `"gobi: dt.%s"` — matches the `strOp.String()`
  / `dtOp.String()` snake-case rendering used everywhere else.

- **Reference to a nonexistent pairwise `Expr.StrConcat(other Expr)`**
  removed from the `Series.StrConcat` docstring. The pairwise
  variant is a v0.3.7 candidate (needs a new binary-op node
  shape since `strOpNode` only carries one inner).

- **Sub-unit precision rescaling in `buildTimestampSeriesLike` now
  uses round-half-away-from-zero, not Go's default truncation
  toward zero.** The type-preservation fix landed with an integer
  `/` division on the ns → source-unit rescale, which quietly
  produces asymmetric error direction across the epoch: a
  post-1970 `AddDuration(500ns)` on a `Timestamp[us]` source rounds
  DOWN (toward zero, magnitude decreases) while the same operation
  on a pre-1970 source rounds UP (also toward zero, but magnitude
  decreases in the negative direction). New `roundedDivTimestamp`
  helper rounds symmetrically. Regression tests: unit tests over
  the ±half / ±just-under-half / ±exact-multiple cases, plus an
  integration test that adds 500ns to a Timestamp[us] value on
  both sides of the epoch and asserts both rounded away from
  zero. Precision loss is still intrinsic to storing sub-unit
  deltas — the fix guarantees the rounding direction is
  consistent regardless of sign.

### Notes

- **General-purpose dataframe posture.** v0.3.6 is the first
  release where gobi's non-geospatial surface is broad enough to
  recommend as a general-purpose Go dataframe library, not just a
  geospatial one. String ops, datetime extractors, and the
  existing LazyFrame optimizer + Parquet / CSV / Postgres I/O
  give a Go team the "read → transform → aggregate → write"
  pipeline they'd otherwise reach for Polars-via-CGo or
  DuckDB-embedded to get.

## [v0.3.5]

### Added

- **Hilbert-curve spatial pre-sort** — the piece that turns v0.3.4's
  row-group bbox pushdown from "works on synthetic clustered files"
  into "works on real-world data." Three layers:

  - `geometry.HilbertIndex(x, y, bounds, order)` — pure-math
    quantized-2D-to-1D space-filling curve index. Order 16 default
    (65,536 × 65,536 grid); iterative quadrant-rotation kernel with
    no lookup tables; ~30 ns/call. Out-of-bounds points clamp to
    the grid edge; empty bounds return 0.
  - `Frame.SortByHilbert(geomCol)` — reorders rows by the Hilbert
    index of each row's geometry centroid. Bounds derived from the
    column's own centroids (self-contained, no caller bbox
    needed). Stable sort, nulls last. Under the hood: two-pass
    O(N) — centroid + bbox computation, then quantize + sort.
  - `parquetio.WriteOptions.HilbertSort` — opt-in flag that runs
    `SortByHilbert` on the primary geometry column before bbox
    covering augmentation and the actual write. Sort runs against
    the pre-augmentation frame so the covering columns are
    computed on the sorted row order.
  - `HilbertSortWithCovering(f, geomCol)` — fused single-pass
    equivalent of `SortByHilbert` followed by
    `WithBboxCoveringColumns`. Parses each row's WKB exactly once
    (vs twice for the two-step form) — the sort's centroid pass
    and the augmentation's bbox pass share a single scan.
    `parquetio.WriteFile` uses this automatically when
    `HilbertSort=true` and `SkipBboxCovering=false` (the common
    combination). Benchmark on a 40k-row 8-vertex-octagon corpus:
    **27.7ms → 23.5ms (1.18× faster), 112MB → 86MB (24% less
    memory), 33% fewer allocs**. Speedup grows with polygon vertex
    count — the sort's f.take gather cost caps the improvement on
    small polygons; on 1000-vertex GSHHS coastlines the WKB parse
    dominates and the fused form wins by a bigger factor.

  Same design principle as the previous release: no assumptions
  about the caller's read pattern in the writer's defaults —
  `HilbertSort` opts in explicitly.

- **Real-world pushdown benchmark**: on a shuffled 40k-polygon
  grid (mirrors the spatial-incoherence shape of a raw
  `shp → parquet` dump), a small AOI query against the same file
  written insertion-order vs. Hilbert-sorted:

  | Read pattern                       | ns/op     | B/op       | allocs/op |
  |------------------------------------|----------:|-----------:|----------:|
  | Insertion-order, no pushdown       | 2,962,573 | 27,218,131 |     4,297 |
  | Insertion-order, WITH pushdown     | 2,958,110 | 27,339,329 |     5,086 |
  | Hilbert-sorted, WITH pushdown      |   632,654 |  2,100,576 |     2,594 |

  Pushdown alone on shuffled data buys ~0% (row-group bboxes are
  too wide to prune anything). Hilbert-sort + pushdown: **4.7×
  faster read, 13× less memory allocated**.

- **GSHHS demo re-benchmarked with sort.** Regenerated
  `experiments/data/GSHHS_i_all.parquet` with `-hilbert=true`
  (the new default in `gshhs_to_geoparquet`). Row-group bboxes went
  from "every group spans ±180°" (pre-v0.3.5) to 9 spatially-
  distinct clusters covering the Americas / North Atlantic / Europe
  / Asia / Antarctica. On a California AOI query: **75.8% of the
  file skipped** (2 of 9 row groups kept), up from ~15% (1 of 9)
  on the insertion-order version.

- **`experiments/gshhs_to_geoparquet` `-hilbert` flag** — defaults
  to `true`. Applies during both single-file conversion and the
  `-merge` step (so the merged file gets globally sorted, not just
  locally sorted per input).

- **`GeomDWithin(other, distance)` — the killer spatial-join
  operator.** Reports whether two geometries are within a given
  planar distance. "Roads within 100m of a coastline", "AOIs within
  5km of a point of interest", "polygons within 50m of a
  track" — all map straight to this call. Three layers:

  - `geometry.WithinDistance(a, b, distance) bool` — pure primitive
    with a bbox-min-distance short-circuit: two shapes whose
    bounding rectangles are already farther apart than `distance`
    return false without walking a single edge.
  - `Series.GeomDWithin(other, distance) (Series, error)` — row-
    wise predicate, null-propagating.
  - `Expr.GeomDWithin(other Expr, distance float64) Expr` — lazy
    composition alongside the seven binary predicates.

  distance is in coordinate units — meters for projected CRSes,
  degrees for lon/lat (project first with `GeomToCRS`). distance=0
  degenerates to `Intersects`; negative or NaN → all-false.

- **DWithin row-group pushdown**. `CanPossiblyMatch` grew a case
  for the new `*geomDWithinNode`: expand the constant's bbox by
  `distance` in all directions, then apply the standard bbox-
  overlap test. Row groups whose bbox is more than `distance`
  from the constant's bbox get pruned before any WKB decode.
  Verified via `TestSpatialPushdown_DWithin_PrunesFarRowGroups`.

- **Two spatial-sort orderings, chosen by access pattern.** Both
  are peers — pick the one that matches how you'll query the file:

  - **`Frame.SortByHilbert(geomCol)` + `SortByHilbertWith(geomCol,
    opts)` + `HilbertSortOptions`**. Space-filling-curve ordering
    that preserves 2D locality symmetrically. Best for point
    queries, diagonal AOIs, and multi-file / multi-partition
    sorts that need cross-file locality — pass
    `HilbertSortOptions.Bounds = <union of all partitions'
    bounds>` and each partition's Hilbert indices land on the same
    curve, so downstream merges preserve global order.
    `SortByHilbert(geomCol)` is the thin zero-opts wrapper.

  - **`Frame.SortBySTR(geomCol, leafSize)`**. Sort-Tile-Recursive
    partitions rows into ⌈√(N/leafSize)⌉ vertical strips sorted by
    x, then sorts each strip by y — producing rectangular row
    groups. Best for datasets queried predominantly along
    axis-aligned AOIs (latitude bands, admin regions, timeseries
    windows), where STR's rectangular tiles overlap fewer row
    groups than Hilbert's diagonal-crossing curve.

  Both are null-last, stable-within-leaf, and swap into
  `WriteOptions.HilbertSort` interchangeably at the parquet layer
  (though today the writer flag maps only to Hilbert; STR is a
  library-level primitive callers apply before write).

### Fixed

None — v0.3.5 is purely additive.

### Notes

- The v0.3.4 CHANGELOG's description of `ReadOptions.Predicate`
  was accurate but easy to misread: it's a **row-group pruning
  hint**, NOT a row filter. The reader walks each row-group's
  footer stats and skips groups whose bounds prove no match, but
  every surviving group's rows come through untouched. Callers
  wanting only the matching rows still run `Frame.FilterExpr(pred)`
  on the eager path or feed via `LazyFrame.Filter(pred)` on the
  lazy path (which does both — pushdown + row filter — in one
  chain).

## [v0.3.4]

### Added

- **Spatial predicates as `Expr`-form operators** in `expr_geom.go`.
  `GeomIntersects`, `GeomContains`, `GeomWithin`, `GeomDisjoint` now
  exist as fluent methods on `Expr`, so a spatial filter composes into
  the same `LazyFrame.Filter(...)` chain as scalar predicates:

  ```go
  lf.Filter(
      gobi.Col("level").Eq(gobi.Lit(1.0)).
          And(gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi))),
  ).Collect()
  ```

  The right-hand side accepts either a `Lit(geom)` constant (fast
  path — reuses the tested `Series.GeomIntersects` driver, so bbox
  short-circuit and null propagation carry over unchanged) or another
  geometry-column expression (new capability — pair-wise per-row
  `geometry.Test`, not available at the Series level today).

  Under the hood, a new `literalGeomNode` gives geometry literals
  their own expression-tree node — separate from the scalar
  `literalNode` — so a future optimizer can inspect a constant
  geometry's bbox to prune GeoParquet row groups. `Lit(v)` routes any
  value implementing `geometry.Geometry` through `LitGeom` so callers
  don't need a separate constructor for the common shape.

  Null semantics match Polars: any null operand (left null, right
  null, or `Lit(nil)`) yields a null output row rather than false.

  Test corpus: constant-right for all four predicates, column-right
  intersects with null-propagation on both sides, the motivation's
  compound `level == 1 AND intersects(aoi)` filter running end-to-end
  through `LazyFrame.Collect`, `Lit(polygon)` routing to
  `literalGeomNode`, degenerate `LitGeom(nil)` producing all-null
  output, and the `ExprNode` reflection surface (Children / Type /
  String) that tree walkers depend on.

  Full predicate surface exposed in this release (see below).
  `GeomDWithin(other, distance)` — the spatial-join operator — is
  the natural next PR, blocked on a `geometry.MinDistance(a, b)`
  primitive.

- **Three more spatial predicates as `Expr`-form operators** —
  `GeomTouches`, `GeomCrosses`, `GeomOverlaps` — completing the
  four-plus-three surface flagged in the design proposal. Same
  executor path as `GeomIntersects`: constant-right fast path reuses
  the Series driver, column-right runs pair-wise `geometry.Test`,
  nulls propagate from either side.

  `geometry.Predicate` grew three enum values (`PredTouches`,
  `PredCrosses`, `PredOverlaps`) so `geometry.Test(pred, a, b)`
  dispatches to the existing `Touches` / `Crosses` / `Overlaps`
  primitives with no closure-per-op indirection at the executor.

- **GeoParquet 1.1 row-group bbox pushdown**. The design's
  transformative payoff. Writer-side, `gobi.WithBboxCoveringColumns`
  augments every write with per-row bbox columns
  (`<geom>_bbox_xmin` / `_ymin` / `_xmax` / `_ymax`) and declares
  them under `columns[geom].covering.bbox` in the geo metadata —
  standard GeoParquet 1.1 covering-columns spec, so pyogrio /
  geopandas / DuckDB spatial read them idiomatically too. Reader
  side, the existing `CanPossiblyMatch` walker grew a case for
  `*geomPredicateNode`: when the right operand is a `literalGeomNode`,
  look up min/max stats on the four covering columns and skip row
  groups whose bbox is disjoint from the constant's bbox.
  `parquetio.ReadOptions.Predicate` (already `gobi.Expr`) accepts
  spatial predicates now; the `parquetio.ScanFile(path).Filter(pred)`
  lazy shape gets it via the optimizer's `PushPredicateToScan` rule,
  no caller plumbing required.

  **Predicate is a row-group pruning hint, not a row filter.** The
  reader walks each row-group's footer stats and skips groups whose
  bounds prove no match, but every surviving group's rows come
  through untouched. Callers wanting only the matching rows still
  run `Frame.FilterExpr(pred)` on the eager path or feed via
  `LazyFrame.Filter(pred)` on the lazy path (which does both —
  pushdown + row filter — in one chain). Easy trap to fall into
  when `ReadOptions.Predicate` reads like a filter; naming it a
  "hint" would have been more honest.

  New API surface introduced in this feature — see the "Changed
  (post-review)" block below for the round-2 additions that pair
  with the pushdown: `ReadOptions.IncludeCoveringColumns`,
  `WriteOptions.SkipBboxCovering`, `geometry.PredDisjoint`,
  `geometry.PlanarRingArea`. `LitGeom` and the seven `Expr.Geom*`
  builders land under this same v0.3.4 tag from the earlier bullet.

  Predicates supported for pruning: Intersects / Contains / Overlaps
  / Touches / Crosses (all require bbox overlap as a necessary
  condition) plus Within (same necessary condition) plus Disjoint
  (skips when the row group is provably inside the constant).
  Column-right or nested predicates pass through as "possibly
  matches" — never wrongly prune.

  Verified via `parquetio/spatial_pushdown_test.go`: a two-cluster
  corpus written into two row groups; an AOI covering only cluster A
  returns exactly A's rows (cluster B's row group is skipped); an
  AOI covering both returns all rows; covering columns land in the
  output schema; concave (L-shaped) Disjoint literal preserves the
  row group its bbox contains (correctness case for the review-fix
  below); rectangular Disjoint literal prunes cleanly.

### Changed (post-review)

Nine items from two rounds of v0.3.4 code review, addressed before
tagging. First five are the round-1 items (correctness + UX shape);
the next four are round-2 (nits + bugs surfaced during lazy-path
conversion).

- **Disjoint pruning correctness**: the naive rule "row group inside
  lit's bbox → no row is disjoint" is only sound when lit's polygon
  area equals its bbox area (a filled rectangle). For concave / holed
  / country-boundary literals, a row inside lit's bbox but outside
  lit's shape IS genuinely disjoint, and the naive prune would
  silently drop it. Gated on `litIsBboxRectangle` — the common
  AOI-rectangle case still prunes, everything else keeps the row
  group and falls back to per-row filtering. Aligns with gobi's
  hard rule: silent-wrong is worse than doing extra work.

- **`geometry.PredDisjoint` enum value** replaces the previous
  `PredIntersects` + `invert=true` side-channel on
  `geomPredicateNode`. `n.pred` is now the single source of truth
  for which predicate applies. `geometry.Test(pred, a, b)`
  dispatches Disjoint directly as `!Intersects(a, b)`; the executor
  drops its post-invert logic; output-column naming drops its
  invert-flag override. Less state, fewer coupling surfaces.

- **`ReadOptions.IncludeCoveringColumns`** (default false).
  Preserves the `WriteFile(f)` ↔ `ReadFile(path)` round-trip
  contract: the bbox covering columns land in the *file* (for
  pruning) but stay hidden from the returned frame unless the
  caller opts in. Round-trip tests read back with the same column
  count they wrote. The columns are still used for row-group
  skipping regardless of the flag.

- **`WriteOptions.SkipBboxCovering`** (default false). Opt-out for
  workloads where the writer's extra scan + 32 bytes/row aren't
  worth the read-side benefit — tiny frames, streaming append
  loops, or targets whose consumer doesn't do row-group pruning.

- **`LitGeom` is now a predicate-only marker.** The old
  `literalGeomNode.Eval` path materialized N copies of the WKB blob
  as an Arrow Binary column (Arrow's monotonic-offsets constraint
  gives no cheap "N rows point at one buffer" form). Non-predicate
  positions (Select / WithColumn) now error at Eval with a message
  pointing to the workaround (build the broadcast column outside
  the Expr layer). Mirrors GeoPandas' approach — the constant lives
  as a Python-object scalar and never becomes a column. The
  constant-right predicate fast path detects the marker before Eval
  runs, so the common shape is unaffected.

- **Pushdown idempotency fix** in the parquetio
  `WithPredicatePushdown` callback. The optimizer runs a fixed-point
  loop; the callback was rebuilding a fresh `ScanFile` with the
  predicate AND-ed onto `opts.Predicate` on every pass, producing an
  exponentially-nested `(P AND P AND P …)` chain 30+ deep by the
  iteration cap. Fixed by walking the current `opts.Predicate` tree
  for structural equality (via `Expr.String()`) and returning nil
  (`= "no change"`) when the incoming pred is already applied. Only
  surfaced during the lazy-path conversion of the `gshhs_query`
  demo — the eager `ReadFile(path, {Predicate: p})` path never
  exercised the optimizer.

- **Schema/runtime alignment for hidden covering columns** in
  `parquetio.ReadSchema`. `ReadSchema` returned the full file
  schema (including the 4 bbox covering columns), but
  `frameFromRecord` returned batches with those columns dropped
  when `IncludeCoveringColumns` was false. The scan node's declared
  `Schema()` (from `ReadSchema`) then disagreed with the batches
  actually flowing through the executor, and downstream operators
  walked column indices into the wrong types — the `utf8 vs
  float64` concat panic. Fixed by applying the same
  covering-column projection in `ReadSchema` so plan schema and
  runtime frames stay in agreement. Same reachability caveat:
  eager path never triggered this.

- **`geometry.PlanarRingArea`** exported (was internal
  `planarRingArea`). Deduplicated `predicate_stats.go`'s local
  shoelace implementation — the invariant "planar area of a ring"
  now lives in one place. Callers wanting spherical geographic area
  should still use `Polygon.Area(u Unit)`; `PlanarRingArea` is
  planar-only and CRS-agnostic.

- **Doc + test polish** from the round-2 review: refreshed the
  Disjoint-branch docstring in `canMatchGeomPredicate` (it still
  described the pre-fix `invert=true` shape); tightened the concave
  Disjoint test to prove upper-right cluster IDs specifically
  survive (`>= 200 rows` alone would have passed a bug that kept
  both row groups); annotated the empty-bounds branch to cover both
  the nil-literal and empty-Polygon degenerate cases; noted the
  rectangle check's representation-strictness; documented
  `coveringColumnNames`'s deliberate error-swallowing behavior on
  malformed geo metadata.

### Benchmarks

- **`BenchmarkSpatialFilter_*`** in `expr_geom_bench_test.go` —
  synthetic 5000-polygon corpus, three shapes:

  | Pattern                              | ns/op    | B/op    | allocs/op |
  |--------------------------------------|---------:|--------:|----------:|
  | Compound Expr (v0.3.4, one shot)     |  867,854 | 2.44 MB |    20,175 |
  | Two-phase eager (cached + Series op) |  401,204 | 1.22 MB |    10,086 |
  | Cached scalar + Expr geom filter     |  410,708 | 1.23 MB |    10,159 |

  Takeaway: routing through the Expr executor costs essentially
  nothing (411μs vs 401μs, within 2.5%). The 2× cost of the
  compound one-shot comes entirely from re-scanning the scalar
  predicate each iteration.

- **`BenchmarkSpatialPushdown_ClusteredFile`** in
  `parquetio/spatial_pushdown_bench_test.go` — 10k-polygon two-cluster
  parquet, two row groups, AOI over cluster A only:

  | Read                          | ns/op   | B/op    | allocs/op |
  |-------------------------------|--------:|--------:|----------:|
  | No predicate (both groups)    | 931,492 | 5.09 MB |     1,170 |
  | With predicate (1 group skip) | 598,045 | 2.49 MB |     1,080 |

  **1.56× faster read, 51% less memory** when pushdown prunes a
  row group. Practical benefit scales with how spatially-clustered
  the row groups are: files sorted by Hilbert / R-tree ordering see
  the full effect; files in insertion order (a raw SHP → parquet
  dump) see modest gains because continent-spanning polygons leak
  their bboxes into every row group. Spatial-sort is a v0.3.5+
  candidate.

## [v0.3.3]

### Added

- **First-class `Ellipse` type** in `geometry/ellipse.go`. Companion
  to Circle (same "lightweight value, deliberately NOT a Geometry —
  OGC has no ellipse encoding" shape). Two constructors, standard
  methods, closed-form Ramanujan circumference:

  ```go
  type Ellipse struct {
      Center   Point
      SemiA    float64 // "first" semi-axis, along +X in local frame
      SemiB    float64 // "second" semi-axis, along +Y in local frame
      Rotation float64 // CCW radians from +X axis
  }

  e := geometry.NewEllipse(center, semiA, semiB, rotation)
  e, err := geometry.EllipseFromFoci(f1, f2, majorAxis)  // classic definition

  e.Contains(p)               // (x'/a)² + (y'/b)² ≤ 1 in local frame
  e.Area()                    // πab (exact)
  e.Circumference()           // Ramanujan II approximation
  e.Bounds()                  // axis-aligned bbox of rotated ellipse
  e.Boundary(n) Polygon       // closed CCW n-gon
  e.BoundaryLine(n) LineString
  ```

  `EllipseFromFoci` validates that `majorAxis >= |f1 - f2|` (returns
  `ErrEllipseFromFoci` otherwise — the two foci must fit inside the
  would-be ellipse) and handles the coincident-foci case as a
  circle. Rotation aligns with the f1→f2 direction; SemiB is
  perpendicular; center is the midpoint of the foci.

  Ramanujan II circumference: `π(a+b)(1 + 3h/(10 + √(4-3h)))` where
  `h = ((a-b)/(a+b))²`. Exact for the circle case (h=0); ~1e-9
  relative on moderate eccentricity (2:1 axis ratio); degrades to
  ~1e-4 at extreme eccentricity. No closed-form solution exists —
  the exact perimeter is an incomplete elliptic integral of the
  second kind.

  Bounds derived from the parametric extremum:
  `x_half = √(a²cos²θ + b²sin²θ)`, `y_half = √(a²sin²θ + b²cos²θ)`.

  Test corpus: axis-aligned Contains at boundary + diagonal
  outside; 90°-rotated Contains verifying axis swap; area /
  circumference agreement vs analytic; bounds for axis-aligned +
  rotated; Boundary output vertex count / closure / on-ellipse
  invariant; EllipseFromFoci for axis-aligned + rotated + coincident
  (circle) + too-small majorAxis + non-positive majorAxis.

- **Series-level Ellipse containment** in
  `series_geom_ellipse.go`: `Series.GeomEllipseContains(e) Series`
  — Boolean per row, matching the shape of GeomCircleContains.
  Points tested directly; non-Point rows tested via their centroid.
  Null rows pass through as null.

- **Two-circle intersection + lens polygon** in
  `geometry/circle_intersect.go`. Analytic — no sweep-line involved,
  no dependency on the Martinez-Rueda machinery — since a two-arc
  intersection has a closed-form solution:

  ```go
  // Zero, one, or two boundary crossing points. One point =
  // externally or internally tangent; zero = disjoint, nested, or
  // concentric.
  pts := geometry.CircleIntersectionPoints(c1, c2)

  // The overlap region as a Polygon, sampled arcSegments per arc.
  // ~2·arcSegments vertices, wound CCW, CRS inherited from c1.
  lens := geometry.LensPolygon(c1, c2, arcSegments)
  ```

  Intersection points computed from the standard chord-midpoint
  construction: project c2's center onto the line from c1's center,
  the chord's perpendicular half-length falls out of Pythagoras.
  Tangent cases clip `h² < 0` to zero to survive float64 noise near
  the tangent — otherwise a ULP of round-off would return zero
  points instead of one.

  Lens sampling: sample each arc between the two intersection
  points, picking the arc direction that passes *through* the other
  circle's center (that's the arc bounding the overlap region).
  Direction picking normalizes the CCW sweep between endpoints into
  `[0, 2π)`; if the "through" angle lies in that CCW range, sweep
  CCW, otherwise sweep CW. Endpoints are forced bit-exact
  post-sample so downstream Clip / topology code sees coincident
  vertices as coincident. Ring is post-checked and reversed if the
  reconstruction happened to come out CW — cheap and preserves area.

  Special cases handled directly (never reach the arc sampler):
  disjoint → empty Polygon; nested → smaller circle's Boundary;
  tangent → empty Polygon (zero-area lens indistinguishable from
  empty for consumers).

  Test corpus: overlap (two points at expected coords), disjoint,
  nested (both argument orders), external + internal tangent
  (single-point return), concentric (nil), CRS propagation, lens
  area against the analytic formula
  `r₁²·acos(...) + r₂²·acos(...) − ½·√((-d+r₁+r₂)(d+r₁-r₂)(d-r₁+r₂)(d+r₁+r₂))`
  for equal + unequal radii within 1e-3 rel err at 128–256
  segments/arc, disjoint + tangent → empty polygon, nested → smaller
  circle boundary (order-independent), CCW winding invariant (both
  argument orders), closed ring, vertex count = ~2·arcSegments,
  default-segments fallback.

## [v0.3.2]

### Added

- **Great-circle sampling + densification** in
  `geometry/geodesic.go`. Complements existing Haversine (distance
  only) and pairs with Simplify / the antimeridian-split path —
  polylines can now be densified along their geodesic before
  splitting at ±180°, avoiding the "New York → Tokyo drawn as a
  straight cartesian line through the middle of the Pacific"
  failure mode:

  ```go
  // n points along the great-circle arc from a to b, inclusive
  // of both endpoints. Requires geographic CRS on both sides.
  samples, err := geometry.SampleGeodesic(a, b, n)

  // Replace each segment of a LineString with its geodesic
  // densification at ≤ stepMeters spacing (uses mean Earth radius).
  dense, err := geometry.DensifyGeodesic(line, stepMeters)
  ```

  Implementation: standard spherical linear interpolation (slerp)
  on unit-sphere vectors — ~30 lines of math, no ellipsoidal
  corrections (matches the sphere assumption used by Haversine).
  Antipodal inputs return `ErrAntipodalPoints` (great circle
  isn't unique through antipodes); coincident inputs return the
  point replicated; projected-CRS inputs return
  `ErrGeodesicRequiresGeographic`. Endpoints are preserved
  bit-exact via a post-slerp overwrite (float64 lat/lon
  reconstruction can drift by a ULP otherwise).

  Test corpus covers endpoint preservation, midpoint correctness
  on an equatorial arc, the "NYC↔Tokyo great-circle midpoint sits
  in the Arctic" case, arc-length agreement with Haversine within
  1e-4, coincident + antipodal + projected-CRS + n<2 error paths,
  and the multi-segment "no duplicated joints" invariant for
  DensifyGeodesic.

- **Series wrapper** in `series_geom_geodesic.go`:
  `Series.GeomDensifyGeodesic(stepMeters)` — row-wise geodesic
  densification over a LineString column. Non-LineString rows pass
  through unchanged (Point / MultiPoint / Polygon don't have
  "segments" in the geodesic sense — callers wanting polygon-ring
  densification can extract rings, densify each as a LineString,
  and rebuild). Null rows stay null; projected-CRS Series return
  `ErrGeodesicRequiresGeographic` up front rather than after
  per-row parse.

## [v0.3.1]

### Added

- **First-class `Circle` type + least-squares fit** in
  `geometry/circle.go`. Circle is a lightweight utility value (not
  a Geometry — OGC SFA has no encoding for circles, so forcing it
  into the Geometry interface would break every io format):

  ```go
  type Circle struct {
      Center Point
      Radius float64  // in Center's CRS linear unit
  }

  c, residuals, err := geometry.FitCircle(points, geometry.CircleFitOptions{})
  c.Contains(p)           // bool
  c.Distance(p)           // signed: negative inside, positive outside
  c.Area()                // πr²
  c.Circumference()       // 2πr
  c.Boundary(64)          // closed Polygon (65 vertices, CCW)
  c.BoundaryLine(64)      // open LineString (64 vertices)
  ```

  Two fit methods, selected via `CircleFitOptions.Method`:
  - **Taubin (default)** — Chernov's algebraic fit with a Newton
    step on the characteristic cubic. Unbiased when the point cloud
    covers only a partial arc of the true circle (the common
    real-world case: sensor sweeps, orbit tracks, anchorage
    circles). Preferred.
  - **Kasa** — plain algebraic. Faster (no root-find), but biased
    toward smaller radii on partial-arc inputs. Use when speed
    dominates and inputs span most of the circumference.

  Test corpus: perfect circle (both methods bit-exact), noisy 90°
  arc (Taubin beats Kasa on radius error — this is why Taubin is
  the default), collinear degenerate returns `ErrCircleFit`, <3
  points errors, CRS propagates from input.

- **Series-level Circle ops** in `series_geom_circle.go`:
  - `Series.GeomCircleContains(c) Series` — Boolean per row. Points
    tested directly; non-Point rows tested via their centroid. Null
    rows pass through as null.
  - `Series.GeomDistanceToCircle(c, unit) Series` — Float64 per
    row, signed distance to boundary (negative inside).
  - `Series.GeomFitCircle(opts) Circle` — aggregate fit across
    every non-null row's representative point.

  All follow the established `series_geom_*.go` Arrow-refcount
  discipline.

## [v0.3.0]

### Added

- **Polygon boolean ops** in `geometry/`:
  - `Clip(subject, mask)` (intersection),
  - `Union(a, b)`,
  - `Difference(a, b)`,
  - `SymDifference(a, b)`,
  - `Boolean(a, b, op, opts ClipOptions)` (shared entry point),
  - `Dissolve(geoms []Geometry)` — collection reducer using
    spatially-sorted divide-and-conquer merge (like shapely's
    `unary_union`).

  Implementation is a from-scratch Martinez-Rueda sweepline (event
  queue + status structure + subdivision + inOut classification +
  contour reconnection with hole nesting via PIP + area sort). Pure
  Go — no cgo, no GEOS, no libproj. Events pooled via `sync.Pool`
  so the M-cell hot loop drops per-call allocations.

  A **Sutherland-Hodgman fast path** dispatches automatically when
  both operands are single-ring convex Polygons and the op is
  intersection (`BenchmarkClip_ConvexFastPath` runs at 998 ns/op /
  7 allocs vs the general sweep's 4258 ns/op / 33 allocs).

  Accepted inputs: `Polygon`, `MultiPolygon`. Requires a projected
  CRS on both sides (`ErrGeographicCRS` on geographic input).
  Configurable relative tolerance via `ClipOptions.Tolerance`;
  default (`1e-10`) is chosen for coastal-scale UTM coordinates.

- **Series-level polygon boolean ops** in `series_geom_clip.go`:
  `Series.GeomClip(mask)`, `Series.GeomUnion(other)`,
  `Series.GeomDifference(other)`, `Series.GeomSymDifference(other)`,
  `Series.GeomDissolve()`, `Series.GeomEstimateUTMCRS()` (aggregate
  UTM zone lookup — matches geopandas's `GeoDataFrame.estimate_utm_crs`),
  and `Series.GeomToCRS(target)` (row-wise reprojection).

- **Series batch spatial predicates**: `Series.GeomIntersects`,
  `.GeomContains`, `.GeomWithin`, `.GeomDisjoint`, `.GeomTouches`,
  `.GeomOverlaps`, `.GeomCrosses` — all return a Boolean Series,
  nulls pass through. Match geopandas's `GeoSeries.<predicate>`
  semantics.

- **Series row-wise transforms**: `Series.GeomBuffer(distance, opts)`,
  `Series.GeomSimplify(tolerance)`, `Series.GeomConvexHull()`,
  `Series.GeomEnvelope()`. All produce a new geometry Series with the
  input's CRS metadata.

- **Series row-wise metric**: `Series.GeomDistance(other, unit)` —
  min Euclidean distance from each row's geometry to `other`, in the
  requested unit.

- **Series introspection**: `Series.GeomIsEmpty()`,
  `Series.GeomIsValid()` (Boolean), `Series.GeomType()` (String).
  Validity checks include: line ≥ 2 points with no consecutive
  duplicates, polygon rings ≥ 3 unique vertices with no self-
  intersection (subset of OGC's full rules; sufficient for
  "safe-to-feed-to-Boolean/Buffer" filtering).

- **Antimeridian handling** for geographic-CRS inputs:
  - `geometry.CrossesAntimeridian(g)` — detects |Δlon| > 180° on
    any adjacent-vertex pair.
  - `geometry.AntimeridianCrossings(g)` — returns (±180, lat)
    crossing points.
  - `geometry.SplitAtAntimeridian(g)` — splits crossing
    LineString/Polygon/Multi geometries at ±180° via linear-lon
    interpolation.
  - `Series.GeomCrossesAntimeridian()` and
    `Series.GeomSplitAtAntimeridian()` — Series wrappers.
  - **`EstimateUTMCRS` now returns `ErrAntimeridianCrossing`** on
    crossing input instead of silently picking the wrong zone
    (previously a Fiji-shaped `[178, -178]` bounds landed on UTM
    31N over Africa).

- **Square-style Buffer**: new `BufferOptions.Style` field:
  - `BufferRound` (default, existing behavior) — semicircle caps
    + rounded joins.
  - `BufferSquare` — Point emits an axis-aligned square of
    half-width = distance; LineString gets flat/extended caps +
    mitre joins; Polygon gets mitre corners. Matches shapely's
    `cap_style=square` + `join_style=mitre`. Faster and produces
    fewer vertices — a 1024-segment round buffer emits 1025
    vertices; the square variant emits 5.

- **Per-io struct-tag namespaces on FromStructs / ToStructs.** New
  `StructOption` type with `StructTagFormat(format)` picks the
  primary tag namespace for a call. Resolution fallback:
  format-tag → `gobi:` → `csv:` (legacy) → field name. A tag value
  of `"-"` in any consulted namespace skips the field entirely.
  Exported helper `gobi.ResolveFieldName(sf, format)` for io
  packages that build their own struct convenience wrappers.
  Backward compatible: existing callers with only `csv:"..."` tags
  see identical output.

- **Struct-direct read/write wrappers across every io package.**
  Each package exposes typed convenience methods that route
  through `gobi.FromStructs` / `gobi.ToStructs` with the
  package-specific tag namespace:

  | Package | Namespace | Read | Write |
  |---|---|---|---|
  | parquetio | `parquet:` | `ReadStructs[T]` | `WriteStructs[T]` |
  | csvio | `csv:` | `ReadStructs[T]` / `ReadStructsReader[T]` | — (read-only pkg) |
  | geojsonio | `geojson:` | `ReadStructs[T]` | `WriteStructs[T]` |
  | gpkgio | `gpkg:` | `ReadStructs[T]` | `WriteStructs[T]` |
  | shpio | `shp:` | `ReadStructs[T]` | `WriteStructs[T]` |
  | kmlio | `kml:` | `ReadStructs[T]` | `WriteStructs[T]` |
  | pgio | `pgio:` | `ReadStructsQuery[T]` / `ReadStructsTable[T]` | `WriteStructsTable[T]` |

  Each wrapper is ~10 lines of glue calling
  `gobi.StructTagFormat("<namespace>")` so the namespaced tag wins
  over `gobi:` / `csv:` / field name. Same struct can carry
  different names per format (shp's 10-char DBF alias,
  `parquet:"-"` to omit from parquet only, etc.). Round-trip tests
  in each package's `structs_test.go`. pgio's is guarded by the
  existing `integration` build tag.

### Changed

- `geometry/buffer.go` no longer names polygon Union/Clipping as
  out-of-scope — the sweep engine shipped in this release.
  Negative buffers and self-intersecting positive buffers on
  concave inputs remain unaddressed and will be handled in a
  follow-up that composes `Union` over the offset outputs.

### Fixed

- **Slow-path Clip mis-classified coincident edges when subject
  and clipping-in-MultiPolygon-wrap represented the same polygon.**
  The general Martinez-Rueda path chose sameTransition vs
  differentTransition by comparing the two coincident partners'
  `inOut` values — but `inOut` is derived from sweep-line state at
  insertion time, and the two partners are inserted against
  different predecessors. On identical CCW octagons, five of eight
  edges survived; the result was a half-octagon with area 141
  instead of 283. Fix: added `polyForward` (a stable per-event
  flag set at enqueue time, recording whether the LEFT sweep event
  corresponds to the ring-order start of that edge) and switched
  `handleOverlap`'s different-role branch to compare polyForward
  instead of inOut. Regressions in
  `geometry/clip_multipoly_dup_test.go`.

- **Hole/exterior misclassification on Dissolve outputs with many
  overlapping intermediate regions.** The contour reconnection used
  to derive each ring's `isHole` flag from a `prevInRes` chain —
  which tracks *sweep-line adjacency*, not geometric enclosure. On
  a Dissolve intermediate with densely-nested overlapping regions
  the chain misclassified rings, producing outputs that either
  dropped area or double-counted it. The bundled bench showed a
  13% area disagreement vs geopandas at N=500 heavily-overlapping
  polygons, with an inverse sign flip at intermediate N counts
  (61% under-report at N=250, 14% over-report at N=500).

  Fix: replaced the `prevInRes`-chain classifier with
  `classifyHolesByContainment`, which sorts rings by absolute
  planar area descending and PIP-tests each ring's first vertex
  against every strictly-larger candidate. All prefix sizes now
  match geopandas to at least 8 significant figures. See
  `benchmarks/gobi/dissolve_bisect.go` for the diagnostic that
  found this.

- **GeoParquet PROJJSON emission** now embeds canonical PROJJSON
  extracted from pyproj for every supported projected CRS
  (EPSG:3857 + WGS 84 / UTM zones 32601–32660 and 32701–32760).
  The previous minimal blob was rejected by pyproj with "Missing
  base_crs", so `geopandas.read_parquet` on gobi-written
  projected-CRS files failed. Data lives in
  `geometry/projjson_data.json` and is bundled at compile time via
  `//go:embed`. Verified end-to-end: gobi writes EPSG:32611 →
  geopandas reads it → CRS round-trips as `WGS 84 / UTM zone 11N`.

### Correctness (validation)

**Bit-exact parity with geopandas** on the bundled two-way bench.
Every op — Clip, Union, Difference, Dissolve, Reproject — reports
0.0000% relative area spread against geopandas at least 8
significant figures. Signature parity on EstimateUTMCRS (returned
EPSG) matches exactly.

**polyclip-go known-bugs regression tests.** polyclip-go's README
calls out two open issues; both are permanent regression tests in
`geometry/clip_polyclip_regressions_test.go` using the exact
inputs from the upstream issues. Both cases: gobi produces the
correct answer where polyclip-go is documented to fail.
- polyclip-go issue #3 (Union of four touching rectangles):
  polyclip returns area 3, gobi returns 4 (correct).
- polyclip-go issue #8 (Intersection of a 227-point inner polygon
  fully inside a 27-point outer, in WGS84 coordinates around LA):
  polyclip returns empty, gobi returns the inner polygon.

### Performance (500 heavily-overlapping polygons, Apple M3 Pro)

Bundled bench pipeline: `EstimateUTMCRS → ToCRS → boolean ops` on
500 WGS84 subject polygons clustered around Los Angeles.

**Where gobi wins:**
- **EstimateUTMCRS: ~83× faster than geopandas** (0.38 ms vs 31.8
  ms). gobi's implementation is direct arithmetic on bounds + a
  WGS84 → zone lookup; pyproj routes through the general PROJ4
  pipeline.
- **Reproject (WGS84 → UTM 11N): ~10× faster than geopandas**
  (2.2 ms vs 21.0 ms). gobi uses ellipsoidal Redfearn formulas
  inline; pyproj/PROJ4 handles the fully general case and pays
  for it. Output areas match to 8 significant figures.
- **Dissolve** (after the divide-and-conquer merge switch): ~45 ms
  for 500 heavily-overlapping polygons (505 MiB allocations, 114
  GCs) — down from an earlier linear-fold implementation at 1130
  ms / 13.7 GiB / 2740 GCs (**26× faster, 27× less allocation**).
  Within ~1.3× of shapely on this op instead of 38× away.
- **Single-shot Clip / Difference / Union wall time is
  competitive with geopandas** — gobi is 1.02× slower on Clip,
  1.23× faster on Difference, 1.44× faster on Union at the
  benchmark's 32-vertex-per-polygon size.

**Bench harness**: `benchmarks/gobi/gobi_clip_bench.go` +
`benchmarks/python/geopandas_clip_bench.py` consume the same
`clip_subject.parquet` + `clip_mask.parquet` fixtures.
`benchmarks/python/compare_clip_parity.py` runs the automated
two-way diff (exits non-zero on >2% area disagreement).
Gobi-side benchmarks include pprof CPU + heap profiles plus
periodic MemStats snapshots so allocation trends are visible
without a profiler.

### Known limitations

- **Antimeridian split**: holes that themselves cross the
  antimeridian are dropped rather than split. Ring crossings > 2
  collapse to a single ring per side rather than potentially
  disjoint rings.
- **Numeric epsilon**: default `1e-10` relative tolerance suits
  UTM-scale (~1e6 m). At larger magnitudes (geocentric coordinates
  or exotic projections) callers should supply an explicit
  `ClipOptions.Tolerance`.
- **Constant-factor sweep tuning** (event-queue heap-vs-slice
  choice, status-structure allocation reuse, sorted-slice `insert`
  copy loop) is on the follow-up list but not urgent given the
  overall performance parity.

## [v0.2.22]

### Fixed

- **`frameToBatch` double-Retained every column, systemically.**
  The exec.go bridge between `*Frame` and `arrow.RecordBatch` called
  `arrs[i].Retain()` on top of `array.NewRecordBatch`'s internal
  Retain — the comment claimed "NewRecord's contract requires a live
  ref" but arrow-go's `NewRecordBatch` already Retains each column
  itself (verified against v18.4.1 through v18.7.0). Result: every
  batch produced by frameToBatch leaked exactly one refcount per
  column. Because frameToBatch is on every hot path in the streaming
  executor (filter, project, withColumn, drop, rename, explode,
  streaming/sort-merge join, materialize wall, fused streaming, scan
  file, scan Frame) this compounded across every batch of every
  Collect. Present since v0.2.9 when the streaming executor landed.
  Fix: rely solely on NewRecordBatch's Retain. Refcount-balance test
  proves the invariant: `TestFrameToBatch_RefcountBalanced`.

- **Streaming exec ops orphaned every intermediate Frame.** Every
  op that ran `batchToFrame(batch)` → compute → `frameToBatch(out)`
  dropped both the input Frame and the output Frame on the floor
  without Release. In isolation each Frame's ref chain was only kept
  alive by Go GC (GoAllocator's `Free` is a no-op), but for
  streaming pipelines that produce many batches through fresh
  arrays (FilterExpr's Take, executeSelect's per-batch column
  construction) this pinned every batch's arrays until process
  exit. Fix: every op releases the intermediate `frame` after the
  compute, the derived Frame after the frameToBatch conversion, and
  the fused-streaming path releases the running Frame between
  ApplyToFrame links. Rename's identity short-circuit is handled
  via pointer equality to avoid a double-Release. New tests:
  `TestScanFrameExec_ReleasesSliceFrames`,
  `TestFilterExec_ReleasesIntermediateFrames`,
  `TestProjectExec_ReleasesIntermediateFrames`,
  `TestWithColumnExec_ReleasesIntermediateFrames`,
  `TestLazy_FilterSelect_RefcountBalanced`.

- **`streamingJoinExec.Close` and `sortMergeJoinExec.Close` leaked
  the materialized build side.** Both ops call `Execute` on the
  right subtree once and stash the resulting Frame for the plan's
  lifetime, but Close never Released it. Every completed join
  pinned one full right-side (or both sides, for sort-merge) Frame
  for the entire Collect. Fix: Close now Releases the materialized
  Frames and nils the fields. Idempotent — safe against double-close.

- **`athenaio.readBucketFiles` pinned every source Frame forever.**
  The `frames` slice accumulated one `*gobi.Frame` per bucket file
  and returned without Releasing any of them. For a 10-bucket
  UnloadAndRead at ~800 MB per bucket, that's ~8 GB of arrow buffers
  pinned per call. The concat path also missed a `chunked.Release()`
  after `arrow.NewColumn(field, chunked)` — the same NewColumn/
  Release dance the v0.2.19 audit had closed at ~35 other sites.
  Fix: extracted the concat portion into `concatFramesSingleChunk`
  which consumes its input frames (Releases them via `defer`) and
  Releases the intermediate chunked. Error paths in the openBucket
  loop also Release previously-loaded frames. New tests:
  `TestConcatFramesSingleChunk_ReleasesInputs` and its error-path
  counterpart guard both the happy path and the schema-mismatch
  path against future regressions.

- **`scanFileExec` had an unnecessary `f.Retain()`/`f.Release()`
  dance around frameToBatch.** With frameToBatch's over-Retain fix,
  `array.NewRecordBatch`'s internal Retain already gives the batch
  an independent ref on each column, so the callback's Frame can
  Release freely after the callback returns without invalidating
  the batch. The dance was harmless but obscured the ownership
  model. Removed; comment updated to point at the frameToBatch
  docstring.

### Added

- **`gobi/exec_refcount_test.go`** — CheckedAllocator-backed tests
  that catch refcount imbalance in the streaming executor. Runs
  each op (filter, project, withColumn) plus the full
  `Filter → Select → Collect` lazy pipeline under a checked pool
  and asserts `pool.AssertSize(t, 0)` — any leaked buffer surfaces
  with a stack trace pointing at the site that failed to Release.
  Would have caught the double-Retain the day it was written.

- **`contrib/athenaio/unload_refcount_test.go`** — same pattern for
  the concat portion of readBucketFiles. Two tests: happy path and
  error path (schema mismatch), both under CheckedAllocator.

## [v0.2.21]

### Changed

- **Bumped `github.com/apache/arrow-go/v18` from v18.6.0 to v18.7.0.**
  All tests pass unchanged. Free wins delivered by the upgrade
  (no gobi code change required):
  - `pqarrow.ReadRowGroups` now caps `batchSize` to
    `NextPowerOf2(nrows)`, so small row groups no longer allocate
    buffers sized for the default `Props.BatchSize` — relevant for
    the 50-row-group GeoParquet experiment.
  - `columnChunkReader.Close()` nils `curPage`/`rdr` fields before
    Release, closing a double-release window on error paths.
  - `Data.SetDictionary` releases the old dictionary before
    installing the new one — was leaking on re-set.
  - Comparison kernel bounds fix: arrays smaller than 8 elements
    no longer read past their end.
  - ByteArray statistics min/max are now copied instead of
    aliased, closing a use-after-free.
  - `RecordReader.Read`'s batch is capped to actual row count.
  - `ReserveData(0)` no-alloc — cheap hot-path win.
  - zstd writes get `WithAllLitEntropyCompression(true)` — tighter
    output on all-literal blocks.
  - Parquet writer's magic-header path now returns errors instead
    of panicking (via `NewParquetWriterWithError`, used internally
    by `pqarrow.NewFileWriter`).

  Transitive bumps that came along: `thrift 0.22 → 0.24`,
  `grpc 1.80 → 1.82`, `golang.org/x/net 0.52 → 0.55`,
  `klauspost/compress 1.18.5 → 1.19`, `modernc.org/sqlite
  1.49.1 → 1.53.0` (carrying `modernc.org/libc 1.72 → 1.73.4`).
  None break API.

### Performance

- **`parquetio` now sets `PreAllocBinaryData: true` on
  `pqarrow.ArrowReadProperties`.** New in arrow-go v18.7.0. The
  reader pre-sizes each row group's BinaryBuilder data buffer
  from the column chunk's `TotalUncompressedSize` and `NumRows`
  metadata, eliminating the O(log n) realloc-and-copy cycles that
  dominated WKB geometry column reads. Zero downside for narrow
  columns — the metadata is already fetched during footer parsing.

- **`parquetio` now sets `PageStreamingEnabled: true` on the
  `parquet.ReaderProperties` passed to `file.NewParquetReader`.**
  Also new in v18.7.0. Eligible pages (PLAIN-encoded V1/V2 data
  pages larger than 1 MiB, using UNCOMPRESSED/GZIP/BROTLI/ZSTD)
  are decoded incrementally into a `min(1 MiB, page-size)` rolling
  buffer instead of materializing the whole uncompressed page.
  Pages ≤1 MiB continue on the old whole-page path (streaming
  would only add overhead). Peak memory drops on files with wide
  row groups; decoded values are identical.

## [v0.2.20]

### Fixed

- **`parquetio.ReadFile` and `parquetio.ReadReader` leaked the
  `arrow.Table` from `pqarrow.FileReader.ReadRowGroups`.**
  `frameFromTable` copied Column values into the returned Frame
  without incrementing the underlying `*Chunked` refcount, and
  neither caller ever called `table.Release()` — so the Table's
  refs stayed at 1 indefinitely (arrays never freed) while the
  Frame borrowed the same pointers. On multi-GB parquet reads at
  high call frequency (per-bucket athenaio flows, snap-to-graph
  reads) the pinned arrow buffers accumulated per call.

  Fix: `frameFromTable` now
  Retains each column's Chunked so the Frame owns its own ref,
  and both callers `defer table.Release()` immediately after the
  `ReadRowGroups` call succeeds. Net refcount balance:
  Table.Release drops Table's ownership; Frame retains via the
  explicit `c.Data().Retain()`; Frame.Release eventually decrements
  to zero.

  `ReadFileChunksFunc` and `ReadReaderChunksFunc` are unaffected —
  they route through `frameFromRecord`, which already uses
  `arrow.NewColumnFromArr` (Retains per array internally), and the
  record reader is `defer rr.Release()`'d.

- **`gobi.NewFrameFromTable` had the same shape leak as
  `frameFromTable`.** Copied Column values into the returned Frame
  without Retaining, so callers who Released the source Table
  triggered use-after-free on the Frame's buffers, and callers who
  didn't leaked the Table's refs. Fix: `NewFrameFromTable` now
  Retains each column's Chunked; callers manage the source Table
  independently, as arrow's ownership contract expects.

- **`parquetio.WriteFile` leaked the transient `arrow.Table` from
  `frame.Table()`.** `frame.Table()` produces a Table with each
  column Retained (arrow.NewTable's internal Retain); the previous
  code passed this to `writer.WriteTable(...)` and never Released
  it, so every parquet write pinned one extra ref on every column
  of the source Frame — effectively doubling the source Frame's
  memory footprint until Frame collection ran. Same bug in
  `experiments/gpkg_to_geoparquet/main.go`. Fix: capture the Table
  in a variable, `Release()` on both success and error paths.

- **`Frame.Table()` doc comment was actively wrong.** Claimed
  "releasing one releases the other" — the reality is the two are
  independent ref-holders (NewTable Retains, so Frame and Table
  each Release once). Corrected the docstring so callers stop
  building on the false invariant.

## [v0.2.19]

### Added

- **`gobi.SeriesFromArray(field arrow.Field, arr arrow.Array) Series`** —
  public helper for wrapping a freshly-built arrow.Array in a Series
  with a caller-supplied field. Handles the full ref-count dance
  (Chunked construction + retain balancing + Column construction +
  release of intermediates) so downstream code doesn't have to
  hand-roll the ceremony. Motivating shape: sibling packages
  building custom column types (geometry, hash, ML feature) that
  previously wrote:

      chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
      col := arrow.NewColumn(field, chunked)
      arr.Release()
      chunked.Release()
      return gobi.NewSeries(col)

  Collapse to:

      return gobi.SeriesFromArray(field, arr)

  Same ref-count semantics either way. Callers transfer ownership
  of arr into the function (do NOT `arr.Release()` afterward);
  returned Series owns the underlying buffers via its Column
  reference.

  Internal helpers `arrayToSeries`, `newSeriesFromArray`, and
  `buildSeries` now delegate to this one canonical implementation
  — every ref-count bug fix lives in exactly one place.

## [v0.2.18]

### Fixed

- **Arrow reference-count hygiene audit.** Systematic sweep of every
  `arrow.NewChunked` / `arrow.NewColumn` construction site in gobi
  core + IO codecs (csvio, geojsonio, shpio, kmlio) added the missing
  `chunked.Release()` calls that pair with `NewColumn`'s internal
  Retain. Same story for the intermediate `arr` from `builder.NewArray()`
  when the caller was passing it into `NewChunked` — now Released
  after NewChunked's own Retain runs.

  Under gobi's default `memory.GoAllocator` these were latent
  refcount leaks masked by Go's GC — no observable memory-growth
  regression. Under a CGo-backed allocator (which nothing in the
  codebase uses today but a downstream might), every fix here
  converts an actual per-call byte leak into a clean drop. Ships as
  hygiene regardless: refcount balance is the honest contract the
  Arrow model asks for, and future allocator swaps stop being
  hazardous.

  Fix pattern (applied ~15 sites):

      // Before
      chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
      col := arrow.NewColumn(field, chunked)
      // (chunked leaks its constructor ref)

      // After
      chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
      col := arrow.NewColumn(field, chunked)
      chunked.Release()

  Shared helpers (`arrayToSeries`, `newSeriesFromArray`,
  `buildSeries`) now do both the transferred-array Release and the
  intermediate-chunked Release inline, so every caller through
  those paths inherits the fix without local changes. Direct
  `NewChunked` sites in `groupby.go`, `groupby_aligned.go`,
  `groupby_fast.go`, `join.go`, `sjoin.go`, `frame_ops.go`,
  `exec.go`, `explode.go`, `points.go`, `series_shift.go`,
  `series_geom.go`, `resample.go`, `setops.go`, `unique.go`,
  `lazy.go`, `pivot.go`, `csvio/csvio.go`, `shpio/shp.go`,
  `kmlio/kmlio.go`, `geojsonio/frame.go` all updated in place.

## [v0.2.17]

### Added

- **`(*geometry.RTree).NearestOne(x, y float64) (id int32, ok bool)`** —
  zero-allocation single-nearest fast path. Depth-first descent
  with a running best-so-far distance + bbox pruning; children
  ordered by ascending bbox distance so the tightest bound gets
  found early and prunes remaining siblings hard. No priority
  queue; recursion depth is O(log_M(N)) which for RTreeNodeSize=16
  handles a billion items in ~8 levels.

  Motivating shape: high-frequency single-nearest lookups (snap-
  to-graph, per-point classification, road-network path finding)
  at 1M+ calls per request. `Nearest(x, y, 1)` on the same shape
  was allocating ~8.6 slice-grow heap objects per call from the
  priority queue plumbing.

  Measured on Apple M3 Pro, 100k-item tree × 10k queries per iter:

  | Path                          | ns/query | allocs/query |
  |-------------------------------|---------:|-------------:|
  | `NearestOne`                  |      942 |            0 |
  | `Nearest(x, y, 1)`            |    2,153 |          8.6 |

  2.3× faster wall time; zero allocations vs 4,962 B/query. Callers
  doing single-nearest lookups at scale should migrate — the
  general `Nearest(k)` path remains for k>1.

### Changed

- **`Nearest`'s internal priority queue is now hand-rolled, not
  `container/heap`.** The `heap.Push(pq, entry)` / `heap.Pop()`
  interface API boxed every rtreeQueue struct (24 bytes, doesn't
  fit in an interface word — one heap alloc per push). Replaced
  with a directly-typed `rtreePQ` (min-heap of `[]rtreeQueue`) with
  private `push` / `pop` methods that do the sift-up / sift-down
  in place. Same complexity, no boxing, no per-op alloc beyond the
  slice's own capacity growth. No API change on `Nearest`.

## [v0.2.16]

### Changed (breaking)

- **`geometry.Haversine` and `geometry.Euclidean` now take `Point`
  arguments instead of separate coordinate floats.** Aligns the
  scalar signatures with `HaversineBatch` and `Point.Distance`, so
  callers holding geometry types can pass them directly without
  decomposing into parallel lat/lon slices at every call site.

  Migration:

      // Before
      d, err := geometry.Haversine(a.X, a.Y, b.X, b.Y, geometry.UnitKilometers)
      d, err := geometry.Euclidean(a.X, a.Y, b.X, b.Y, geometry.UnitMeters)

      // After
      d, err := geometry.Haversine(a, b, geometry.UnitKilometers)
      d, err := geometry.Euclidean(a, b, geometry.UnitMeters)

  Semantics unchanged: same math, same Earth-radius constant, same
  unit conversion table. `Point.Z` and `Point.CRSValue` are ignored
  (both functions have always been pure lon/lat / planar math);
  callers who need CRS-aware dispatch should keep using
  `Point.Distance` which picks Haversine vs Euclidean based on the
  Point's CRS.

- **`HaversineExpr` now takes two `PointExpr` values instead of
  four positional `Expr` arguments.** `PointExpr{Lat, Lon Expr}` is
  a named-field wrapper — the two coordinate components can't be
  accidentally swapped at the call site, killing the classic
  lat/lon-vs-lon/lat footgun the old signature enabled.

  Migration:

      // Before
      HaversineExpr(
          Col("lat"), Col("lon"),
          Col("lat").Shift(1).Over("eid"),
          Col("lon").Shift(1).Over("eid"),
          geometry.UnitKilometers,
      )

      // After
      HaversineExpr(
          PointExpr{Lat: Col("lat"), Lon: Col("lon")},
          PointExpr{
              Lat: Col("lat").Shift(1).Over("eid"),
              Lon: Col("lon").Shift(1).Over("eid"),
          },
          geometry.UnitKilometers,
      )

  Internals unchanged: still four Float64 columns under the hood
  with zero-copy `Float64Values()` fast path on single-chunk input
  and hoisted scale constant. `PointExpr` is a value-type wrapper,
  not an ExprNode — it doesn't compose with arithmetic combinators
  (there's no sensible arithmetic on coordinate pairs at the Expr
  layer).

- **`geometry.MetersPerUnit(u Unit) (float64, error)` exported.**
  The scale-factor lookup table (`km → 1000`, `mi → 1609.344`, etc.)
  is now a public helper so callers building their own bulk
  distance kernels can hoist the constant outside a hot loop
  instead of paying a per-call `metersPerUnit` lookup. Formerly the
  unexported `metersPerUnit`; the unexported alias is kept so
  internal call sites don't churn.

### Added

- **`geometry.HaversineBatch(from, to []Point, u Unit) ([]float64, error)`.**
  Bulk-loop-optimized great-circle distance over paired point
  slices. Semantically equivalent to calling scalar `Haversine`
  per row, but the unit conversion + Earth-radius constant +
  degree-to-radian factor are hoisted out of the inner loop, and
  the per-call error-check overhead is amortized across the batch.

  Measured on Apple M3 Pro, N=10k point pairs, warm cache:
  235 µs/op batch vs 307 µs/op scalar-loop — **~30% faster**,
  identical allocation profile (one output `[]float64`). No SIMD;
  the win is entirely scalar loop scaffolding + constant hoisting.

  Signature takes `[]Point` so callers already holding geometry
  types can call it directly without decomposing into parallel
  lat/lon slices. CRS is not consulted (Haversine is a pure lon/lat
  sphere computation — points in a projected CRS give nonsensical
  distances; convert to WGS84 first).

  Returns a flat `[]float64` — the shape a downstream SIMD kernel
  or arrow buffer wants. Groundwork for a future SIMD-vectorized
  trig kernel once Go's `simd` package stabilizes trig support.

## [v0.2.15]

### Added

- **`gpkgio.WriteMany(path, layers ...Layer) error`** — batch
  multi-layer write on a single SQLite connection. Amortizes the
  Open + WAL PRAGMA + application_id/user_version + metadata-table
  scaffolding across the whole batch — the ~1-3ms of per-`WriteFile`
  ceremony collapses to one pass regardless of layer count.

  Semantically equivalent to N sequential `WriteFile` calls: each
  layer's per-layer transaction is preserved (one bad layer doesn't
  roll back siblings), each `Layer.Opts.Replace` is honored
  independently, output files are byte-compatible with the
  loop-of-WriteFile shape (regression-tested against a
  side-by-side fixture).

  Failure model: **first-error-wins**. Layers before the failing
  one stay written; layers after aren't attempted; the returned
  error wraps `WriteMany[<index>] layer %q` for pinpointing.
  Empty layer slice is a legal no-op.

  Refactor along the way: `WriteFile` now delegates to two internal
  helpers (`openWriteDB` + `writeLayerToDB`) that both API entry
  points share. No behavior change on the `WriteFile` side; just
  factors the ceremony out so `WriteMany` can call it once.

## [v0.2.14]

### Added

- **`(*GeoPackage)` scalar-aggregate helpers.** `LayerNames()`,
  `CountRows(layer)`, `SumColumn(layer, col)`, `MeanColumn(...)`,
  `MinColumn(...)`, `MaxColumn(...)`. Each runs a single scalar
  SQL query — constant memory, no WKB decode, no Go-side row
  iteration. Return types match `Series.Sum` / `Mean` / `Min` /
  `Max` conventions: Sum returns 0 on empty (sum of nothing = 0);
  Mean / Min / Max return NaN on empty or all-null (matching the
  existing Series behavior — `math.IsNaN` to check). Integer
  columns promote to Float64.

  Motivating shape: "rank layers by a summary metric, keep top-N,
  drop the rest" patterns previously had to materialize every
  feature just to compute one number per layer. The new helpers
  collapse that inner loop to a single scalar query per layer,
  followed by `RemoveLayer` for the dropped ones. `LayerNames` is
  the string-slice shortcut; `FeatureTables` is still there for
  the richer per-layer struct.

  All aggregate helpers guard against SQLite's "double-quoted
  identifier falls back to string literal" quirk by verifying the
  column exists via `PRAGMA table_info` before running the
  aggregate — a typo yields a clean "column not found" error
  instead of a silent 0.

- **`gpkgio.RemoveLayer(path, layer)` + `(*GeoPackage).RemoveLayer`.**
  Public primitive for dropping a single layer from a GeoPackage:
  the feature table, its RTree shadow, and the gpkg_contents +
  gpkg_geometry_columns metadata rows. Externally-installed
  triggers (GDAL / QGIS) drop automatically via SQLite when the
  feature table goes.

  Previously only the internal `dropLayer` existed, wired into
  `WriteFile(...)` via `WriteOptions.Replace=true`. Callers doing
  in-place filter-and-rewrite or out-of-band cleanup had no way
  to drop a specific layer without also writing a replacement.

  Returns `ErrLayerNotFound` (a package sentinel wrapped via `%w`)
  when the layer isn't in gpkg_contents — distinguishes "already
  gone" from "file not a valid GeoPackage." Callers who want an
  idempotent drop check via `errors.Is(err, gpkgio.ErrLayerNotFound)`.

### Fixed

- **`PartitionMetadata` now survives a Collect → re-lift boundary.**
  Two-part fix; both were needed for the claim to actually reach
  downstream ops.

  1. `LazyFrame.Collect()` propagates the plan's `PartitionMetadata`
     onto the returned Frame via `Frame.WithPartitionMeta`.
     Previously the plan's alignment claim was computed but dropped
     at the Execute boundary. `CollectRaw` (via `collectPlan`)
     already did this; Collect now agrees.
  2. `scanFrameNode.PartitionMetadata()` reads from the wrapped
     Frame instead of returning nil. Comment predated Frames
     carrying metadata — `Frame.partitionMeta` has existed since
     v0.2.0. `frame.Lazy()` now yields a plan whose root scan
     reports whatever claim the frame carries.

  Without (2), (1) is a paper fix: the Frame carried the claim but
  `frame.Lazy()` produced a plan with `PartitionMetadata() = nil`,
  so downstream `Over` / `GroupBy` / `Join` still fell through to
  the general (unaligned) paths. Both pieces landed together.

  Confirmed impact: a workload with 5 `Shift.Over("eid")` × 8
  bucket workers was hitting the general Over path on every wall —
  `collectHashedPartitions` at 14.7 GB flat alloc_space,
  `evalContiguous` (aligned) barely present at 1.4 GB cum. With
  the claim actually flowing, the aligned path fires and the
  general-path hash-partition build drops to near zero.

  Callers who want to strip a frame's claim (rare — mutating the
  frame in ways that invalidate the partitioning) call
  `f.WithPartitionMeta(nil)` before re-lifting.

## [v0.2.11]

### Fixed

- **`materializeExecOp` retained its concatenated Frame for the
  entire `Collect` lifetime.** After the materialize wall streamed
  its output downstream (batch-by-batch via `frameToBatch`, which
  `Retain`s each column it hands off), the op's own `e.out` pointer
  never dropped its reference — arrow ref-counts stayed above zero,
  the full-frame buffers stayed allocated, and every materialize
  wall in the plan pinned an additional full-frame copy in memory
  until the whole `Collect` returned.

  Compounds on plans with multiple `Over` / `Shift.Over` /
  `SortBy` walls: N walls × plan-width × row-count is the pinned
  cost, and with parallel bucket execution it multiplies again by
  the worker count. A real-world workload with ~8 materialize
  points × ~1 GB per pinned frame × 8 parallel bucket workers
  matched a ~58 GB observed peak — exactly the "direct-LazyFrame
  path is worse than the old materialize-then-loop path" surprise.

  Fix: `Next` releases `e.out` on the EOF path (downstream already
  `Retain`ed what it kept, so the drop is safe); `Close` releases
  defensively for cancelled / errored / partially-iterated plans.
  Also releases the intermediate `in` Frame in `materialize`
  immediately after `compute` returns — `NewFrame` retains what
  it keeps, so identity-compute results stay alive via `out` while
  the orphaned concat-frame columns get freed.

## [v0.2.10]

### Added

- **`Expr.Cast(arrow.DataType)` — numeric-to-numeric expression
  conversion.** Widens or narrows numeric columns inside an expression
  tree. Matrix: Float64 ← {Float32, Int64, Int32, Uint64, Uint32};
  Int64 ← {Float64, Float32, Int32, Uint32}; Float32 ← {Float64,
  Int64, Int32}; Int32 ← {Int64, Float64, Float32}; Uint64 ← {Int64,
  Uint32}; Uint32 ← {Uint64, Int64}. Same-type is a no-op (returns
  the input Series unchanged). Narrowing follows Go's numeric
  conversion semantics (truncation, no overflow check). Nulls
  propagate. Unsupported source/target combinations error with
  `ErrExprTypeMismatch`.

  Primary motivating shape: unblocking `If` / `Coalesce` with mixed
  numeric literals — those require exact type match, and `Cast`
  provides the explicit widening step:

      gobi.If(cond,
          gobi.Col("int_col").Cast(arrow.PrimitiveTypes.Float64),
          gobi.Lit(1.5))

- **`AggMedian` — per-group sample median.** Buffers non-null numeric
  values per group, sorts, emits the middle (or the average of the
  two middle values on even-sized groups). Output is Float64. Empty
  groups (or all-null groups) emit null. Memory is proportional to
  group size — no way to compute exact median in bounded space. For
  approximate-quantile workloads the future t-digest aggregator will
  trade exactness for memory. Fluent Expr surface: `Col("v").Median()`
  (composes with `Over(...)` for per-partition medians).

- **`AggMode` — per-group most-frequent value.** Tracks per-value
  counts; emits the value with the highest count. Ties are broken by
  first-seen order (deterministic across runs). Output type follows
  the source column (String, Int64, etc. — not always Float64).
  Fluent Expr surface: `Col("label").Mode()` composes with `Over(...)`.
  Empty or all-null groups emit null. Memory is O(distinct values
  per group).

  Both new aggregators fire on the eager `GroupBy.Agg` path AND the
  streaming aggregate executor (via `aggAccumulator` implementations
  `medianAcc` + `modeAcc` registered in `newAccumulator`). The single-
  primitive-key `aggFast` bailout list gained both kinds — they
  intentionally sit on the general path, since their accumulators
  aren't compatible with the numeric-slice iteration `aggFast` uses.

- **Zero-copy typed value accessors on `Series`.** New public methods
  `Float64Values() ([]float64, bool)`, `Int64Values() ([]int64, bool)`,
  `Float32Values()`, `Int32Values()`, `Uint64Values()`,
  `Uint32Values()`. Each returns a zero-copy view of the underlying
  arrow buffer when the Series is single-chunk and the type matches;
  `(nil, false)` otherwise (multi-chunk or type mismatch — callers
  should either `Rechunk` or fall back to the generic `numericAt`
  walker). Companion to the existing copy accessors
  `Int64s()`/`Float64s()`/etc.

  Motivating use case: users writing their own numeric kernels
  (custom SIMD via cgo alternatives, hand-rolled Go loops, hooks
  into external Go-native numeric libraries) want the raw slice,
  not a per-row `Value(i)` accessor. This surface folds the
  single-chunk assertion into the return so the caller doesn't
  have to type-assert the chunk itself.

- **`Series.HasNulls()` + `Series.NullCount()`.** Cheap null-metadata
  accessors that consult arrow's cached `NullN` without touching the
  bitmap.

- **Bitwise integer expressions: `Expr.BitAnd`, `Expr.BitOr`,
  `Expr.BitXor`.** Distinct from the existing logical `And` / `Or`
  (which operate on Boolean columns). Both operands must be
  integer-typed (Int32/Int64/Uint32/Uint64); mixed-integer or
  float/bool operands error at Type-check with
  `ErrExprTypeMismatch`. Output type is Int64. Composes with
  comparison + cast for flag-unpack patterns:

      gobi.Col("flags").BitAnd(gobi.Lit(int64(1 << 3))).
          Ne(gobi.Lit(int64(0))).
          Cast(arrow.PrimitiveTypes.Int64)

  Runtime dispatches to a new `scalarI64` bitwise path when the
  RHS is a literal, and to the extended `arithI64I64` kernel for
  col-vs-col. Both share the same op switch shape via the new
  `applyI64Op` / `applyI64OpScalar` helpers.

- **`Expr.UnixNano()` — Timestamp → Int64 nanoseconds (unit-
  normalized).** Handles Second / Millisecond / Microsecond /
  Nanosecond source units, always emitting Int64 nanoseconds.
  Distinct from `Cast(Int64)` on a Timestamp — that returns the
  raw underlying value in the source unit; `UnixNano` normalizes.
  Errors at Type-check on non-Timestamp inputs. Composes with
  arithmetic to derive time-delta expressions inline:

      // hours-since-epoch as Float64
      gobi.Col("ts").UnixNano().Cast(arrow.PrimitiveTypes.Float64).
          Div(gobi.Lit(float64(time.Hour)))

- **`Expr.Cast(Int64)` / `Cast(Float64)` now accept Timestamp
  sources.** Emits the raw underlying epoch value in the source
  column's `TimeUnit`. Together with `UnixNano()` gives callers
  both "raw in source unit" and "normalized to nanoseconds" as
  first-class options.

- **`HaversineExpr(lat1, lon1, lat2, lon2 Expr, u geometry.Unit) Expr`
  — great-circle distance between two lon/lat point columns.**
  Composes with `Shift(1).Over(K)` for prev-row coordinate lookups,
  so per-segment ground-track distance is expressible entirely in
  LazyFrame land without a Custom ExprNode:

      speedKMH := gobi.HaversineExpr(
          gobi.Col("lat"), gobi.Col("lon"),
          gobi.Col("lat").Shift(1).Over("eid"),
          gobi.Col("lon").Shift(1).Over("eid"),
          geometry.UnitKilometers,
      ).Div(gobi.Col("delta_hours"))

  All four operands must be Float64. Nulls propagate. Fast path
  uses `Series.Float64Values` for zero-copy access and
  `HasNulls`/`Nulls` to skip null-checks on all-valid inputs; a
  multi-chunk fallback uses `Series.Float64s`. Internally routes to
  `geometry.Haversine` for the scalar math — same accuracy, same
  Earth-radius constant.

### Fixed

- **Arithmetic on integer columns with integer literals now preserves
  Int64 at runtime.** `binOpNode.Type()` reported `Int64` for
  `Int64Col.Add(Lit(int64(1)))` per `promoteNumeric`, but
  `binOpNode.Eval` dispatched to `Series.AddScalar(float64)` which
  unconditionally emitted Float64. The mismatch was latent on
  single-chunk pipelines and surfaced as a `NewColumn: inconsistent
  data type float64 vs int64` panic in `concatBatchesToFrame` the
  moment a multi-chunk source Frame flowed through the executor.

  Fix: `binOpNode.Eval` now routes `(IntCol op IntLit)` (op ∈
  {Add, Sub, Mul, BitAnd, BitOr, BitXor}) through a new
  `tryScalarIntFastPath` that calls the `scalarI64` kernel,
  preserving the source column's Int64 dtype to match `Type()`.
  Div still widens to Float64 per IEEE semantics (matching
  `Series.Div`'s `wantFloat=true`), and `binOpNode.Type()` now
  returns Float64 for Div explicitly so Div-in-schema and
  Div-at-runtime agree.

  `Series.AddScalar` / `SubScalar` / `MulScalar` / `DivScalar`
  public contract is unchanged — they still emit Float64. The
  integer-preserving path lives in the expression layer where the
  literal's original dtype is visible.

- **`scanFrameExec` panicked on multi-chunk input Frames.** The scan
  split Frames into fixed `batchRows`-sized ranges and handed each
  slice to `frameToBatch`, which grabbed `chunks[0]` and paired it
  with `f.NumRows()` in `array.NewRecordBatch`. When the slice
  straddled an underlying chunk boundary, arrow's `NewColumnSlice`
  preserved the multi-chunk structure — `chunks[0]` returned only
  the first sub-chunk while `NumRows()` still reported the whole
  slice, and Arrow panicked with a row-count mismatch.

  Fix: `scanFrameExec` now precomputes chunk-aligned boundaries at
  construction (the union of every column's chunk-end offsets, plus
  `batchRows`-spaced cuts inside any remaining large spans) and
  emits one batch per adjacent pair. Every slice sits within a
  single underlying chunk for every column, so `frameToBatch`'s
  single-chunk assumption always holds. `batchRows` is now a cap,
  not a fixed size — batches shrink when a chunk boundary falls
  before the cap.

  Also added a defensive `panic` in `frameToBatch` naming the
  offending column when the invariant is ever violated in the
  future (previously the failure surfaced as an opaque Arrow
  runtime panic several stack frames away from the cause).

### Performance

- **`Series.Nulls()` bitmap walk (2-4× faster on typical mixed-null
  input).** The `[]bool` mask builder now consults the raw arrow
  validity bitmap directly rather than calling `chunk.IsNull(i)` per
  row (each such call re-derives the chunk offset into the bitmap).
  Chunks with `NullN == 0` skip the walk entirely and leave output
  slots at their zero value — zero cost on the common all-valid
  case. No API change.

## [v0.2.9]

### Performance

- **Vectorized numeric accumulators — 7× faster on single-chunk Sum
  (and Mean / Min / Max).** `sumAcc.Update`, `meanAcc.Update`, and
  `minMaxAcc.Update` grew per-type single-chunk fast paths. When the
  aggregation input is a single-chunk Float64 / Int64 / Float32 /
  Int32 column (the common shape after `concatBatchesToFrame` /
  materialize / most eager Frame ops), the accumulator now
  type-switches once on the chunk and iterates rows with direct
  typed accessors — bypassing the per-row `Series.numericAt` walker
  and its chunk-lookup + type-switch overhead.

  Multi-chunk and non-numeric-shortlist columns fall through to the
  existing generic walker unchanged. No API changes.

  Measured on a 1M-row single-chunk Float64 column via `sumAcc.Update`:

  | Path                          | ns/op | B/op | allocs/op |
  |-------------------------------|------:|-----:|----------:|
  | numericAt walker (multi-chunk) | 7.23ms |    0 |         0 |
  | Vectorized (single-chunk)      | 1.05ms |    0 |         0 |

  85% reduction in wall time on the hot inner loop. Zero-alloc on
  both paths. Benchmarks landed as `BenchmarkSumAcc_VectorizedSingleChunk`
  vs `BenchmarkSumAcc_NumericAtMultiChunk`.

## [v0.2.8] — 2026-07-26

### Changed

- **Streaming batch-transform ops now fuse.** Adjacent chains of
  `filter` / `project` / `withColumn` / `drop` / `rename` / `explode`
  streaming exec ops are coalesced by a post-Compile pass into a
  single `fusedStreamExecOp` that runs one `batch → Frame → all ops
  → Frame → batch` cycle per input batch, instead of the previous
  N cycles (one per op). Non-fusable ops (limit, materialize, scan,
  aggregate, join) are boundaries — fusion stops at them.

  Each fusable exec op now implements the `frameApplier` interface
  (single method: `ApplyToFrame(*Frame) (*Frame, error)`). The fusion
  walker sits at the tail of `Compile` and rewrites the exec tree
  bottom-up. Filter-in-the-middle-of-a-chain short-circuits the
  remaining ops when the running Frame reaches 0 rows and pulls the
  next input batch — matches pre-fusion filter behavior.

### Performance

- **Fused chained streaming ops — 22% fewer allocations.** Measured
  on a 200k × 20-column Float64 input with a
  `WithColumn.WithColumn.WithColumn.Filter` chain:

  | Path     | ns/op | B/op     | allocs/op |
  |----------|------:|---------:|----------:|
  | Unfused  | 7.42ms | 165.9 MB |     4,029 |
  | Fused    | 6.88ms | 165.8 MB |     3,156 |

  Wall-time delta is 7% at 20 columns, scaling with column count
  (each boundary conversion allocates one `arrow.Column` header per
  column). At narrow frames (~2 columns) wall time is roughly flat
  and only the 22% alloc reduction remains. Benchmarks landed as
  `BenchmarkFusion_ChainedOps` vs `BenchmarkFusion_ChainedOpsUnfused`
  (invokes the private `compileNode` bypassing the fusion pass).

## [v0.2.7]

### Added

- **`IncrementalAggregator` interface — streaming custom
  aggregators.** New optional interface (extends `Aggregator`) that
  signals per-batch incremental support:

  ```go
  type IncrementalAggregator interface {
      Aggregator
      Clone() IncrementalAggregator
      Update(col Series, rows []int) error
      Finalize() any
  }
  ```

  Custom aggregators that implement it route through
  `streamingAggregateExec` instead of the materializing fallback —
  each group gets a `Clone()`-produced instance at first-touch, and
  the executor calls `Update` per batch, then `Finalize` once per
  group. Legacy `Aggregator`-only custom Fns continue to route
  through materialize (additive change, non-breaking).

  All shipped `NewStringSetAggregator` / `NewInt64SetAggregator` /
  `NewInt32SetAggregator` / `NewUint64SetAggregator` /
  `NewUint32SetAggregator` factories now implement
  `IncrementalAggregator`. Existing code using them gets streaming
  execution automatically.

### Fixed

- **`Over` in a streaming pipeline no longer splits partitions
  across batch boundaries.** Previously `Col("v").Shift(1).Over("id")`
  (or any Over expression) inside a lazy `WithColumn` / `Filter` /
  `Select` evaluated against each batch's Frame independently — a
  partition with rows in batches 1 and 2 was treated as two disjoint
  partitions, and Shift's "first row of the partition" semantics
  wrongly fired at every batch boundary. Same shape broke scalar-
  aggregate `Sum().Over("id")` (per-batch sum instead of partition-
  wide) and predicates like `Filter(Col("v").Eq(Col("v").MaxAgg().Over("id")))`
  (per-batch max instead of partition max).

  Fix: at Compile time, `withColumnNode` / `filterNode` /
  `projectNode` inspect their expression via a new
  `exprContainsOver` walker; if any `overNode` is present anywhere
  in the tree, they route through `materializeExecOp` instead of
  their streaming counterparts. `Over` needs to see the whole input
  Frame at once for correctness — no way to stream it without
  partition-aware batch splitting, which is not something the
  executor supports.

  Cost: any WithColumn / Filter / Select containing Over now
  materializes its input. In practice this rarely regresses real
  pipelines because upstream operations that produce cross-batch
  partitions (Full/Right joins, sorts) already materialize. The
  fix is strictly a correctness gate — previously wrong-quiet
  became correct-materialized.

  Regression tests: `TestOver_StreamingCrossBatchCorrectness`
  (Shift row-64K boundary), `TestOver_StreamingScalarAggCrossBatch`
  (Sum partition-wide across batches),
  `TestOver_StreamingInFilterPredicate` (Max-based filter).

### Changed

- **`Explode` now runs per-batch in the streaming executor.** The
  `explodeNode` no longer compiles to `materializeExecOp`; it now
  uses a new `explodeExecOp` that calls `Frame.Explode` on each
  input batch independently. Correctness is preserved because
  per-parent-row expansion has no cross-batch dependency. Output
  batches may exceed the batch-size soft cap when dense multi-part
  geometries or long lists arrive — downstream operators handle
  variable batch sizes fine. Segment-branch pipelines like
  `h3CellExprNode → gridPathExprNode → Explode → GroupBy` now
  stream end-to-end (with CollectSet's IncrementalAggregator
  support below).

### Performance

- **Shape-preserving `Over` — projection + aligned slice fast path.**
  Two cuts to `overNode.evalShapePreserving`'s per-partition
  allocation:

  1. **Referenced-column projection.** `Over` now inspects the
     inner expression's referenced columns via
     `referencedColumns(...)` and `SelectCols`-narrows the input
     Frame before the per-partition loop. On a 22-column input
     where the inner reads only 1 column (typical
     `Col("v").Shift(1).Over("id")` shape), per-partition
     mini-Frame construction now copies just the referenced
     columns instead of all 22.

  2. **Aligned slice.** When `PartitionMetadata` proves aligned
     + sorted contiguity, `evalShapePreservingAligned` replaces
     the per-partition `take` (O(N) copy) with `Frame.slice`
     (zero-copy ref-count). Partitions become Frame views, not
     Frame copies.

  Measured on 10k rows / 100 partitions / 20 payload columns,
  eager `Col("v").Shift(1).Over("id")`:

  | Path                    | ns/op | B/op    | allocs/op |
  |-------------------------|------:|--------:|----------:|
  | General (w/ projection) | 745µs | 1.62 MB |    23,815 |
  | Aligned + slice         | 498µs | 0.98 MB |    12,097 |

  33% faster, 40% smaller footprint, 49% fewer allocations on the
  aligned path. Benchmarks landed as
  `BenchmarkOver_ShapePreserving_WideFrame` vs
  `BenchmarkOver_ShapePreserving_AlignedSlice`.
- **`Explode` streaming — 12.5% faster, 19% fewer allocations.**
  Measured on 10k rows × 3 list-element = 30k exploded rows,
  streaming (`explodeExecOp`) vs the previous materialize path:

  | Path                   | ns/op | B/op    | allocs/op |
  |------------------------|------:|--------:|----------:|
  | Materialize (baseline) | 456µs | 3.12 MB |       180 |
  | Streaming (new)        | 399µs | 2.75 MB |       146 |

  Modest per-op delta because `takeArray` inside `Frame.Explode`
  dominates the cost — both paths pay it. What streaming removes is
  the pre-Explode `concatBatchesToFrame` copy plus the
  post-Explode output-batch slicing. On larger fixtures the
  qualitative win compounds: downstream operators start consuming
  exploded batches immediately instead of after a full buffered
  materialize. Benchmarks landed as `BenchmarkExplode_Streaming`
  vs `BenchmarkExplode_Materialize`.
- **collect-set aggregation now streams — 75% faster, 98.7% fewer
  allocations.** Measured on 100k rows / 100 groups with
  `NewStringSetAggregator`:

  | Path                    | ns/op | B/op | allocs/op |
  |-------------------------|------:|-----:|----------:|
  | Materialize (baseline)  | 7.64ms | 14.16 MB | 302,873 |
  | Streaming (Incremental) | 1.92ms |  9.60 MB |   4,043 |

  The allocation count reduction (~75× fewer) comes from removing
  the `concatBatchesToFrame` copy inside `materializeExecOp` —
  every input batch used to be concatenated into one giant Frame
  before the aggregator ran on it; now each batch's `Update` runs
  in place. Benchmarks landed as `BenchmarkAggregator_CollectSet_Materialize`
  vs `BenchmarkAggregator_CollectSet_Streaming` — same fixture,
  only difference is whether the aggregator hides its
  `IncrementalAggregator` methods.

## [v0.2.6] — 2026-07-25

### Fixed

- **`Frame.Join` now carries `List<T>` columns across.** Previously
  any join whose payload included a `List<T>` (String / Int64 /
  Uint64 / ...) errored with
  `ErrColumnTypeMismatch: join not implemented for list<...>` (for
  Left/Right/Full) or `take not implemented for %T` (for Inner). The
  three take-family helpers — `takeArrayFast` (single-chunk),
  `takeArraySlow` (multi-chunk), and `takeArrayWithNulls` (unmatched-
  side null-emit for outer joins) — gained a `LIST` case that copies
  each row via a new shared `appendListRowFromArray` helper. Null
  lists are preserved; `-1` unmatched-side indexes emit a null list.
  Also fixes any pipeline that Explodes a Frame with a companion
  `List<T>` column (Explode's `takeArray` call now works too).

  Nested `List<List<T>>` and `List<Struct<...>>` fall through
  `appendArrayValueAt`'s primitive-only element dispatch and surface
  a clear error — noted as a follow-up.

## [v0.2.5] — 2026-07-25

### Added

- **`gobi.Coalesce(exprs ...Expr) — SQL-style first-non-null.`**
  Variadic. Per row, returns the first operand whose value is non-
  null; if every operand is null at a given row, the output is null.
  All operands must produce the same arrow type — no automatic
  widening; cast explicitly or match `Lit` types. Zero operands
  errors; a single operand is a passthrough. Not short-circuit: every
  operand is evaluated in full (`gobi.If` splits pipelines if that
  matters). Supports primitives + `List<T>` (list values are copied
  per-row via `copyRowValue`, which walks the offsets and reuses
  `appendArrayValueAt` for elements). Polars parity: `coalesce()`.
- **`gobi.LitEmptyList(elemType arrow.DataType)` — empty-list
  literal.** Companion to `LitNull` for the "coalesce list-null to
  empty-list" pattern. Broadcasts a non-null zero-length list of the
  given element type to every input row. Distinct from
  `LitNull(ListOf(elemType))`, which produces null-list rows —
  `LitEmptyList` produces non-null empty rows. Enables the full-
  outer-join + `ListUnion` shape without a Go-side merge:

  ```go
  segSafe  := gobi.Coalesce(gobi.Col("seg_providers"),  gobi.LitEmptyList(arrow.BinaryTypes.String))
  pingSafe := gobi.Coalesce(gobi.Col("ping_providers"), gobi.LitEmptyList(arrow.BinaryTypes.String))
  merged   := segSafe.ListUnion(pingSafe)
  ```

## [v0.2.4]

### Added

- **`gobi.LitNull(dtype)` — typed-null literal.** Broadcasts a
  null of the given arrow type to every input row. Complements
  `Lit(v)` for the case where the value is null but a specific
  type is required (e.g. filling a `List<String>`-shaped column on
  one branch of a union). Works with any type `builderForType`
  supports (all primitives, List, Struct).
- **`LazyFrame.SelectCols(names ...string)` + `Frame.SelectCols(names ...string)`.**
  Name-based Select shorthand — equivalent to
  `Select(Col(n0), Col(n1), ...)` on the lazy side, a fresh Frame
  with reordered columns on the eager side. Handles column
  reordering as a side effect (output order = argument order).
  Missing columns error with `ErrColumnNotFound`.
- **`LazyFrame.Rename(old, new)` + `Frame.Rename(old, new)`.**
  Single-op column rename that preserves column position and
  buffers (schema field's Name changes; arrow arrays are shared
  via ref-count). `LazyFrame.Rename` compiles to a streaming
  `renameExecOp` — per-batch relabel, no buffering. `renameNode`
  in the plan tree carries partition metadata across the rename:
  if the renamed column is a partition column, `PartitionMetadata.Columns`
  and `SortedBy` entries are rewritten to the new name so alignment
  proofs stay valid. Same-name rename (old == new) is a no-op that
  returns the receiver.
- **Filtered aggregation — `Aggregation.Filter Expr` field.** Optional
  per-agg predicate; when set, only rows where the filter evaluates
  to TRUE (non-null) participate in that aggregation. Different aggs
  in the same `Agg` call may carry different filters — each is
  applied independently. SQL FILTER (WHERE cond) semantics: null cond
  → row excluded. Polars parity:
  `pl.col("x").sum().filter(pl.col("source") == "seg")`.

  Filtered aggregations route through the eager `GroupBy.Agg` path
  today. `allBuiltInAggs` treats a Filter-having agg as "not built-
  in", so `LazyFrame.Collect()` falls back to the materializing
  executor — the streaming hash-aggregate hot path stays unchanged
  for filter-free workloads. Both the general and aligned linear-
  scan paths honor filters via a precomputed per-agg `[]bool` mask.
  `aggFast` (single-primitive-key path) bails when any agg has a
  filter — falls through to the general path.
- **Typed row extraction on `Series`.** Ergonomic accessors that
  copy a Series' values into a fresh `[]T` Go slice without needing
  to walk arrow chunks by hand:
  - `Series.Int64s()` / `Int32s()` / `Uint64s()` / `Uint32s()`
  - `Series.Float64s()` / `Float32s()`
  - `Series.Strings()` (String + LargeString)
  - `Series.Bools()`
  - `Series.Timestamps()` (returns `[]arrow.Timestamp`)
  - `Series.Nulls()` — parallel `[]bool` mask (true = null); pair
    with any value extractor to distinguish arrow-null from a
    zero-valued row.

  Errors on arrow-type mismatch (`ErrColumnTypeMismatch`). Returned
  slices own their memory — safe to mutate, safe to hold past the
  source Frame's `Release`. Walks multi-chunk sources internally.
  Sits between the raw arrow chunk API (fast but needs walking +
  type-asserting each Series) and `ToStructs[T]` (which needs a
  matching Go struct).
- **`Expr.ListUnion(other)` — per-row deduplicated list union.**
  Both sides must be `List<T>` with the same element type; row `i`'s
  output is the union of left[i] and right[i] with duplicates removed,
  preserving first-seen order (left elements first, then any new
  elements from right). Nulls inside a list are skipped; a null list
  itself propagates (null on either side → null output). Enables the
  "join two aggregated sets and combine them" pattern in-plan —
  combines with `NewStringSetAggregator` / `NewInt64SetAggregator`
  outputs to produce cross-branch distinct sets without a Go-side
  merge step. Polars `list.set_union` / Spark `array_union` parity.
- **`gobi.If(cond, a, b)` — SQL CASE-WHEN expression.** Package-level
  constructor that evaluates to `a` where `cond` is true, `b` where
  false, and null where `cond` itself is null (SQL semantics — users
  can wrap `cond` in `.IsNotNull().And(cond)` to force false-on-null).
  `cond` must be Boolean; `a` and `b` must have identical arrow
  output types — no automatic numeric widening in v1 (cast explicitly
  or match Lit types). Chains for nested else-if:

  ```go
  gobi.If(cond1, valA,
      gobi.If(cond2, valB, valC))
  ```

  Composes with `IsNull` / `IsNotNull` for mean-fill / null-fallback
  patterns. Not short-circuit — all three subtrees are evaluated in
  full; split into filtered pipelines if that matters.
- **Built-in distinct-set aggregators — `List<T>` collect_set.**
  Ready-to-use `Aggregator` factories for the common "give me the set
  of distinct X per group" pattern, matching polars `.list.unique()`
  / Spark `collect_set(x)`:
  - `NewStringSetAggregator()` → `List<String>`
  - `NewInt64SetAggregator()`  → `List<Int64>`
  - `NewInt32SetAggregator()`  → `List<Int32>`
  - `NewUint64SetAggregator()` → `List<Uint64>` (h3 cell-id shape)
  - `NewUint32SetAggregator()` → `List<Uint32>`

  Passed via `Aggregation{Column: "x", Fn: gobi.NewStringSetAggregator(), Alias: "..."}`.
  Per-group output is deduplicated and sorted (stable, equality-
  friendly). Nulls are skipped. `Merge` combines peer sets as a union
  (used by future parallel/window paths). All variants share a
  generic `setAggregator[T]` under the hood; adding a new type is a
  ~15-line constructor. Adding this ships the aggregator layer that
  `appendCustomListValue`'s typed-slice dispatch (v0.2.1) was
  originally built to support.

## [v0.2.3] — 2026-07-25

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
entry points.

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

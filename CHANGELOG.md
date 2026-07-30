# Changelog

All notable changes to gobi are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org). Pre-1.0 minor versions may
introduce breaking changes; check this file when upgrading.

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

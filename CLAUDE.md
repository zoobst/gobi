# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What gobi is

A geospatial dataframe library for Go — "GeoPandas-shaped API with Polars-shaped internals." Built on Apache Arrow (`github.com/apache/arrow-go/v18`, the successor to `github.com/apache/arrow/go/v18`). Requires Go 1.26+.

## Hard constraint: pure Go, no cgo

No GDAL, no GEOS, no libproj, no SQLite (except via `modernc.org/sqlite`, the pure-Go port). Anything that requires C bindings is off the table — the design intentionally trades feature breadth for cross-compilation simplicity. This constraint drives many decisions (custom WKB codec, hand-rolled UTM / Web Mercator reprojection via Snyder / Redfearn, pure-Go STR R-tree). When adding features, do not reach for cgo — find a pure-Go path or say the feature isn't in scope.

## Hard constraint: no disk spilling

The executor and all future aggregation/sort/join implementations do **not** spill to disk. If a working set doesn't fit in RAM, the process OOMs — that's the acceptable failure mode. Speed is the priority; disk is orders of magnitude slower than RAM and spill code is a maintenance pit (buffer management, eviction, serialization, temp-dir cleanup, ~10× perf cliff surprises). Every DataFrame engine that ships spill regrets the complexity; we're not going there.

**What's allowed instead:**
- Approximate aggregators (HyperLogLog for count-distinct, t-digest for quantile) — cap memory algorithmically without touching disk.
- Refusing to execute when parquet stats prove the working set won't fit (a future feature, requires cost estimation).
- Documenting the RAM ceiling honestly; suggesting DuckDB/Spark for genuinely big data.

Do not propose spill-to-disk in roadmap discussions.

## Commands

```bash
# Build everything.
go build ./...

# Full test suite. -race is what CI runs; run it locally before pushing.
go test -race ./...

# Single package, verbose.
go test -race -v ./parquetio/

# Single test in a package.
go test -race -run TestLazy_Filter ./...

# Vet must pass.
go vet ./...

# Module hygiene.
go mod tidy && git diff --exit-code go.mod go.sum

# Benchmarks (fixture files live under benchmarks/ — see that directory's *.go).
go test -run=^$ -bench=BenchmarkReadFile_1M -benchtime=10x ./parquetio/...
```

CI is defined in `.github/workflows/ci.yml`: `go mod tidy` + `go vet` + `go build` + `go test -race -coverprofile`. Runs on ubuntu-latest and macos-latest.

## Architecture

### Core types (in the top-level `gobi` package)

- **`Frame`** — schema + `[]Series`. Immutable-by-convention: mutating methods (`Filter`, `WithColumn`, `SortBy`, `DropColumn`) return a new `*Frame`. The Series inside share Arrow buffers with the parent; `Retain`/`Release` on the Frame propagate to underlying `*arrow.Column`s.
- **`Series`** — a named `*arrow.Column`. **By-value type** (3-word struct) — `df.Column("x")` is cheap. Series does NOT own its column: the buffers are shared with the parent Frame; that's why `Retain`/`Release` matter for streaming callback flows.
- **`Expr` + `ExprNode`** — expression IR (data, not code). `expr.go` has the public wrapper + fluent combinators (`.Add`, `.Gt`, `.And`); `expr_eval.go` has the internal node types (`colRefNode`, `literalNode`, `binOpNode`, etc.) and evaluation. Users extend via `gobi.Custom(node ExprNode)`.

### Query plan / lazy execution

The library has both an eager path (`Frame.Filter` etc., materializing at each step) and a lazy path (`LazyFrame`, plan tree + optimizer). Both live in the same package.

- **`plan.go`** — `LogicalPlan` interface + node types: `scanFrameNode`, `scanFileNode`, `filterNode`, `projectNode`, `withColumnNode`, `dropNode`, `sortNode`, `aggregateNode`, `joinNode`, `limitNode`, `tailNode`, `emptyNode`. Each node knows its Schema.
- **`lazy.go`** — `LazyFrame` fluent API + `Collect()` walker that dispatches each plan node to the eager engine (`Frame.FilterExpr`, `Frame.SortBy`, `GroupBy.Agg`, etc.). `CollectRaw()` bypasses the optimizer.
- **`optimize.go`** — Rule interface + fixed-point loop + the current rule set: `FoldConstants`, `RemoveTrivialFilter` (handles both `Lit(true)` and `Lit(false)` → `emptyNode`), `CombineFilters`, `PushFilterBelowProject`, `PushFilterBelowSort`, `ProjectionPushdown`, `PushPredicateToScan`, `CascadeEmpty`. Two shared walkers: `mapExprs` (rewrites every Expr in every node) and `walkRewrite` (bottom-up structural rewrites).
- **`predicate_stats.go`** — `Stats` interface + `CanPossiblyMatch(pred, stats)`. Used by source packages (parquetio) to prune row-groups via footer statistics. 3-valued: true = "possibly matches", false = "definitely can't match (safe to prune)".

### Extension seams

Three interfaces let external packages plug into gobi without touching core:

- **`Namer`** — expression nodes that carry an output column name (`Col`, `Alias`, custom H3-style nodes). Consumed by `Select` for auto-naming.
- **`ProjectableScan`** — scan nodes that accept column-projection hints. `parquetio.ScanFile` implements this via a `WithColumnProjection` callback registered on `NewScanNode`.
- **`PredicateSink`** — scan nodes that accept predicate hints for row-group / bloom-filter skipping. Same pattern via `WithPredicatePushdown`. **`nil` return from either callback = "no change"** — used by parquetio to signal "user set ReadOptions.Columns explicitly, don't override."

### Format subpackages

Each format has its own subpackage that depends on the core `gobi` package (never the reverse — that circular direction is deliberately blocked, which is why scan constructors like `parquetio.ScanFile` live in the format package, not in gobi core).

- **`csvio`** — typed CSV via `arrowcsv.NewReader` + struct tags (`csv:"col"`, `geom:"true"`, `time:"layout"`). `Read[T]` / `ReadFile[T]` / `ReadChunksFunc[T]` / `ReadFileChunksFunc[T]`. Streaming callback API for bounded-memory ETL.
- **`parquetio`** — `ReadFile` / `ReadFileChunksFunc` / `ScanFile` + `WriteFile` with `WriteOptions{Codec, RowGroupRows, BloomFilterColumns, BloomFilterFPP}`. `ReadOptions.Columns` for projection, `ReadOptions.Predicate` for row-group skipping. `ScanFile` is the LazyFrame entry point.
- **`geometry`** — `Point`, `LineString`, `Polygon`, `MultiPoint`, `MultiLineString`, `MultiPolygon`, `GeometryCollection`. 2D + optional XYZ. Own WKB/WKT codec. UTM + Web Mercator reprojection via Snyder / Redfearn formulas. Static STR R-tree for spatial join.
- **`geojsonio`** — RFC 7946 encoder/decoder for every geometry type (Point, LineString, Polygon, MultiPoint, MultiLineString, MultiPolygon, GeometryCollection) with optional XYZ. `ReadFile` / `ReadFileChunksFunc` / `ScanFile` + `WriteFile` at Frame level; `Marshal` / `Unmarshal` at geometry level. Auto-detects `.geojsonl` / `.ndjson` line-delimited format.
- **`gpkgio`** — OGC GeoPackage 1.3 read/write via `modernc.org/sqlite` (pure Go). `WriteFile` emits spec-compliant metadata + RTree spatial index; `ReadFile` / `ReadFileChunksFunc` / `ScanFile` mirror parquetio's entry points. `ScanFile` supports predicate pushdown via `gobi.ExprToSQL`. RTree is populated directly from Go, not via SpatiaLite triggers (they'd need `ST_MinX`/`ST_MaxX` which pure-Go SQLite lacks). Metadata DDL lives in `schema/init_metadata.sql`, embedded via `//go:embed`.
- **`pgio`** — PostgreSQL / PostGIS via `jackc/pgx/v5` in native mode (not the `database/sql` wrapper). `ReadQuery` / `ReadTable` / `ScanTable` on the read side; `WriteTable` uses `pgx.CopyFrom` for 10-100× bulk-insert throughput vs naive INSERT loops. Native mode chosen deliberately for CopyFrom access + PostGIS OID handling via pgtype. Geometry columns wrapped in `ST_AsEWKB` on read to preserve SRID; `pgio.ScanTable` supports predicate pushdown via `gobi.ExprToSQL` with `?`→`$N` placeholder rewriting.
- **`kmlio`** — KML (OGC 12-007r2) + KMZ (zipped KML) read/write. `ReadFile` / `WriteFile` auto-detect the format from the file extension (`.kmz` → zip archive with a `doc.kml` entry). `Read` / `Write` from raw io.Reader/Writer default to KML but accept `Format: FormatKMZ` for explicit override. KMZ archives are read via `archive/zip` — the reader prefers `doc.kml` and falls back to the first `.kml` entry so KMZs from other tools still parse.
- **`shpio`** — Shapefile read/write. Empty `ReadOptions` / `WriteOptions` stub structs so future config additions don't force breaking signature changes.

### IO options: naming conventions

Every IO subpackage exposes `ReadOptions` and `WriteOptions` structs. Field names are aligned across packages where the semantics overlap; the intentional exceptions below are worth remembering because they look like inconsistencies but aren't.

**Aligned fields (same name across packages that need them):**

- `Columns []string` — projection at read time. Present in parquetio, gpkgio, geojsonio, pgio ReadOptions. csvio's schema comes from struct tags so it doesn't need this.
- `Limit int64` — row cap at read time. Present in gpkgio, pgio (formats that can push it to the source). Not on parquetio/csvio/geojsonio because those have no useful early-termination beyond the streaming exit.
- `ChunkRows int` — target batch size for the streaming reader. csvio, parquetio, geojsonio. gpkgio + pgio don't expose this yet.
- `Allocator memory.Allocator` — arrow allocator override. csvio, parquetio, gpkgio, geojsonio, pgio ReadOptions. nil = memory.DefaultAllocator via the package's local `resolveAllocator` helper.
- `GeomCol string` — geometry column name for writes. gpkgio, geojsonio, pgio WriteOptions.
- `SRID int32` — spatial reference identifier for writes. gpkgio, pgio WriteOptions. (The GeoPackage SQL column is still named `srs_id` internally — only the Go field is aligned.)

**Deliberate non-alignments (semantic distinction, do not "fix"):**

- **`Predicate gobi.Expr` (parquetio) vs `Where string` + `WhereArgs []any` (gpkgio, pgio).** Different by design. parquetio's `Predicate` is a typed gobi.Expr evaluated client-side against parquet footer statistics (min/max, bloom filters) to skip row-groups — gobi understands the expression. gpkgio + pgio's `Where` is a raw SQL fragment shipped verbatim to the driver — the DBMS evaluates it. Merging the names would suggest a coupling that doesn't exist.
- **`Compression Codec` (csvio) vs `Codec Codec` (parquetio WriteOptions).** Different concepts, same Go type. csvio's Compression is the outer file-wrapper (gzip/zstd/bzip2 of the CSV stream). parquetio's Codec is the parquet page compression codec (snappy/gzip/brotli/zstd/lz4) applied per column chunk inside the parquet container.
- **`Schema string` (pgio) vs `Layer string` (gpkgio).** Both scope a table but they name different concepts — PostgreSQL schemas are namespaces; GeoPackage layers are top-level tables. Neither term is right for the other.

**Format-specific fields (leave alone):** csvio's tokenizer knobs (`Delimiter`, `Comment`, `HasHeader`, `SkipRows`, `LazyQuotes`, `UseCRLF`, `NullTokens`, `CRSHint`), parquetio's `RowGroups` / `ScanWorkers` / `BloomFilterColumns` / `BloomFilterFPP` / `RowGroupRows`, gpkgio's `Layer` / `Replace` / `BatchSize` / `SkipRTree`, geojsonio's `Format` / `Indent`, pgio's `Schema` / `Truncate` / `GeomColumns`. These name format-specific behaviors and don't map to peers.

### Query planner layer status

Recent work has been building out a Polars-shaped lazy engine. The current state (updated when things change):

- **Layer 1** (Expression IR): done. `gobi.Col`, `gobi.Lit`, `gobi.Custom`, fluent combinators, `Frame.FilterExpr`, `Frame.WithColumnExpr`.
- **Layer 2-3** (Logical plan + LazyFrame API): done. All operators covered.
- **Layer 4** (Rule-based optimizer): done. Nine rules across three slices — FoldConstants, RemoveTrivialFilter, CombineFilters, PushFilterBelowProject, PushFilterBelowSort, ProjectionPushdown, PushPredicateToScan, CascadeEmpty.
- **Layer 5** (Physical plan translation): folded into Layer 6 — `Compile` is the physical translator. `LazyFrame.ExplainPhysical()` shows what strategy each node gets.
- **Layer 6** (Vectorized executor): three slices landed. Streaming for Filter/Project/WithColumn/Drop/Limit/ScanFrame/ScanFile; native streaming aggregate for built-in Kinds; native streaming hash join for Inner/Left/Semi/Anti (Right/Full still materialize).
- **Slice B** (data-parallel scan): landed. `parquetio.ReadOptions.ScanWorkers` + `WithParallelStreamReads` scan option + `parallelScanFileExec`. Partitions row-groups across N workers, fan-in via bounded channel.
- **Slice D** (partitioned parallel aggregate): landed. `streamingAggregateExec.workers` set at Compile via `resolveWorkers()`. Fan-out reader hash-partitions rows by `fnv1a(compositeKey) mod N` — no cross-worker key overlap, so the merge is a union with no value-level combine.
- **Composite-key allocation cuts:** reusable `[]byte` scratch buffers (`keyScratch` for serial, `dispatchScratch` for parallel reader, worker-local for each worker); `map[string(scratch)]` compiler-optimized probe; `map[*aggGroup][]int` bucketing to avoid map-write string allocations; `keyOfAppend` / `composeCompositeKeyInto` append-into-scratch variants of the existing eager helpers. Cut per-row aggregate alloc count from ~2/row to ~0 (413 first-touch heap copies total on 1BRC, i.e. one per unique group).
- **Single-string-key fast path:** compile-time detection via `pickKeyMode` when the aggregate has exactly one String/LargeString key. Skips composite byte encoding entirely — reads `arr.Value(row)` (already zero-copy in arrow-go via `unsafe.Pointer`-cast string), uses it directly as the map key on probe, `strings.Clone` on first-touch insert. Selected via `keyMode` field on `streamingAggregateExec`. Parallel dispatch hashes arrow strings directly via `fnvHashString1`.

Executor still has per-core throughput headroom (1BRC on measurements.snappy.parquet is 15.5s vs polars streaming's ~3s — ~5× gap). See "Future work" section below. **Vectorized numeric kernels are intentionally deferred until Go 1.27's `simd`/`simd/archsimd` packages ship arm64 support (August 2026).** The scaffolding exists at `//go:build goexperiment.simd` in `series_ops_simd_*.go` — rewrite against the portable package when it lands.

### Parallelism history + what's still off the table

**What landed (Slices B + D + key-alloc cleanup):** 1BRC wall time went from 1m53s → 15.5s across the three passes. Peak RSS ended at ~1.3 GB (up from 156 MB — the growth is per-worker parquet reader state, not the aggregate itself). Effective parallelism on 11 cores is ~83%.

- **Slice B (data-parallel scan)** lives in `parquetio/scan_parallel.go` + `exec_scan_parallel.go`. `partitionRowGroups(path, opts)` peeks at the parquet footer to count row-groups, splits them into N contiguous ranges, and returns N read closures. `parallelScanFileExec` runs each closure in its own goroutine, fan-in via a bounded channel. Compile picks parallel-vs-serial by asking `WithParallelStreamReads` for sub-callbacks (>1 → parallel). Applies to any parquet source with more than one row-group.
- **Slice D (partitioned parallel aggregate)** lives in `exec_aggregate_parallel.go`. Reader hash-partitions rows by `fnvHashBytes(compositeKey) mod N` (or `fnvHashString1` on the single-string fast path). Workers have disjoint key sets by construction — merge is a set union with no value-level combine. `streamingAggregateExec.workers` is set at Compile time via `resolveWorkers()`.
- **Composite-key allocation cuts** are in `exec_aggregate.go` + `exec_aggregate_parallel.go` + `groupby.go`. `keyOfAppend` / `composeCompositeKeyInto` append into a reusable scratch. `map[*aggGroup][]int` bucketing avoids the map-write string alloc that the compiler doesn't optimize. Single-string-key fast path (`keyModeString1`) skips composite encoding entirely.

**Explicitly still out of scope:**
- **Pipeline parallelism** (goroutine-per-operator with channels) — high complexity, marginal wins on top of B+D.
- **Task parallelism** (concurrent independent subtrees) — narrow applicability.
- **Morsel-driven scheduler** (DuckDB-style work-stealing pool) — whole other project.
- **Streaming Right/Full joins** — need a second-phase pass emitting unmatched right rows.
- **Streaming Sort** — impossible without disk spill, which the no-disk-spill rule forbids.
- **Hand-rolled SIMD kernels** — deliberately waiting on Go 1.27's stdlib `simd`/`simd/archsimd` packages (arm64 support ships August 2026). The `//go:build goexperiment.simd` scaffolding in `series_ops_simd_*.go` gets rewritten against the portable package when the toolchain support lands. Not writing throwaway `.s` files in the meantime.

**Candidates when the toolchain is ready (not scheduled):**
- **Vectorized accumulator kernels.** Today `minMaxAcc.Update` calls `Series.numericAt(row)` per row → interface dispatch + type switch per row. Rewriting to take `(col Series, rows []int)` and read arrow buffers as typed slices (`[]float64`) would cut the ~30s cumulative CPU visible on the 1BRC profile. Doesn't need SIMD to be a win — the Go compiler + cache locality alone should get 3-5×. SIMD on top gets the last factor.
- **Pooled decoder buffers.** See the "Future work" section below.
- **Additional single-key fast paths.** `pickKeyMode` currently handles `keyModeString1`; add `keyModeInt641` for single int64/int32/uint64/uint32/timestamp/bool keys (direct `map[int64]*aggGroup`, skip encoding + string hashing entirely). Modest — 1BRC-style workloads are usually string-keyed — but cheap to add.

### Future work: pooled decoder buffers (arena, un-sliced)

After Slice B landed, `-workers=11` on 1BRC drove RSS from 156 MB → 1.3 GB. Investigation traced most of that growth to per-worker parquet reader state: each of N parallel workers opens its own `pqarrow.FileReader`, which owns per-column `serializedPageReader` state, each of which owns three `*memory.Buffer`s (`decompressBuffer`, `dataPageBuffer`, `dictPageBuffer`) sized to the largest page they've seen. 11 workers × 2 columns × 3 buffers × page-size = the bulk of the RSS growth.

**Not yet scoped as a slice.** The motivating win (~700-900 MB RSS reduction on 1BRC-like workloads) hasn't been measured against an actual heap profile yet; before committing engineering time, capture a `pprof` alloc profile of the `-workers=11` run and confirm the buffers dominate. The arrow-array output side (Float64Builder, LargeStringBuilder) may also account for a large fraction — those drain as batches complete but peak-at-a-time is bounded by in-flight batches × workers.

**Path forward (if profile confirms):**

Custom `memory.Allocator` wrapping the default, backed by a process-scope `sync.Pool[[]byte]` bucketed by size class (1 KB, 4 KB, 16 KB, 64 KB, 256 KB, 1 MB). Passed to `pqarrow.NewFileReader(pf, props, ourAllocator)`. When arrow-go asks for a resizable buffer, it gets one whose Bytes() is backed by a pooled slice; on `Buffer.Release()` the slice returns to the pool.

- **No arrow-go patch required.** The `memory.Allocator` interface is `Allocate(size int) []byte` + `Reallocate` + `Free`. We control what we return.
- **Analogous to the `sharedZstdCodec` pattern in `laforge/icebergETL/writer.go`:** one process-scope pool feeding all readers, sized to peak concurrent decode calls rather than total open readers.
- **Snappy itself is stateless** in arrow-go's wrapper (`snappyCodec.Decode(dst, src)` — no encoder/decoder object to pool). So the pool is for the decompression **output** buffers, not for codec state.

**Risks / open questions:**

- Buffer size churn: if page sizes vary wildly, pool churn could hurt more than help. Size-class bucketing mitigates but adds complexity. Verify page-size distribution from a real workload first.
- Correctness under `Release()`: arrow-go's `memory.Buffer` has a refcount. Our allocator must return byte slices that stay valid until the last `Release()`. Simplest correct impl: allocator's `Free(b)` returns `b` to pool immediately; buffers with outstanding refs never call Free until the last Release, so the pool never gets a live buffer. Verify against `-race`.
- Doesn't help the arrow-array output side. That's a separate problem (batch-level allocator reuse).

**Not urgent:** current 1.3 GB RSS on 1BRC is well within RAM on any dev machine; the no-disk-spill rule holds. Do this if we see the working set start to bite in real deployments, or if Slice D/other per-core work leaves this as the last obvious source of drag.

### v0.2.0 plan: list columns, Aggregator.Merge, Over(partition)

Three tracks landing in order. Each block below records the shape of the work and the design commitments already made so a future session can pick up mid-track without re-litigating.

**Track 1 — List-typed columns (Level 3).** Full support for `arrow.ListType` columns end-to-end: plumbing, construction from struct slices, explode, list-shaped expression ops, per-element aggregations.

- **Phase 1a — arrow-go plumbing (in progress).** `builderForType` in `groupby.go` handles `arrow.LIST` (returns `array.NewListBuilder(pool, lt.Elem())`); INT8/16 + UINT8/16 also landed as drive-bys. Smoke tests in `list_column_test.go` (Frame carrying `List<String>`, `builderForType` returning `*array.ListBuilder`). Everywhere else that switches on `arrow.Type.ID()` should be audited before declaring 1a done — grep for `case arrow.INT64` / `case arrow.STRING` to find the switches that need a LIST arm (or an explicit unsupported-error with a good message).
- **Phase 1b — FromStructs/ToStructs slice fields → List columns.** `from_structs.go` currently rejects slice fields. Add: on `FromStructs`, a `reflect.Slice` field (except `[]byte`, which stays Binary) builds a List column with element type derived from the slice element. Nested pointer/nullable semantics apply to elements. On `ToStructs`, a List column round-trips into `[]T`. Nested struct-in-slice is out of scope for 0.2.0 (would need Struct-typed columns).
- **Phase 1c — Frame.Explode for arbitrary List columns.** `Frame.Explode` today only handles geometry columns (MultiPoint → Points, etc.). Generalize: exploding a `List<T>` column repeats each row N times (N = list length; null list → 1 row with null element; empty list → 1 row with null element per polars semantics — decide vs 0 rows before implementing, polars-parity is the default). Non-exploded columns are duplicated per element.
- **Phase 2 — List expression ops.** New ExprNodes: `list_len`, `list_get(i)` (negative index = from end), `list_slice(start, stop)`, `list_contains(elem)`. Follow the existing ExprNode pattern in `expr_eval.go` (immutable state, Eval takes `*Frame`, returns `arrow.Array` or `arrow.Column`). Add constructors to `expr.go`.
- **Phase 3 — Per-element aggregations.** `list_sum`, `list_mean`, `list_min`, `list_max`, `list_first`, `list_last` — operate row-wise on a List column to produce a scalar column of the element type. These are ExprNodes, not GroupBy Aggregators (they collapse the list dimension, not the row dimension).

**Track 2 — Aggregator.Merge (breaking interface change, interface-only slice landed).** Shipped: `Aggregator.Merge(other Aggregator) error` is a required method on the public `Aggregator` interface. Semantics documented on the interface: implementations must reset state at the start of `Aggregate` (the eager engine reuses one instance across every group), and Merge combines a peer's state into the receiver for future parallel/window executors that hand it deliberately-constructed peer instances. The v0.2 hash-partitioned executor never splits a group across workers, so Merge is never invoked on the current parallel path — the interface exists so users don't have to guess whether they'll need it later.

Deferred to a follow-up slice (needs its own design pass — see the design tension in-file):

- **Unify `Aggregator` + `aggAccumulator`.** Built-in Kinds (`sumAcc`, `meanAcc`, `minMaxAcc`, `stdVarAcc`, `nUniqueAcc`) still live behind the internal `aggAccumulator` interface (`Update` / `Finalize` / `OutputType`). They don't implement Aggregator; refactoring them onto a merged interface would let the streaming exec accept both paths uniformly.
- **Wire `streamingAggregateExec` to accept custom Fn.** `compile.go`'s `allBuiltInAggs` still routes custom aggs to `materializeExecOp`. Removing that guard requires either the unified interface above or an Aggregator→aggAccumulator adapter — neither is a small change.
- **Welford-style state on the built-in accumulators.** Mean/Std/Var currently store running sum + count in a shape that combines trivially; formalizing that as a Merge implementation is contingent on the unification decision.

**Track 3 — Over(partition) window functions (landed).** Polars-style windows: `Col("v").Sum().Over("group")` computes the group aggregate and broadcasts it back to every input row. Row order is preserved (unlike GroupBy which collapses to one row per group).

- `Expr.Over(partitionCols ...string)` wraps a scalar aggregate ExprNode with partition keys ([expr_over.go](expr_over.go)). Executor: eval the inner column once, build row→group-id via `composeCompositeKeyInto` (reused from the aggregate hot path), spin up per-group `aggAccumulator`s, scatter each group's finalized value back to its row positions via `appendCustomValue`.
- Requires a scalar aggregate as its immediate inner — `Col("v").Sum().Over(...)` works, `Col("v").Add(Col("w")).Over(...)` errors at eval. The scalar aggregate methods (`Sum`, `Mean`, `MinAgg`, `MaxAgg`, `Count`) route through `newAccumulator` so the built-in Kinds are automatically Over-compatible.
- Multi-key partitions work (`Over("g", "t")`); composes with arithmetic (`Col("v").Sub(Col("v").Mean().Over("g"))` for mean-centering).
- Explicitly out of scope for 0.2.0: `.over(...).order_by(...).rolling(...)` (rolling window on ordered partition). That's a separate feature — needs sort-within-partition semantics, which parallels the sorted-input contract concern raised earlier. Custom `Aggregator`-inside-`Over` is also deferred, contingent on the Track 2 unification.

**Sequencing rationale:** Track 1 first because it opens up ETL use cases (nested arrays in JSON/parquet inputs) and doesn't touch any hot paths. Track 2 next because it unblocks Track 3 (window functions want partial-aggregate combining internally) and clears a latent parallel-executor limitation. Track 3 last because it composes with both.

**Track 4 (partial) — Struct-typed columns.** Shipped: `builderForType` handles `arrow.STRUCT`, and the Expr framework already accommodates ExprNodes whose `Eval` returns any Arrow type (including `Struct<...>` and variable-length `List<T>`) — verified by [struct_column_test.go](struct_column_test.go) and the list-UDF reference in [expr_list_test.go](expr_list_test.go) (`TestExprList_CustomUDFReturnsListColumn`). This unblocks the road-snap-shaped UDF (`Struct<List<Uint64>, Bool>` output) and the interval-list aggregator shape (`List<Struct<Start, End>>`). Deferred for later:

- **FromStructs/ToStructs for nested struct fields.** A Go struct field whose type is itself a struct should map to an `arrow.StructType` column. Non-trivial because the recursion has to handle nullability, tag inheritance, and cycle detection. Custom ExprNode is the current path for producing Struct columns; FromStructs's slice-field handling (Track 1b) does *not* recurse into slice-of-struct.
- **StructField ExprNode.** `Col("snap").StructField("path")` to extract a named field from a struct column as a first-class Expr. Users can peel via arrow-array API today (see the road-snap test); a fluent Expr method would be the natural v0.3 add.
- **List<Struct> in `Frame.Explode`.** Today Explode's per-element scatter uses `appendArrayValueAt` which only handles primitive types; exploding a `List<Struct>` errors at the element-append step. Fix would extend `appendArrayValueAt` (or replace it with an arrow-copy helper) to handle Struct elements.

### v0.3.0 plan: partition-aware LazyFrame + athenaio (in design)

Two coupled deliverables, currently in design. Full detail lives in `contrib/athenaio/` (gitignored while we iterate — `contrib/athenaio/DESIGN.md` for the vendor package, `contrib/athenaio/PARTITION-METADATA.md` for the core-side infrastructure). This section is the summary that survives when the design docs move around.

**Anchor.** athenaio is a gobi subpackage providing a `LazyFrame`-shaped read path over AWS Athena. It lives at `contrib/athenaio/` with its own `go.mod` — isolates `aws-sdk-go-v2` dep weight, iterates independently of gobi core's semver, sets the precedent for future `bigqueryio` / `snowflakeio` / `redshiftio`. Distinguishing feature vs. a "just download the query results" wrapper: the returned `LazyFrame` carries `PartitionMetadata` so gobi's optimizer can prove alignment on `.Over(K)` / partition-wise `Join(K)` / repartition-skip `GroupBy(K)` and eliminate cross-worker shuffle.

**Tiered scope.** T1 = lifecycle wrapper (submit + poll + fetch), useful but not architecturally interesting. T3 = partition-aware `LazyFrame` via CTAS with hash bucketing — the anchor, where athenaio pulls weight no user-level wrapper can replicate. T2 (streaming download) collapses into T3 because partitioned CTAS writes N files and workers stream their share via parquetio's parallel scan.

**Write path: CTAS not UNLOAD.** UNLOAD's `partitioned_by` is Hive-style directory-per-value, catastrophic for high-cardinality keys. CTAS's `bucketed_by=ARRAY[K], bucket_count=N` provides bounded hash bucketing — the shape the alignment proof was designed around. **Iceberg default, Hive fallback (shipped):** Iceberg's Murmur3-32 hash is uniform (Hive's Java `hashCode`-based hash isn't) and its `sorted_by` is first-class enforced (Hive's is a hint). Detection: attempt Iceberg, catch specific engine-v2 error messages, fall back to Hive with a warn log via `ClientConfig.WarnLog`. `Client.hiveFallbackOnly` latches so subsequent calls on the same Client skip the doomed Iceberg attempt. `spec.TableFormat=FormatIceberg` disables the fallback for callers who explicitly want Iceberg. Iceberg cleanup still expects `DROP TABLE ... PURGE` (or explicit manifest cleanup) so metadata files under `external_location` don't orphan.

**SQL composition.** athenaio owns the outer CTAS; user provides the SELECT body as a string. No SQL DSL — the wrapper appends `CREATE TABLE ... WITH (...) AS ` + user body. Rejects user-level `ORDER BY` at the top of the SELECT (silent-drop inside a subquery is a footgun). Prepasses with `LIMIT 0` to validate the partition key exists in the projection. Namespaces internal table names as `gobi_athenaio_<8-hex>_<unix-epoch>`. Logs the composed SQL on any Athena `FAILED` state.

**Read-back verification.** After CTAS returns, call `GetTable` on the Glue catalog and confirm the actual bucketing config matches what was requested. On mismatch, error and refuse to hand back the `LazyFrame` — do **not** silently narrow `PartitionMetadata`, that produces correctness bugs months later that are nearly undiagnosable.

**Cleanup enum.**

```go
type Cleanup int
const (
    CleanupCatalogOnly Cleanup = iota  // default: DROP TABLE, files stay
    CleanupAll                          // DROP TABLE + delete S3 prefix
    CleanupNone                         // leave both (debug/inspection)
)
```

Files are the user's responsibility (S3 bucket lifecycle policy). Glue catalog entries are athenaio's responsibility. `Client.Close(ctx)` walks tracked tables and drops each; idempotent, safe on already-dropped tables. Naming convention `gobi_athenaio_<hex>_<epoch>` plus Glue tags (`athenaio_created_at`, `athenaio_client_id`, `athenaio_query_id`) enables an out-of-band sweep Lambda for orphans from crashy sessions.

**PartitionMetadata (gobi core work).** New first-class property on `LazyFrame`:

```go
type PartitionMetadata struct {
    Columns      []string   // ordered — hash(a,b) ≠ hash(b,a)
    HashFn       string     // versioned, source-namespaced tag; "" = value partitioning
    SortedBy     []SortKey
    SortEnforced bool       // Iceberg true; Hive false (hint only)
}
type SortKey struct { Column string; Descending bool }
```

Reserved `HashFn` tags: `""` (value partitioning), `"gobi/xxhash64/v1"` (runtime shuffle), `"athenaio/iceberg/murmur3-32/v1"`, `"athenaio/hive/bucket/v1"`, `"pgio/hash/v1"` and `"bigqueryio/hash/v1"` reserved for future. Cross-tag comparisons always fail — different sources cannot be assumed hash-compatible even when the algorithm name matches, because type-encoding of hash inputs differs. Cross-source shuffle alignment is v2.

**Alignment predicates (v1 first cut).** Two functions, exact match only:

- `Aligned(meta, columns) bool` — single-source check for `.Over(K)` / aligned-GroupBy consumers. Non-nil meta with ordered-equal Columns; HashFn unconstrained. Refuses subset, superset, reorder, aliasing.
- `AlignedWith(l, r *PartitionMetadata) bool` — two-source check for partition-wise Join. Both non-nil, non-empty Columns, byte-equal HashFn, ordered-equal Columns.

Refuse subset matching, FD-based aliasing, cross-hash claims. Modulus is orthogonal — `hash(K) % N` aligns with `.Over(K)` regardless of N.

**Propagation rules.** Every plan node computes output metadata from input metadata. `Filter` / `WithColumn` preserve; `Project` / `Drop` preserve iff partition columns survive; `Sort` (global) drops `Columns`+`HashFn` and replaces `SortedBy`; `GroupBy.Agg` drops entirely; `Join` preserves per-side when both sides align. `Explode` drops `SortedBy` unconditionally (changes row cardinality per parent). `Limit` strips `SortedBy` when `SortEnforced == false` — represent what's actually true, not what was hinted.

**Escape hatch.** `LazyFrame.WithPartitionAssertion(meta)` for opaque sources (RawCTAS / hand-crafted parquet scans / custom UDFs). **Narrowing only** — refuses to widen an existing source claim. User owns correctness; gobi never verifies against data.

**Sequencing (implementation order).**

1. ✅ Land `PartitionMetadata` type + attach to scan nodes. No consumer wiring yet. (`partition.go`)
2. ✅ Propagation rules through the plan tree. (`partition_propagation_test.go` has 16 subtests covering every rule.)
3. ✅ Alignment predicate + `WithPartitionAssertion`. `partitionAssertionNode` in plan tree, validation refuses widening.
4. ✅ `.Over(K)` consumer rewired — `overFastPathApplicable` + `overNode.evalContiguous` linear-scan reducer. Measured **34% wall-time reduction** on 100k-row / 100-group workload (3.73ms → 2.47ms). Required Frame-level metadata plumbing: `Frame.partitionMeta` field + `WithPartitionMeta` mutator + `withColumnExecOp.inputMeta` propagation at Compile time. Step 5+ can rely on this plumbing.
5. ✅ athenaio T1 (lifecycle wrapper). `Client.RawQuery(ctx, sql)` submits + polls + streams the single-file result via `parquetio.ReadReader` (new reader-based public API added as a prerequisite in [parquetio.go](parquetio/parquetio.go) — `ReadReader` + `ReadReaderChunksFunc` accept `io.ReaderAt + Size`, refactored `openReader` around a shared `openReaderFromRS`). No partition claim. `QueryStats` registry via `StatsFor(lf)` / `ClearStats(lf)`. Mock-driven unit tests cover happy path + terminal failure; `//go:build integration` scaffold for real-Athena smoke tests.
6. ✅ athenaio T3 (partition-aware `UnloadAndRead`), shipped in three slices:
   - **6a** — Iceberg CTAS happy path. `UnloadAndRead(ctx, spec)` composes `CREATE TABLE ... WITH (partitioning = ARRAY['bucket(N, K)'], sorted_by = ARRAY[...], format = 'PARQUET', location = ...) AS <user SELECT>`, submits, polls, read-back verifies via Glue `GetTable` (asserts `table_type=ICEBERG` + `Location` match — hard-errors on mismatch rather than silently narrowing the claim), lists bucket files via `S3.ListObjectsV2` (`.parquet` only; skips Iceberg's `metadata/*.avro|json`), reads each via `parquetio.ReadReader`, concatenates via `array.Concatenate` (single-chunk output; `gobi.Concat`'s multi-chunk would silently drop rows past `chunks[0]` in `frameToBatch`), attaches `PartitionMetadata{HashFn: "athenaio/iceberg/murmur3-32/v1", ...}` via `WithPartitionAssertion`. `Client.Close(ctx)` walks tracked tables and drops via Glue `DeleteTable`.
   - **6b** — `RawCTAS(ctx, spec)` escape hatch (user provides full SQL + `TableName` + `ExternalLocation` + optional Metadata — symmetric with `WithPartitionAssertion` on the read side). Opt-in `LIMIT 0` prepass via `UnloadSpec.ValidatePartitionCols` (adds one Athena round-trip; fails fast when partition columns missing from projection — case-insensitive check because Athena normalizes column names in result metadata). `CleanupAll` S3 deletion: extended `S3API` with `DeleteObjects`; `Client.deletePrefix` walks `ListObjectsV2` pages and batches deletes to S3's 1000-per-call cap. Non-fatal on S3 errors — catalog cleanup already succeeded.
   - **6c** — Iceberg → Hive fallback. `composeCTAS(spec, useHive)` dispatches to `composeIcebergSQL` or `composeHiveSQL`; the Hive shape uses `external_location` + `bucketed_by = ARRAY['col']` + `bucket_count = N` and appends `ORDER BY` onto the user's SELECT (wrapped in a subquery) since Hive's `sorted_by` property is a hint. `UnloadAndRead` runs an adaptive loop: try Iceberg → detect Iceberg-not-supported via `isIcebergNotSupportedErr` (substring match on `iceberg` + `not supported`, or `table_type` + `not a valid`/`does not exist`) → retry with Hive → invoke `ClientConfig.WarnLog` → latch `Client.hiveFallbackOnly` (sticky per-Client so subsequent calls skip the doomed Iceberg attempt). `spec.TableFormat=FormatIceberg` disables the fallback for callers who explicitly want Iceberg. Hive-shape output emits `PartitionMetadata{HashFn: "athenaio/hive/bucket/v1", SortEnforced: false}` — the alignment predicate correctly refuses cross-format claims (Iceberg's Murmur3-32 hash isn't interchangeable with Hive's Java hashCode-based hash) and downstream operators refuse to trust Hive's hint-only sort. `verifyCTASOutput` gained a `verifyHiveTable` branch that hard-errors if a supposedly-Hive table reports `table_type=ICEBERG` (guards against the fallback getting fooled).

   **Deferred inside step 6:** Iceberg partition-spec verification via Glue table parameters (needs Iceberg spec knowledge to parse `iceberg.schema.*` properties for bucket-count / sort-key round-trip); prepass result caching (per-Client TTL map keyed on SQL+workgroup+database — the current opt-in prepass does a fresh Athena round-trip every time).
7. ✅ Partition-wise `Join` execution — sort-merge Inner join fast path ([exec_join_merge.go](exec_join_merge.go)). Detection via `canMergeJoin`: both sides have `PartitionMetadata`, `AlignedWith` (same HashFn + Columns), both `SortedBy` starting with the join key column, `SortEnforced=true`. When those hold, `Compile` emits `sortMergeJoinExec` instead of `streamingJoinExec` — materializes both sides, precomputes per-row encoded keys via `keyOfAppend` (same byte encoding as the hash-join path, so byte order matches numeric/string order the writer's sort enforced), two-pointer merge with cross-product on equal-key runs, output built via the shared `Frame.buildTwoSidedOutput`. **Measured 31% wall-time reduction** on 10k×10k Int64-keyed Inner join (1.98ms → 1.36ms). Inner-only in v1; Left/Semi/Anti stay on the streaming hash path.
8. ✅ Aligned `GroupBy` fast path ([groupby_aligned.go](groupby_aligned.go)) — linear-scan aggregate that fires when `groupByFastPathApplicable` holds (input `PartitionMetadata` aligned on group keys + `SortedBy` starts with them + `SortEnforced=true`). Detects group boundaries via composite-key comparison between consecutive rows (two swapped scratch bufs, no per-row alloc), emits each group in stream order — skips the `rowKeys []string` allocation, the `groups map[string][]int`, and the terminal `sort.Strings(order)` that the general path builds. Extracted `buildAggBuilders` + `assembleOutput` helpers so both paths share aggregation type-selection + output construction. Dispatch sits *after* `aggFast`'s single-primitive-key hot path, so string-key workloads still hit the tuned 1BRC path; the win lands on multi-column keys, First/Last, and custom Fn aggregators (any shape `aggFast` bails on). **Measured 74% wall-time reduction** on 100k-row / 1000-group two-column-key `AggSum` (12.4ms → 3.25ms) — well above the design's 10-15% prediction because the general path's multi-key encoding + map + final sort are collectively expensive.

Steps 1-4 + 7-8 are gobi core (all shipped). Steps 5-6 are athenaio-only, gitignored under `contrib/athenaio/` with its own go.mod; only [parquetio.go](parquetio/parquetio.go)'s reader-based API + its two tests live in the tracked tree. All 8 sequenced steps of the v0.3.0 plan are now landed.

**Testing.** Alignment predicate + propagation rules are pure functions — unit-testable without an executor. Executor integration tests use `WithPartitionAssertion`-constructed frames to prove shuffle-skip kicks in (verify via `ExplainPhysical`). athenaio unit tests mock the aws-sdk-go-v2 client interfaces; real-Athena tests behind `//go:build integration` with `ATHENAIO_TEST_WORKGROUP` / `ATHENAIO_TEST_RESULT_LOCATION` (same pattern as pgio's `PGIO_TEST_DSN`). No localstack equivalent for the query engine, so integration coverage is limited to real-AWS-with-creds.

**Deferred to v2 or later:**

- `Client.OpenPartitionedTable(ctx, tableName)` reading an existing bucketed Glue table's spec via `GetTable`. Falls out for almost free once `PartitionMetadata` is defined right and the alignment proof is a pure function of metadata + plan.
- Cross-source shuffle-aligned join. Needs gobi runtime to compute source-compatible hashes on demand.
- Multiple partition-metadata claims on a single `LazyFrame` (weaker + stronger claims coexisting).
- Sibling vendor packages: `bigqueryio` / `snowflakeio` / `redshiftio` land under `contrib/` with the same shape. Not a shared `sqlio` interface — vendor SQL diverges too much — but the `PartitionMetadata` machinery in core is uniform.

**Benchmark baseline + landed measurements.** Post-step-3 hot-path numbers were captured before any consumer wiring — this table is the reference point for steps 4-8 (see notes column for what landed). Hot paths were unchanged through step 3 (metadata plumbing lives off the runtime path). Step 4 landed the first measurable win (Over aligned fast path); steps 5-6 are athenaio I/O — no in-tree benchmarks because they'd measure AWS latency + mock overhead, neither useful. Steps 7-8 haven't shipped yet.

Hardware: Apple M3 Pro (11 cores), Go 1.26, `go test -run='^$' -bench=. -benchtime=3s -count=3 ./`. Median across 3 runs.

| Benchmark | ns/op | B/op | allocs/op | notes |
|---|---:|---:|---:|---|
| `GroupBy_100k_by_100` | 3.78ms | 2.55MB | 1303 | step-3 baseline; step 8 target |
| `Series_Add_Float64_1M` | 762µs | 25.4MB | 11 | step-3 baseline |
| `Series_Add_Int64_1M` | 945µs | 25.4MB | 11 | step-3 baseline |
| `Series_Mul_Float64_1M` | 782µs | 25.4MB | 11 | step-3 baseline |
| `Series_Sum_Float64_1M` | 731µs | 0 | 0 | step-3 baseline (zero-alloc reduction) |
| `Series_GtScalar_Float64_1M` | 2.23ms | 1.59MB | 11 | step-3 baseline |
| `SJoin_10kPointsIn100Polygons` | 1.30ms | 2.80MB | 40519 | step-3 baseline (spatial R-tree, won't move under step 7) |
| `SJoin_100kPointsIn10kPolygons` | 17.2ms | 52.0MB | 621926 | step-3 baseline (spatial R-tree, won't move under step 7) |
| `Over_UnalignedHashMap` (100 × 1000) | 3.73ms | | | step-4 slow-path baseline |
| `Over_AlignedFastPath` (100 × 1000) | 2.47ms | | | step-4 **34% faster** than slow path |
| `Join_HashUnaligned` (10k × 10k) | 1.98ms | | | step-7 hash-path baseline |
| `Join_MergeAligned` (10k × 10k) | 1.36ms | | | step-7 **31% faster** — sort-merge fast path |
| `GroupByUnaligned` (100k rows / 1k groups, 2-col key) | 12.4ms | | | step-8 general-path baseline |
| `GroupByAligned` (100k rows / 1k groups, 2-col key) | 3.25ms | | | step-8 **74% faster** — linear-scan fast path |

**Expected motion by step:**

- **Step 4 (`.Over(K)` consumer):** ✅ landed. Measured 34% wall-time reduction (3.73ms → 2.47ms), within the predicted 30-50% band. `BenchmarkOver_UnalignedHashMap` vs. `BenchmarkOver_AlignedFastPath` in [expr_over_test.go](expr_over_test.go) — same fixture, only difference is whether `Frame.WithPartitionMeta` attaches the aligned+sorted+enforced claim.
- **Step 7 (sort-merge Inner join):** ✅ landed. Measured 31% wall-time reduction (1.98ms → 1.36ms) on 10k×10k Int64-keyed Inner join. Fast path eliminates the hash-index build by walking both sides with a two-pointer merge over encoded keys. Still materializes both sides (unlike the hash path which streams the probe side), so the RSS story is nuanced: hash index vanishes but probe frame materializes — net RSS depends on input-key cardinality vs. probe-side size. The wall-time win is unambiguous.
- **Step 8 (aligned GroupBy linear-scan):** ✅ landed. Measured 74% wall-time reduction (12.4ms → 3.25ms) on 100k-row / 1000-group two-column-key `AggSum`. The design predicted 10-15% by targeting the parallel executor's hash-repartition step; the eager `GroupBy.Agg` general path turned out to be the bigger win — its `rowKeys []string` + `map[string][]int` + terminal `sort.Strings` collectively dominate for multi-column keys. Sits after `aggFast` (single-primitive-key hot path from the 1BRC work), so string-key workloads keep their existing performance; the new fast path lights up on shapes `aggFast` bails on (multi-column keys, First/Last, custom aggregators). Parallel-executor's hash-repartition-skip variant is deferred — the eager win already delivers the anticipated user-visible benefit.

**Rule for benchmark-worthy PRs going forward:** each step-4-8 delivery ships with the aligned-vs-unaligned benchmark that measures its win. Otherwise the design-doc claim ("30-50% off Over") is asserted, not measured — which is what the athenaio-partition-metadata thread was designed to avoid.

## Testing conventions

- Tests co-locate with source: `frame_test.go` next to `frame.go`. Table-driven where meaningful; test data built in-place, no external fixtures for unit tests.
- Cross-package tests use `_test` package suffix (e.g. `package parquetio_test`) so they exercise the public API. Same-package tests skip the suffix to touch internals.
- Benchmarks in `*_bench_test.go` or in `benchmarks/*.go` (the latter uses `//go:build ignore` and is run via `go run`, not `go test`).
- Race detector is mandatory before pushing. Every existing test passes under `-race`.

## Non-obvious things worth knowing

- **arrow-go's `ReadRowGroups(ctx, indices, rowGroups)`** treats `nil` slices as "read nothing," not "read everything." Always pass concrete indices. `openReader` in parquetio does this correctly; if you add a new read path, don't cargo-cult `nil, nil` from other arrow-go examples.
- **`Frame.Head(0)` treats 0 as "default 5"**, not zero. If you need a real empty Frame from Head, use `f.take(nil)` or route through `Limit(0)` in the lazy layer (which handles this correctly).
- **String equality on Series** is not supported by `Series.Eq` (numeric-only). The expression evaluator handles it via a dedicated `stringCompare` helper. If adding new comparison ops, follow that pattern.
- **`Series.Retain`/`Release`** — a Series is a borrowed view. If callback code needs to keep a Frame past callback return (see `ReadFileChunksFunc`), call `frame.Retain()` and match with `frame.Release()`.
- **The optimizer is always-on in `LazyFrame.Collect()`**. `CollectRaw()` exists for debugging + benchmarks. Optimizer overhead is ~8 µs per plan (measured), so there's no reason to expose disable knobs in production.
- **`benchmarks/`** and **`experiments/`** are in `.gitignore` — they're local performance harnesses, not part of the shipped library. Files there use `//go:build ignore` so they don't affect normal builds.

## Where to add things

- New expression node type → implement `ExprNode` (in a new file or `expr_eval.go`), add an `Expr` constructor to `expr.go`.
- New plan node type → append to `plan.go`, add `Collect` dispatch in `lazy.go`, add walker case in `optimize.go`'s `mapExprs` and `walkRewrite`.
- New optimizer rule → new `*Rule` type in `optimize.go`, add to `DefaultRules()`. Rules walk the tree themselves; use `walkRewrite` for structural rules and `mapExprs` for expression rewrites.
- New scan source → format package calls `gobi.NewScanNode(label, schema, readFn, opts...)`. Optional `WithColumnProjection` / `WithPredicatePushdown` callbacks let the source participate in optimizer pushdowns.
- New aggregation → implement `Aggregator` interface, users pass via `Aggregation{Column, Fn: myAgg}`. See `groupby_custom_test.go` for a mode/p95 example.

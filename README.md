# gobi

`gobi` is a geospatial dataframe library for Go, built on top of
[Apache Arrow](https://arrow.apache.org). Think of it as a GeoPandas-shaped
API with Polars-shaped internals: columnar, chunk-slice fast paths, and
built around a strongly-typed schema.

> **Status:** early. The API is settled enough to build a small pipeline on;
> semver stability begins with v1.0. GeoParquet v1.1
> output has been verified against GeoPandas v1.1.1 and QGIS v4.0.2.

## Highlights

- **Arrow-native.** A `Frame` is a set of Arrow `Column`s. `Head`, `Tail`,
  and row selection are zero-copy where possible.
- **Full 2D + optional XYZ geometry.** `Point`, `LineString`, `Polygon`
  (with holes), `MultiPoint`, `MultiLineString`, `MultiPolygon`,
  `GeometryCollection`. WKB and WKT round-trip per OGC/ISO SFA 1.2 (type
  codes 1..7 for 2D, 1001..1007 for XYZ).
- **Real spatial operations.** Area, length, centroid (every geometry
  type), convex hull, containment, `Simplify` (Douglas-Peucker), `Buffer`
  with rounded OR square joins/caps, `EstimateUTMCRS` on every type,
  full topological predicates (`Intersects` / `Contains` / `Within` /
  `Touches` / `Overlaps` / `Crosses` / `Disjoint`).
- **Polygon boolean ops.** Pure-Go Martinez-Rueda sweepline:
  `Clip` / `Union` / `Difference` / `SymDifference` and a
  `Dissolve(geoms)` collection reducer using spatially-sorted
  divide-and-conquer merge (like shapely's `unary_union`). Bit-exact
  parity with geopandas verified on a 500-polygon benchmark; a
  Sutherland-Hodgman fast path takes over when both operands are
  convex (4× faster than the general sweep). Linestring-vs-polygon
  clipping has its own Cyrus-Beck primitive: `LineString.Clip(p)` and
  `SplitBy(p)` (same on `MultiLineString`), targeting the Overture
  road-vs-h3-cell partitioning workload — 100-vertex hex clip in
  ~890 ns on M3 Pro.
- **Antimeridian handling.** `CrossesAntimeridian(g)` detects
  ±180°-crossing geographic-CRS input; `SplitAtAntimeridian(g)`
  splits it into per-side components via linear-lon interpolation.
  `EstimateUTMCRS` refuses crossing input with a clear error rather
  than silently returning the wrong zone (a Fiji-shaped bbox
  `[178, -178]` used to pick UTM 31N over Africa).
- **Geometry constructors from columns.** `PointsFromXY(x, y, crs)`
  and `PointsFromXYZ(x, y, z, crs)` build a WKB geometry Series
  directly from numeric coordinate columns — modeled on
  `geopandas.points_from_xy`. Mixed numeric types (Float64, Float32,
  Int64, Int32) auto-promote; nulls in either input yield a null
  geometry.
- **Reprojection engine.** WGS84 ↔ Web Mercator ↔ all 120 UTM zones,
  using the ellipsoidal Redfearn/Snyder formulas. Sub-cm round-trip
  accuracy verified against reference cities worldwide.
- **Spatial index and join.** Static Sort-Tile-Recursive R-tree with
  bounding-box and k-nearest queries. Internal storage is struct-of-
  arrays (parallel `[]float64` bbox arrays) — measured **−27% wall
  time on bbox-intersect Search** (100k-item tree, 1k queries)
  compared to the AoS shape it replaces. Public API unchanged.
  `Frame.SJoin(right, ..., pred)` with `SPIntersects` / `SPContains` /
  `SPWithin` predicates, multi-threaded across left rows, tunable via
  `Workers(n)`.
- **SoA geometry kernels (v0.4.1).** For hot-path callers that touch
  a geometry's coordinates without needing the full `[]Point` /
  `Polygon` object graph — a common shape on the parquetio write
  path, spatial-sort pipelines, and per-polygon many-candidate
  refine loops. `PointsView{Xs, Ys []float64, …}` materializes
  parallel coordinate slabs via `LineString.View()`,
  `Polygon.RingViews()`, and friends. Zero-allocation WKB
  byte-stream scanners (`BoundsFromWKB`, `CentroidFromWKB`,
  `CentroidAndBoundsFromWKB`, `PIPFromWKB`, `PlanarLengthFromWKB`,
  `PlanarAreaFromWKB`) walk WKB bytes tracking running accumulators —
  semantics match `ParseWKB(data).<op>()` exactly on projected CRSes.
  `PIPRingFromXY` / `PIPPolygonFromRings` deliver a measured **3×
  held-view speedup** vs. `Polygon.Contains(pt)` on the SJoin-refine
  shape (64-vertex polygon × 100 candidates). Wired into
  `geoparquet.computeBboxColumns` (**−31% wall time, −47% memory**
  on 100k-row GeoParquet writes), both Hilbert sort paths (**−24%
  wall time, essentially zero-alloc WKB pass** on 100k-row sorts),
  and `Series.GeomArea` / `Series.GeomLength` on projected CRSes
  (**−48% / −67% wall time** on 1M-vertex WKB blobs, zero
  allocations per row). Iterative Douglas-Peucker on parallel
  slabs (`SimplifyDPFromXY`, `PointsView.SimplifyDP`) replaces the
  recursive AoS `[]Point` splice+append pattern — wired into every
  `Simplify` entry point transparently. **−81% wall time and 24k×
  fewer allocations** on a 1M-vertex polyline (3.85 s → 0.75 s,
  260k allocs → 11). WKB → PointsView direct-parse
  (`PolygonRingViewsFromWKB`, `MultiPolygonRingViewsFromWKB`,
  `LineStringViewFromWKB`, `PrepareFromWKB`) skips the `[]Point`
  intermediate for polygon-family types — **2.3× faster and 5×
  less memory** than `ParseWKB(data).RingViews()` on a 1M-vertex
  polygon. SoA min-distance kernels
  (`PointToSegmentDistanceSqXY`, `PointToPolylineMinDistanceSq`)
  power an internal rewrite of `planarMinDistance` — every
  `GeomDistance` / `WithinDistance` caller gets **−80% wall time**
  on a 64-vertex Polygon×Polygon and **−82%** at 1024 vertices,
  from deferring `math.Sqrt` to a single call at the end instead
  of paying one per (vertex, segment) pair. Andrew's monotone-chain
  convex hull on slabs (`ConvexHullFromXY`,
  `PointsView.ConvexHull`) replaces the Graham-scan polar-angle
  sort — every `Polygon.ConvexHull` / `geometry.ConvexHull(g)`
  caller sees **−30 to −54% wall time** at n≥1024 and **3-4× lower
  memory** (index sort on `[]int` instead of `[]Point` structs).
  WKB-direct `Series.GeomDistance` fast path
  (`PlanarMinDistanceFromWKB`) skips the AoS `ParseWKB` on
  bbox-disjoint rows — **−73% memory** on 10k rows of 64-vertex
  polygons vs a fixed target (65 MB → 17.6 MB per call, −12%
  wall time). Slice 14 extends the same bbox-reject pattern to
  `Series.GeomIntersects` / `GeomContains` / `GeomWithin` and the
  LazyFrame `Col('geom').GeomIntersects(x)` filter expression —
  **−74% wall time and 98.5% less memory** on a 5k-row × fixed-AOI
  benchmark (802 μs → 205 μs, 2.37 MB → 36 KB). `Series.GeomBounds`
  and `Series.GeomCentroid` also wired to the byte-stream scanners.
  Slice 15 sweeps the last non-clip Series entry points:
  `Series.GeomDWithin` (bbox-min-distance reject: **−66% wall,
  −93% memory**) and `Series.GeomType` (WKB-header peek: **−88%
  wall, 358× fewer allocs** — was pure ParseWKB waste).
  Slice 16 adds boundary-inclusive `PIPInclusiveFromWKB` and
  wires an SJoin `Points × Polygons` fast path that skips both
  `decodeGeometryColumn` passes — **−62% wall, −70% memory, 33×
  fewer allocs** on the 10k-points × 100-large-polygons bench.
  Slice 17 adds Series-level bbox reject for `GeomClip` (empty
  on disjoint intersection) and `GeomDifference` (row unchanged
  on disjoint difference) — **−81% / −93% wall time** and
  **334× / 530× fewer allocs** on 5k rows × small mask.
  Slice 18 adds convex-containment fast paths inside
  `geometry.Boolean()` itself — when either operand is a convex
  polygon that fully contains the other, intersection returns
  the contained operand unchanged. **−50% wall and −96% memory**
  on the isolated containment case; **−26% wall, −30% memory**
  on the M-cell loop (100 cells × user disc). Slice 19 extends
  the Sutherland-Hodgman fast path from convex×convex to
  **convex clipper × any single-ring subject** when the
  intersection is guaranteed simply connected (transition-count
  gate on vertex containment) — **−83% wall time** on the
  concave-L-shape-clipped-by-AOI shape. Slice 20 extends the
  SJoin fast path from Points-only to **LineString × Polygon**
  and **single-ring Polygon × single-ring Polygon** (bilateral
  vertex-inside + AoS fallback for edge crossings) — **−36% wall
  and −72% memory** on 1k lines × 100 large polygons. Plus
  Union/Difference containment fast paths in `geometry.Boolean()`
  (union with a convex containing operand → operand unchanged;
  `a ⊆ convex b` difference → empty without the sweep). Slice 21
  finishes clip coverage: `Series.GeomUnion` and
  `Series.GeomSymDifference` now build the combined MultiPolygon
  inline for bbox-disjoint rows (**−89% wall, 22× fewer allocs**
  on 5k rows × small mask), and SJoin gains **SPWithin /
  SPContains fast paths** for Polygon×Polygon and LineString×Polygon
  (convex-container gate; convexity precomputed during extract).
  Slice 22 pushes the same shape into the LazyFrame executor:
  Series scalar-comparison ops (`GtScalar` / `LtScalar` / etc.)
  now dispatch to `compute.CmpF64*` SIMD kernels, and the
  expression executor recognizes AND-chained range and bbox
  filters, dispatching to fused kernels
  (`compute.AndChainF64Range` / `AndChainF64BBox`) that skip
  intermediate boolean-column materialization — **−49% wall
  and 71% fewer allocs** on a 100k-row 4-comparison bbox filter.
  Slice 23 adds Int64 comparison kernels
  (`compute.CmpI64Ge/Le/Gt/Lt`) with matching Series wire-in,
  a foundational `compute.CountTrue` bool-reduce + `Series.CountTrue()`
  wrap, and a lane-count gate on the SIMD compare kernels that
  fixes a pre-existing Apple 2-lane NEON regression the Slice 22a
  dispatch had exposed (SIMD build now matches scalar build on
  Apple, wins expected on amd64 AVX2 / AVX-512).
- **Prepared-geometry predicate API.** `PreparedGeometry` +
  `Prepare(g)` + `TestPrepared(pred, a, b)` amortize the AoS→SoA
  materialization tax across many predicate calls — the shape
  spatial-index-free refine loops need. Fast paths for
  Point×Polygon and Point×MultiPolygon; other pair shapes fall
  through to `Test()` transparently. Not used by gobi's built-in
  SJoin (measurement showed R-tree pre-filter drives candidate
  count too low for amortization to pay off); designed for
  advanced callers with dense overlapping polygons or many
  candidates per prepared geometry.
- **DataFrame ops.** `Filter`, `Take`, `Head`, `Tail`, `SortBy`
  (multi-key stable, nulls-last), `WithColumn`, `DropColumn`,
  `SelectCols`, `Rename`, `Explode` (also as a `LazyFrame` streaming
  step), `Join` (inner / left / right / full / semi / anti with
  coalesced keys). `GroupBy(...).Agg(...)` with built-in kinds:
  `Count`, `Sum`, `Mean`, `Min`, `Max`, `First`, `Last`, `NUnique`,
  `Std`, `Var`, `Median`, `Mode`. Aggregations can carry a
  per-aggregation `Filter Expr` for `SUM(x) FILTER (WHERE …)`-style
  reductions. Series arithmetic, comparisons, aggregations — all
  with single-chunk bulk fast paths and Int64-preserving scalar
  arithmetic.
- **Built-in collect-set aggregators.** `NewStringSetAggregator()`,
  `NewInt64SetAggregator()`, `NewUint64SetAggregator()`,
  `NewInt32SetAggregator()`, `NewUint32SetAggregator()` — distinct-
  value roll-ups that emit `List<T>` per group and stream through
  the aggregate executor via `IncrementalAggregator`.
- **User-defined aggregations.** `type Aggregator interface { ... }`
  plugs directly into `GroupBy.Agg` alongside the built-ins. Opt
  into the streaming aggregate executor by additionally implementing
  `IncrementalAggregator` (`Clone` / `Update` / `Finalize`) — per-
  batch state updates instead of a materialize-then-reduce pass.
- **Expression IR.** `gobi.Col("price").Mul(gobi.Lit(1.08)).Gt(gobi.Lit(100))`
  builds a data tree, not a chain of already-executed calls.
  `Frame.FilterExpr` and `Frame.WithColumnExpr` evaluate it. The
  built-in vocabulary covers arithmetic (`Add`/`Sub`/`Mul`/`Div`),
  bitwise (`BitAnd`/`BitOr`/`BitXor`), comparisons, logical
  (`And`/`Or`/`Not`), `IsNull`/`IsNotNull`, `Cast(dtype)` (numeric-
  to-numeric + Timestamp source), `If`/`Coalesce`, `LitNull(dtype)`,
  `LitEmptyList(elem)`, `ListLen`, `ListUnion`, `Shift(n)`,
  window functions (`.Sum()/.Mean()/.Min()/.Max()/.Count()/.Median()/
  .Mode().Over(cols...)` for scalar-agg-and-broadcast; shape-preserving
  inners like `Shift(1).Over(K)` for prev-row-within-partition
  patterns), `UnixNano()` (Timestamp → Int64 ns), and
  `HaversineExpr(lat1, lon1, lat2, lon2, unit)` for great-circle
  distance between two point columns. A `Custom(node ExprNode)`
  escape hatch lets sibling packages (H3, hashes, ML inference)
  plug in their own expression types alongside the built-ins.
- **Alignment-aware fast paths.** `PartitionMetadata` claims
  (attached via `LazyFrame.WithPartitionAssertion` or produced by
  `contrib/athenaio` on Iceberg CTAS output) let `GroupBy`, `Over`,
  and `Join` skip the hash-shuffle and linear-scan partition
  boundaries directly. Runs 30-70% faster than the general path
  when applicable; falls through automatically otherwise.
- **List<T> and Struct columns.** First-class support end-to-end:
  Explode expands list rows, `ListUnion` merges per-row, aggregations
  can emit `List<T>` (see set aggregators above), and Struct-typed
  builders round-trip through Frame → LazyFrame → Frame.
- **LazyFrame + rule-based optimizer.** `df.Lazy()` and
  `parquetio.ScanFile(path)` build plan trees that don't execute
  until `.Collect()`. Nine rewrite rules run to a fixed point:
  constant folding, dead-filter removal, adjacent-filter combining,
  push-filter-below-project, push-filter-below-sort, column
  projection pushdown, predicate pushdown (into row-group stats),
  and cascade-empty (short-circuits `Lit(false)`-derived subtrees).
  Projection pushdown routes into `parquetio.ReadOptions.Columns` — 2.4×
  faster reads on partial-column queries, matching the eager
  baseline. Optimizer overhead is ~8 µs on a five-node plan;
  always-on.
- **Parallel streaming executor.** `LazyFrame.Collect()` compiles
  the optimized plan to a tree of `ExecOperator`s that pull one
  record batch at a time — bounded memory regardless of source
  size. Filter, Project, WithColumn, Drop, Rename, Select, Explode,
  Limit, ScanFrame, and ScanFile all stream natively. Adjacent
  batch-transform ops are fused into a single per-batch pass by
  the compiler (~22% fewer allocations on typical Filter → Project
  → WithColumn chains). Aggregate (all built-in kinds + custom
  `IncrementalAggregator`s) and hash-join (Inner/Left/Semi/Anti)
  run as native streaming operators — no materialization step.
  Parquet scan parallelizes across row-groups; the streaming hash
  aggregate partitions rows across workers by key hash. Both scale
  to `GOMAXPROCS` out of the box. `LazyFrame.ExplainPhysical()`
  prints what strategy each node compiles to (worker counts
  included).
- **Datetime + timezone-aware ops.** `Timestamp[ns]` columns with
  optional IANA tz label. Component extractors, `AddDuration` /
  `DiffDuration`, comparisons, sub-day + calendar truncation.
  `ResampleEvery(timeCol, interval)` for downsampling and
  `RollingBy(timeCol, period)` for trailing time windows; plus fixed-size
  `Series.RollingSum` / `Mean` / `Min` / `Max` / `Count`.
- **Multi-format I/O.** Every format has `ReadFile` / `WriteFile` at
  Frame level plus a `ScanFile` LazyFrame entry point where the
  underlying source supports it, and every format also has
  struct-direct `ReadStructs[T]` / `WriteStructs[T]` wrappers with
  a per-format struct-tag namespace — `parquet:` / `csv:` /
  `geojson:` / `gpkg:` / `shp:` / `kml:` / `pgio:` — so the same
  Go type can carry different column names per format (shp's
  10-char DBF alias, `parquet:"-"` to omit from parquet only,
  etc.). Formats: CSV (with `.gz` / `.zst` / `.bz2` auto-detect),
  Parquet with proper GeoParquet 1.1 metadata (snappy / gzip /
  brotli / zstd / lz4, canonical PROJJSON for EPSG:3857 + all 120
  UTM zones), full RFC 7946 GeoJSON (every geometry type + XYZ,
  `.geojsonl` streaming), OGC GeoPackage 1.3 (SQLite, RTree
  spatial index, spec-compliant metadata), PostgreSQL / PostGIS
  (via `pgx/v5`, native `CopyFrom` bulk load), **KML + KMZ
  read/write** (zipped KML auto-detected by `.kmz` extension), and
  **Shapefile read/write** (`.shp` + `.shx` + `.dbf` + optional
  `.prj`).
- **Streaming readers.** `csvio.ReadFileChunksFunc` and
  `parquetio.ReadFileChunksFunc` yield one Frame per record batch
  (~64k rows), releasing arrow buffers after each callback. Peak
  memory is bounded regardless of source-file size — good for ETL
  over multi-GB inputs.
- **Column projection.** `parquetio.ReadOptions{Columns: ...}` skips fetch,
  decompress, and arrow materialization for the columns you don't
  need. Composes with streaming.
- **Parquet write tuning.** `parquetio.WriteOptions` exposes
  `RowGroupRows` (predicate-pushdown-friendly small groups vs.
  compression-friendly large ones), `BloomFilterColumns` +
  `BloomFilterFPP` (equality-filter skipping in DuckDB / Spark /
  Polars / pyarrow / gobi readers today).
- **Parallelism controls.** Package-level `SetMaxParallelism(n)` or
  per-op `Workers(n)`.
- **Pure Go, no cgo.** No GDAL, no GEOS, no libproj. Cross-compiles
  cleanly to every architecture Go supports.

## Install

```bash
go get github.com/zoobst/gobi
```

Requires Go **1.26** or newer.

## Docs

Full API reference is on
[pkg.go.dev](https://pkg.go.dev/github.com/zoobst/gobi) —
auto-generated from source doc comments. Every subpackage
(`parquetio`, `csvio`, `geometry`, `geojsonio`, `gpkgio`, `pgio`,
`kmlio`, `shpio`) has its own page; use the same base URL with
the package path appended.

## Quick start

### Read a CSV, write a GeoParquet

```go
package main

import (
    "github.com/zoobst/gobi/csvio"
    "github.com/zoobst/gobi/parquetio"
)

type city struct {
    Name       string `csv:"name"`
    Population int64  `csv:"population"`
    Geom       string `csv:"geometry" geom:"true"`
}

func main() {
    // .gz / .zst / .bz2 are auto-detected from the filename; explicit
    // ReadOptions.Compression overrides.
    df, err := csvio.ReadFile[city]("cities.csv.gz", &csvio.ReadOptions{CRSHint: 4326})
    if err != nil { panic(err) }
    defer df.Release()

    // The output file carries a spec-compliant GeoParquet 1.1 metadata
    // blob and reads cleanly in GeoPandas / QGIS. nil = Snappy + arrow's
    // default row-group sizing.
    _ = parquetio.WriteFile(df, "cities.parquet", nil)
}
```

### Build a geometry column from lat/lng

```go
// Turn two numeric columns into a WKB geometry column. Argument
// order is x, y — i.e. longitude, latitude — matching WKB / GeoJSON /
// geopandas.points_from_xy. Nulls on either side yield a null point.
lng, _   := df.Column("lng")
lat, _   := df.Column("lat")
points, _ := gobi.PointsFromXY(lng, lat, 4326)
df, _     = df.WithColumn("geometry", points)
// df["geometry"] is now a proper WKB column, ready for SJoin,
// GeoParquet write, etc.
```

### Spatial join

```go
cities, _ := parquetio.ReadFile("cities.parquet", nil)   // 1M points
regions, _ := parquetio.ReadFile("regions.parquet", nil) // 5k polygons

// Which region contains each city?
joined, err := cities.SJoin(regions, "geometry", "geometry", gobi.SPWithin)

// Cap parallelism per-op (see "Parallelism" below):
joined, err = cities.SJoin(regions, "geometry", "geometry", gobi.SPWithin, gobi.Workers(4))
```

### GroupBy + aggregate

```go
gb, _ := df.GroupBy("region")
totals, _ := gb.Agg(
    gobi.Aggregation{Column: "population", Kind: gobi.AggSum},
    gobi.Aggregation{Column: "population", Kind: gobi.AggMean, Alias: "avg_pop"},
)
```

### Sort

```go
// Multi-key, stable, nulls-last. Earlier keys have priority;
// later keys break ties.
sorted, _ := df.SortBy(
    gobi.SortKey{Column: "date"},                        // ascending
    gobi.SortKey{Column: "revenue", Descending: true},
)
```

### Tuned parquet write

```go
// Small row groups + bloom filters on high-cardinality equality columns.
// DuckDB / Spark / Polars / pyarrow readers all use the bloom filters
// for predicate pushdown on equality filters.
err := parquetio.WriteFile(df, "events.parquet", &parquetio.WriteOptions{
    Codec:              parquetio.CodecZstd,
    RowGroupRows:       128_000,                     // 0 = arrow default (~1M)
    BloomFilterColumns: []string{"user_id", "session_id"},
    BloomFilterFPP:     0.01,                        // 0 = arrow default (0.05)
})
```

### Streaming ETL

```go
// Reads a 5 GB parquet file at ~15 MB peak memory. Only two columns are
// fetched off disk; the rest are never decompressed.
err := parquetio.ReadFileChunksFunc(
    "events.parquet",
    &parquetio.ReadOptions{Columns: []string{"user_id", "ts"}, ChunkRows: 64_000},
    func(batch *gobi.Frame) error {
        // Process ~64k rows at a time. The batch is released after
        // return; call batch.Retain() to keep it past this callback.
        return sink.Write(batch)
    },
)
```

CSV has the same shape: `csvio.ReadFileChunksFunc[Row](path, opts, fn)`.

### Derived columns

Two shapes. `WithColumn` accepts any Series the caller built by hand:

```go
// A user-space helper produces the derived Series any way it likes
// (external library call, vectorized loop, whatever). WithColumn wires
// it back into the Frame — appending, or replacing an existing column
// of the same name.
lat, _   := df.Column("lat")
lng, _   := df.Column("lng")
cells, _ := h3x.Encode(lat, lng, 9)
df, _     = df.WithColumn("h3", cells)

df, _ = df.DropColumn("raw_geometry")
```

`WithColumnExpr` accepts an expression tree, so pipelines composed from
built-in ops read left-to-right:

```go
// Same shape via the expression IR — no intermediate Series to name.
df, _ = df.WithColumnExpr("usd_price",
    gobi.Col("eur_price").Mul(gobi.Lit(1.08)),
)
df, _ = df.WithColumnExpr("margin",
    gobi.Col("revenue").Sub(gobi.Col("cost")),
)
```

### User-defined aggregation

```go
// Compute the 95th percentile of a numeric column per group.
type P95 struct{}

func (P95) Aggregate(s gobi.Series, rows []int) (any, error) {
    arr := s.Column().Data().Chunks()[0].(*array.Float64)
    vals := make([]float64, 0, len(rows))
    for _, r := range rows {
        if !arr.IsNull(r) { vals = append(vals, arr.Value(r)) }
    }
    if len(vals) == 0 { return nil, nil }
    sort.Float64s(vals)
    return vals[int(float64(len(vals)-1)*0.95)], nil
}
func (P95) Type() arrow.DataType { return arrow.PrimitiveTypes.Float64 }
func (P95) Name() string          { return "p95" }

// Mix custom + built-in aggregations in one call.
gb, _ := df.GroupBy("h3")   // Uint64 keys are hashable
out, _ := gb.Agg(
    gobi.Aggregation{Column: "latency_ms", Kind: gobi.AggMean},
    gobi.Aggregation{Column: "latency_ms", Fn: P95{}},
)
```

### Filter

Two shapes. `Filter` takes an already-computed Boolean Series (mask):

```go
pops, _ := df.Column("population")
mask, _ := pops.GtScalar(1_000_000)
big,  _ := df.Filter(mask)
```

`FilterExpr` takes an expression tree — no intermediate Series to name:

```go
big, _ := df.FilterExpr(
    gobi.Col("population").Gt(gobi.Lit(1_000_000)).
        And(gobi.Col("country").Eq(gobi.Lit("US"))),
)
```

### Window functions

`Over(partitionCols...)` runs either an aggregate (broadcast to every
row in the partition) or a shape-preserving inner like `Shift(1)`
(prev-row-within-partition). Both compose with the alignment
fast paths when `PartitionMetadata` proves the frame is
partition-contiguous.

```go
// Per-region total (broadcast). Same shape works for Mean / Min /
// Max / Count / Median / Mode.
df, _ := df.WithColumnExpr("region_total",
    gobi.Col("sales").Sum().Over("region"),
)

// Previous row's timestamp within (eid) partition. First ping of
// each entity emits null.
df, _ := df.WithColumnExpr("prev_ts",
    gobi.Col("ts").Shift(1).Over("eid"),
)

// Great-circle distance between successive pings, entirely in the
// expression tree — no Custom ExprNode needed.
df, _ := df.WithColumnExpr("step_km", gobi.HaversineExpr(
    gobi.Col("lat"),
    gobi.Col("lon"),
    gobi.Col("lat").Shift(1).Over("eid"),
    gobi.Col("lon").Shift(1).Over("eid"),
    geometry.UnitKilometers,
))
```

For flag-unpacking a packed Int64 into per-bit indicator columns:

```go
const FlagBogonIP = 1 << 3
df, _ := df.WithColumnExpr("has_bogon_ip",
    gobi.Col("flags").
        BitAnd(gobi.Lit(int64(FlagBogonIP))).
        Ne(gobi.Lit(int64(0))),
)
```

### Reproject

```go
p := geometry.Point{X: -73.9857, Y: 40.7484, CRSValue: geometry.WGS84}
utm, _ := p.EstimateUTMCRS()   // EPSG:32618 (WGS 84 / UTM zone 18N)
proj,  _ := p.ToCRS(utm)       // coordinates now in meters
```

### Buffer + simplify

```go
poly := geometry.SimplePolygon(points, geometry.PseudoMercator)

// Rounded (semicircle caps, rounded joins) — the default.
rounded, _ := geometry.Buffer(poly, 100, geometry.BufferOptions{Segments: 32})

// Square-style buffer: flat/extended caps, mitre joins. Faster and
// produces fewer vertices (5 instead of 33 for a Point).
square, _ := geometry.Buffer(poly, 100, geometry.BufferOptions{
    Style: geometry.BufferSquare,
})

simpler := poly.Simplify(5.0) // Douglas-Peucker at 5-unit tolerance
```

### Polygon boolean ops (Clip / Union / Dissolve)

```go
subject := geometry.SimplePolygon(subjectPts, geometry.PseudoMercator)
mask    := geometry.SimplePolygon(maskPts, geometry.PseudoMercator)

// Row-scalar ops on a whole geometry Series.
gs, _ := df.Column("geom")
clipped,    _ := gs.GeomClip(mask)         // intersection per row
unioned,    _ := gs.GeomUnion(mask)
diffed,     _ := gs.GeomDifference(mask)

// Dissolve every row's polygon into a single geometry (like
// shapely's unary_union — divide-and-conquer merge, bit-exact
// against geopandas on the bundled 500-polygon bench).
merged, _ := gs.GeomDissolve()

// Or reach the raw ops directly on a Geometry.
inter, _ := geometry.Clip(subject, mask)
uni,   _ := geometry.Union(subject, mask)
```

### Linestring clipping (LineString / MultiLineString vs Polygon)

Boolean ops handle Polygon-vs-Polygon; linestring-vs-polygon has its
own primitive that skips the general polygon sweep entirely.

```go
line := geometry.NewLineString([]geometry.Point{
    {X: -5, Y: 5}, {X: 15, Y: 5}, {X: 15, Y: -5}, {X: -5, Y: -5},
}, geometry.PseudoMercator)
cell := geometry.SimplePolygon(hexPts, geometry.PseudoMercator)

inside            := line.Clip(cell)          // fragments inside cell
inside, outside   := line.SplitBy(cell)       // both sides, one pass

// MultiLineString version — flat []LineString result, walk order
// preserved across components.
ml := geometry.NewMultiLineString([]geometry.LineString{line, other}, geometry.PseudoMercator)
frags := ml.Clip(cell)
```

Convex polygons (including h3 cells) take a Cyrus-Beck fast path with
per-segment AABB reject; concave / multi-ring polygons fall back to
sort-and-march. Coordinate system is planar — densify first if you
need geodesic accuracy, and use `SplitAtAntimeridian` before clipping
if your linestring crosses ±180°.

### Antimeridian split

```go
// A geographic-CRS polygon spanning ±180° confuses every naive
// cartesian primitive. Detect + split it into per-side components
// before feeding it downstream.
poly := geometry.SimplePolygon([]geometry.Point{
    {X: 170, Y: -10}, {X: -170, Y: -10},
    {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
}, geometry.WGS84)

if geometry.CrossesAntimeridian(poly) {
    split, _ := geometry.SplitAtAntimeridian(poly)
    // split is a MultiPolygon with one component in [170,180] and
    // another in [-180,-170]; each has valid, non-crossing bounds.
}
```

### Struct-direct io

Every io package has a struct-direct wrapper that uses per-format
struct tags — `parquet:` for parquet, `shp:` for shapefile,
`geojson:` for GeoJSON, etc. Resolution falls back to a universal
`gobi:` tag when the format-specific one is absent, then to `csv:`
(legacy), then to the Go field name. `format:"-"` skips a field.

```go
type Feature struct {
    ID         int64   `parquet:"id" shp:"OBJECTID"` // per-format aliases
    Name       string  `gobi:"name"`                 // shared across formats
    Population float64 `shp:"POP10"`                 // 10-char DBF alias
    Notes      string  `parquet:"-"`                 // omit from parquet
    Geometry   []byte  `geom:"true"`                 // geometry marker
}

// Read a slice of structs directly — no Frame boilerplate.
rows, err := parquetio.ReadStructs[Feature]("in.parquet", nil)

// Write the same slice out as a shapefile — column names come from
// the shp: tags automatically.
err = shpio.WriteStructs(rows, "out", nil)
```

The same shape works for `csvio.ReadStructs`,
`geojsonio.ReadStructs` / `WriteStructs`, `gpkgio.ReadStructs` /
`WriteStructs`, `kmlio.ReadStructs` / `WriteStructs`, and
`pgio.ReadStructsQuery` / `ReadStructsTable` / `WriteStructsTable`.

### R-tree

```go
tree := geometry.NewRTree(bboxes)
hits    := tree.Search(query)      // IDs whose bounds intersect query
nearest := tree.Nearest(x, y, k)   // k closest bounds, sorted
```

Internal storage is struct-of-arrays: parallel `[]float64` bbox
arrays for both item and node bboxes. `Search` sees a measured
27% wall-time reduction on 100k-item / 1k-query workloads vs. the
AoS shape it replaces. Public API is unchanged from earlier
releases.

### Datetime + timezone

```go
type event struct {
    Name string    `csv:"name"`
    When time.Time `csv:"when" time:"2006-01-02 15:04:05"`
}

df, _ := csvio.ReadFile[event]("events.csv", nil)
when, _ := df.Column("when")

// Render the same instants in New York local time.
nyWhen, _ := when.WithTimezone("America/New_York")

// Component extractors honor the tz.
hourNY, _ := nyWhen.Hour()   // Int64 series with local hours

// Truncate to the top of each local day.
dayStart, _ := nyWhen.TruncateToCalendar(gobi.CalendarDay)
```

### Resample + rolling

```go
// Downsample to hourly buckets (Unix-epoch aligned).
r, _ := df.ResampleEvery("when", time.Hour)
hourly, _ := r.Agg(
    gobi.Aggregation{Column: "value", Kind: gobi.AggSum},
    gobi.Aggregation{Column: "value", Kind: gobi.AggMean, Alias: "avg"},
)

// Trailing 5-minute rolling sum keyed by timestamp.
tr, _ := df.RollingBy("when", 5*time.Minute)
rollSum, _ := tr.Agg("value", gobi.AggSum)

// Fixed-window rolling on a plain Series.
val, _ := df.Column("value")
m7, _ := val.RollingMean(7) // 7-row moving average
```

### KML / KMZ / Shapefile

```go
// KML → Frame (auto-parses ExtendedData into columns)
places, _ := kmlio.ReadFile("places.kml", nil)
_ = kmlio.WriteFile(places, "out.kml", nil)

// KMZ works the same way — extension picks the format automatically.
// Set kmlio.WriteOptions{Format: FormatKMZ} explicitly for Writer flows.
_ = kmlio.WriteFile(places, "out.kmz", nil)         // writes zip with doc.kml

// Shapefile → Frame (reads .shp + .shx + .dbf + optional .prj)
counties, _ := shpio.ReadFile("counties", nil)      // no .shp suffix needed
_ = shpio.WriteFile(counties, "counties_out", nil)  // writes all four files
```

## Packages

| Package                   | What it does                                                                                    |
|---------------------------|-------------------------------------------------------------------------------------------------|
| `github.com/zoobst/gobi`  | `Frame`, `Series`, `GroupBy`, `Join`, `SJoin`, `Explode`, datetime + rolling + resample, options |
| `.../gobi/geometry`       | 2D + XYZ primitives, WKB / WKT, CRS + reprojection, predicates, R-tree, Buffer / Simplify / Centroid |
| `.../gobi/csvio`          | Typed CSV read + streaming (`ReadFileChunksFunc`), gzip / zstd / bzip2 auto-detect              |
| `.../gobi/parquetio`      | Parquet read/write + streaming + column projection + row-group + bloom-filter tuning; snappy/gzip/brotli/zstd/lz4 + GeoParquet 1.1 |
| `.../gobi/geojsonio`      | Full RFC 7946 GeoJSON (all geometry types + XYZ) — Frame-level `ReadFile`/`WriteFile`/`ScanFile`, `.geojsonl` streaming |
| `.../gobi/gpkgio`         | Read / write OGC GeoPackage 1.3 (SQLite) with RTree spatial index + LazyFrame `ScanFile` + SQL predicate pushdown |
| `.../gobi/pgio`           | **Beta.** PostgreSQL / PostGIS via `pgx/v5` — `ReadQuery`/`ReadTable`/`ScanTable` + `WriteTable` with `CopyFrom` bulk load. Integration tests are `//go:build integration`-gated; set `PGIO_TEST_DSN` and run against a live PostGIS to exercise them. |
| `.../gobi/kmlio`          | Read / write KML (OGC 12-007r2) + KMZ (zipped KML). Placemarks + ExtendedData. `.kmz` extension auto-detected. |
| `.../gobi/shpio`          | Read / write ESRI Shapefile (`.shp` + `.shx` + `.dbf` + optional `.prj`)                        |
| `.../gobi/contrib/athenaio` | AWS Athena CTAS integration — `UnloadAndRead`, `UnloadAndReadBuckets`, `RawCTAS`, `RawCTASBuckets` return LazyFrames with `PartitionMetadata` claims that flow into gobi's aligned fast paths. Own `go.mod`; versions independently. |

## Geometry columns

`gobi` intentionally does *not* use an Arrow custom-extension type for
geometry. Geometries are Arrow `Binary` columns holding WKB, with the
column marked in schema metadata:

```
"gobi:geometry_type" = "WKB"
"gobi:crs_epsg"      = "4326"
```

When writing Parquet, gobi additionally emits a proper GeoParquet 1.1
`geo` blob at the file level with `primary_column`, `geometry_types`,
`crs`, and `bbox`. That is what makes gobi-produced files interoperate
with GeoPandas and QGIS out of the box.

Use `gobi.GeometryField(name, epsg)` to construct a tagged field manually.

## Extending gobi

Three extension points cover most add-on work today, without forking:

**1. Derived columns via a helper package + `Frame.WithColumn`.**
Write a sibling package (e.g. `h3x`, `hashcol`) whose functions take one or
more `gobi.Series` and return a `gobi.Series`. Users compose:

```go
lat, _   := df.Column("lat")
lng, _   := df.Column("lng")
cells, _ := h3x.Encode(lat, lng, 9)
df, _     = df.WithColumn("h3", cells)
```

Because the helper controls the whole loop, it can dispatch to native
libraries (H3, MurmurHash, whatever) once per row without the DataFrame
having to know anything about the operation. `WithColumn` appends or
replaces, `DropColumn` removes.

**2. Custom aggregations via the `Aggregator` interface.**

```go
type Aggregator interface {
    // Reduce s[rows...] to a single scalar. Return nil for a null.
    Aggregate(s Series, rows []int) (any, error)
    // Declares the arrow type of Aggregate's return values. Supports
    // Float32/64, Int32/64, Uint32/64, Bool, String, Binary, Timestamp.
    Type() arrow.DataType
    // Suffix for the default output column name.
    Name() string
}
```

Set `Aggregation{Column: "col", Fn: myAgg}` and call `GroupBy.Agg` as
usual. Mix custom + built-in aggregations in a single call. If the
returned dynamic type doesn't match the declared `Type()`, `Agg`
returns an error naming the offending aggregation rather than
panicking.

Opt into the streaming aggregate executor by additionally
implementing `IncrementalAggregator`:

```go
type IncrementalAggregator interface {
    Aggregator
    // Clone returns a fresh instance with empty per-group state.
    // Called once per group at first-touch; each group runs on its
    // own clone, no cross-group state sharing.
    Clone() IncrementalAggregator
    // Update adds col[rows] to this instance's state. Called at
    // most once per input batch per group. Additive — do not reset.
    Update(col Series, rows []int) error
    // Finalize returns the group's aggregated value. Called once
    // per group after all Updates.
    Finalize() any
}
```

Plain `Aggregator`s still work — they route through the materializing
fallback exactly as before. `IncrementalAggregator`s skip the
materialize-then-reduce and process per batch, matching the built-in
aggregators' streaming shape. The bundled set aggregators
(`NewStringSetAggregator`, etc.) are the reference implementations.

**3. Custom expression nodes via the `ExprNode` interface.**

```go
type ExprNode interface {
    // Evaluate against a Frame. Return a Series with input.NumRows() rows.
    Eval(input *Frame) (Series, error)
    // Declared output arrow type given the input schema. Used by
    // FilterExpr / WithColumnExpr for validation.
    Type(schema *arrow.Schema) (arrow.DataType, error)
    // Sub-expressions, for tree walkers.
    Children() []Expr
    // Pretty-printer for logs and debug output.
    String() string
}
```

Wrap your node with `gobi.Custom(node)` and it composes with the
built-in `Col`, `Lit`, and operator methods:

```go
// h3x.Encode returns a gobi.Expr backed by a custom node.
cellExpr := h3x.Encode(gobi.Col("lat"), gobi.Col("lng"), 9)

df, _ = df.WithColumnExpr("h3", cellExpr)
df, _ = df.FilterExpr(cellExpr.Eq(gobi.Lit(uint64(0xdead))))
```

Because expressions are data, not function calls, the tree can be
inspected before evaluation (`e.String()`, `e.Node().Children()`),
type-checked without touching the buffers (`e.Node().Type(schema)`),
and — in a future release — rewritten by an optimizer that pushes
predicates into scans and prunes unused columns. Extension points
that implement `ExprNode` will benefit from those passes automatically.

**Group-by key types.** Hashable key columns: `String`, `Bool`,
`Int32`, `Int64`, `Uint32`, `Uint64`, `Float64`, `Timestamp`.
`Uint64` is what makes H3-cell grouping ergonomic.

## Parallelism

Two layers of parallel execution work together in a LazyFrame
collect: the parquet scan splits row-groups across workers, and the
streaming aggregate partitions rows by key hash across workers.

- **Parallel scan.** `parquetio.ScanFile(path, &parquetio.ReadOptions{ScanWorkers: N})`
  splits row-groups across N goroutines. Each worker reads a
  disjoint subset of row-groups; batches fan-in through a bounded
  channel. `ScanWorkers: 0` (the default) auto-picks `GOMAXPROCS`,
  capped at the file's row-group count. `ScanWorkers: 1` forces
  serial for reproducibility. Files with a single row-group skip
  parallel scan automatically (no benefit possible).
- **Parallel aggregate.** The streaming hash aggregate
  (`GroupBy(...).Agg(...)` on a LazyFrame with built-in Kinds)
  partitions rows across `GOMAXPROCS` workers by key hash — no
  cross-worker key overlap, no locks, no value-level combine at
  merge. Kicks in for any aggregate where every `Aggregation` uses
  a built-in `Kind`; custom `Fn` aggregators still route through
  the materializing fallback.

Both layers respect the package-level `SetMaxParallelism(n)` /
per-op `Workers(n)` overrides, in this priority:

1. Per-op `gobi.Workers(n)` option
2. Package default via `gobi.SetMaxParallelism(n)`
3. `GOMAXPROCS`

```go
gobi.SetMaxParallelism(4)                 // process-wide default
df.SJoin(..., gobi.Workers(8))            // override for one call
df.SJoin(..., gobi.Workers(1))            // force sequential
```

`LazyFrame.ExplainPhysical()` shows the resolved worker count for
each parallel node — useful when debugging why a query didn't get
the parallelism you expected.

## Design constraints

- **Pure Go, no cgo.** GDAL, GEOS, libproj, and other C libraries are
  intentionally off the table. This keeps `go build` clean across every
  platform Go targets and avoids the LGPL/toolchain overhead. The trade:
  no File Geodatabase support and no PROJ-grade reprojection beyond
  WGS84 / Web Mercator / UTM. Polygon boolean ops (Clip / Union /
  Difference / SymDifference / Dissolve) DO ship — as a pure-Go
  Martinez-Rueda sweepline — but complex-projection pairs like
  Albers ↔ Lambert still require an external tool.

## Performance

All numbers Apple M3 Pro, warm cache, 10–20 iterations per op. Fixtures
and scripts live under [`benchmarks/`](benchmarks/) — regenerate with
`go run generate_fixture.go`, `go run generate_csv_fixture.go`, and
`go run generate_spatial_fixture.go`.

### Compute ops (1M-row Parquet, non-spatial)

| Op                            | gobi (default) | gobi (SIMD) | pandas 2.3 | Polars 1T | Polars all |
|-------------------------------|---------------:|------------:|-----------:|----------:|-----------:|
| `Sum(value_a)`                |        1.04 ms | **0.43 ms** |    0.31 ms |   0.10 ms |    0.09 ms |
| `value_a + value_b`           |        1.79 ms |     1.55 ms |    1.03 ms |   0.82 ms |    0.90 ms |
| `Filter(value_a > 500k)`      |       15.9 ms  |    14.9 ms  |    6.83 ms |   1.96 ms |    1.60 ms |
| `GroupBy(key).Agg(Sum,Mean)`  |       45.4 ms  |    42.6 ms  |   19.73 ms |   9.89 ms |    2.42 ms |

Polars 1T = `POLARS_MAX_THREADS=1`; Polars all = default (all cores).
gobi (SIMD) = compiled with `GOEXPERIMENT=simd` on Go 1.27+ arm64/amd64;
gobi (default) = normal build with the scalar compute path. `Sum` gained
a 2.4× speedup under SIMD (`compute.SumF64` via `simd.Float64s.Add`
lane-parallel reduce). The other ops changed less because SIMD helps
compute-bound loops most and pandas/Polars' remaining lead here is
architectural (NumPy's arena allocator, Polars' cache-blocked kernels)
rather than kernel-level SIMD. See [compute/](compute/) for the full
per-op arrow-go vs gobi kernel positioning.

**Cross-arch SIMD note** (v0.4.1): the SIMD wins in this table are on
Apple M-series (deep out-of-order execution engines). Measured on
AWS Graviton 2 / Ampere Altra (Neoverse-N-class server ARM64), the
`compute.BoundsF64` kernel actually **regresses ~48%** because
Neoverse's NEON `Float64.Min/Max` instructions cost more per lane
than scalar compare-branch. Other kernels (`SumF64`, and the
new-in-v0.4.1 `PolygonCentroidShoelace`) win on both architectures
— shoelace measures **−28% wall time on Ampere** at every size ≥64,
while flat on Apple silicon. The rule: `GOEXPERIMENT=simd` is safe
to enable on server ARM64, but call `compute.BoundsF64` explicitly
only when targeting Apple silicon. `geometry.BoundsFromXY` stays
portable scalar so it's fast everywhere. See the "Slice 6" section
in [.vscode/CLAUDE.md](.vscode/CLAUDE.md) for the arch-vs-kernel
decision table + measured numbers.

### CSV read (38.6 MB / 1M rows)

| Reader                          |    per-read | notes                                              |
|---------------------------------|------------:|----------------------------------------------------|
| Polars 1.42, all threads        |    11.5 ms  | multi-threaded Rust tokenizer + SIMD (typed schema) |
| Polars 1.42, 1 thread           |    13.5 ms  | SIMD numeric parse, single core                    |
| pandas 2.3, `engine="pyarrow"`  |    43.2 ms  | pyarrow C++ tokenizer                              |
| pandas 2.3, default (C engine)  |   149.4 ms  | pandas' native C tokenizer                         |
| **gobi `csvio.Read`**           | **224.3 ms** | arrow-go's CSV wraps stdlib `encoding/csv`      |

The gap is entirely in stdlib `encoding/csv` allocating a `[]string` per
row + per-cell `strconv` — 99.5% of gobi's CSV allocations show up
there in a pprof run. Closing it means replacing that layer with a
byte-level tokenizer that writes straight into Arrow buffers; not on
the roadmap yet. Maybe the arrow-go folks will pick that up.

### Spatial ops (100k points × 100 polygons)

| Op                              |     gobi | geopandas 1.1 |     result |
|---------------------------------|---------:|--------------:|-----------:|
| Read points.parquet (100k)      |  4.04 ms |     37.74 ms  | **9.3× faster** |
| Read polygons.parquet (100)     |  0.26 ms |      0.83 ms  | **3.2× faster** |
| `Area(polygons)`                |  0.02 ms |      0.13 ms  | **6.5× faster** |
| `Centroid(polygons)`            |  0.02 ms |      0.16 ms  | **8× faster**   |
| `SJoin(100k pts, 100 polys)`    |  3.49 ms |      2.62 ms  | 1.3× slower     |

Gobi wins on read and per-row bulk ops because it doesn't have to
construct Shapely Python objects per row on load. The one gap is
`sjoin`: geopandas uses Shapely 2's GEOS-backed STRtree in C++; gobi's
Sort-Tile-Recursive R-tree is pure Go. Landing within 40% of a
GEOS-C++ index while staying cgo-free is the intended trade.

### LazyFrame + optimizer (projection pushdown)

Same 1M-row parquet fixture, `Select(id, value_a)` — reading 2 of 4
columns. Measures the projection-pushdown rule's effect on I/O and
decode cost.

| Path                                                        | per-op   | vs. baseline |
|-------------------------------------------------------------|---------:|:-------------|
| `ReadFile(path, Options{Columns:[id,value_a]})` (eager)     |  6.21 ms | 1.0× (baseline) |
| `ScanFile(path).Select(id,value_a).Collect()` (optimized)   |  8.12 ms | ~1.31× — close to eager |
| `ScanFile(path).Select(id,value_a).CollectRaw()` (no rules) | 14.58 ms | 2.3× slower — reads all 4 cols |

The optimizer's `ProjectionPushdown` rule turns the lazy pipeline
into the equivalent of an eager `Options.Columns` — 2.0× faster than
the same pipeline with optimization disabled. Optimizer overhead
itself is **8 µs per plan** (measured on a 5-node pipeline), or
0.14% of the collect time. Always-on optimization is effectively
free.

### 1 Billion Row Challenge

The [1BRC fixture](https://github.com/gunnarmorling/1brc) is 1 billion
weather-station rows in Snappy-compressed parquet (~4 GB on disk).
Query: min / mean / max of `temperature` grouped by `station`. Apple
M3 Pro, 11 GOMAXPROCS.

| Engine                                   |     wall |  user CPU | peak RSS |
|------------------------------------------|---------:|----------:|---------:|
| **gobi streaming** (parallel scan + agg) | **12.7s** | **~88s** | **611 MB** |
| Polars 1.42 streaming (reference)        |    3.0 s |      ~15s |  4.42 GB |
| Polars 1.42 eager (reference)            |   12.0 s |     ~120s | 20.96 GB |

Streaming end-to-end — no `LazyFrame.CollectRaw()` materialization,
no disk spill. Getting here took six complementary changes:
partitioning the parquet scan across row-groups; sharding the
streaming hash aggregate by key hash across workers; eliminating
per-row key allocations (reusable scratch buffers +
single-string-key fast path that reads the arrow value zero-copy);
eliminating the per-row `map[*aggGroup][]int` bucket lookup that
sat between `groups[key]` and the accumulator `Update` calls
(v0.3.9); pooling the parallel dispatcher's per-worker `[]int`
row-index payloads via `sync.Pool` (v0.3.9); and threading the
pooled payloads as `*[]int` through the channel path so
`sync.Pool.Put` doesn't box the slice header into an interface
per call (v0.3.9). gobi's own allocations across a full 1BRC run
are ~540 MB total — down from ~13 GB pre-pool.

Peak RSS is **7.2× lower than Polars streaming** and 34× lower than
Polars eager because gobi keeps at most one batch per worker in
memory + recycles per-batch scratch through `sync.Pool` — Polars
buffers larger working sets by design.

### Alignment fast paths (1M rows, 1000 groups)

When the caller can prove input partitioning + sort order via
`WithPartitionMeta(...)`, `Over` and `GroupBy` skip the general
hash-map path and linear-scan group boundaries directly. Same
`bench_alignment.parquet` fixture (1M rows, 10 regions × 100 ids,
pre-sorted by `(region, id)`) across all three rows; Polars doesn't
expose alignment metadata, so it picks its own path.

| Op                              |     gobi (unaligned) | gobi (aligned) |    Polars all |
|---------------------------------|---------------------:|---------------:|--------------:|
| `Over(region, id).Sum(v)`       |             47.8 ms  |    **29.9 ms** (37% faster) |       7.11 ms |
| `GroupBy(region, id).Sum(v)`    |             94.6 ms  |    **22.1 ms** (77% faster) |       4.65 ms |

The aligned `GroupBy` number improved from 32.4 ms → 22.1 ms in v0.3.9
(vs the v0.2.0 landing) via two complementary changes: the streaming-
aggregate refactor (buckets-map elimination + `sync.Pool` for per-worker
row-index payloads, `-32%` overall) and the aligned-path SIMD reduce
wire-in (`compute.SumF64` on contiguous per-group slices under
`GOEXPERIMENT=simd`).

Sort-merge Inner join (also alignment-gated) is measured in-tree
rather than in this fixture bench — a fixture-scale self-join is
dominated by 100M-row cross-product construction rather than the
join algorithm itself. `BenchmarkJoin_MergeAligned` on 10k×10k Int64
keys shows the aligned sort-merge path is 31% faster than the hash
join it replaces, and `BenchmarkJoin_HashMultiProbeBatch` shows the
streaming-hash-join build-side cache fix that landed with the
alignment work is 49% faster on multi-batch Inner joins.

Polars still wins in absolute terms — the aligned fast paths close a
1.6× gap on Over and cut a 3.3× gap on GroupBy to 4.8× overall. If
the workload can carry alignment metadata, taking it is nearly free.

### GeoParquet write + Hilbert sort (v0.4.1 SoA push)

Two write-path measurements on a 100k-row 9-vertex-polygon Frame,
before/after the Slice 2 + Slice 3 wire-ins (`BoundsFromWKB`,
`CentroidAndBoundsFromWKB`). No config change required — the
scanners are the default path on every build.

| Path                              | before (v0.4.0) | after (v0.4.1) | delta |
|-----------------------------------|----------------:|---------------:|:------|
| `WithBboxCoveringColumns` (write) | 31.7 ms / 154 MB / 600k allocs | **21.9 ms / 81 MB / 300k allocs** | **−31% wall, −47% mem** |
| `SortByHilbert`                   | 33.6 ms / 115.6 MB / 300k allocs | **25.5 ms / 42.9 MB / 66 allocs** | **−24% wall, −63% mem, essentially zero-alloc pass** |
| `HilbertSortWithCovering`         | 51.1 ms / 203.5 MB / 600k allocs | **41.8 ms / 131 MB / 300k allocs** | **−18% wall, −36% mem** |

The wins come from skipping the `ParseWKB` full-geometry
allocation on every row when the caller immediately discards the
geometry after `.Bounds()` / `.Centroid()`. Byte-stream scanners
walk the WKB blob tracking running accumulators — zero allocation
per row on well-formed input.

Slice 7 extends the same shape to `Series.GeomArea` /
`Series.GeomLength` on projected CRSes via `PlanarAreaFromWKB` /
`PlanarLengthFromWKB`. Microbench on Apple M3 Pro:

| Path                              | n     | ParseWKB + AoS | Scanner   | delta wall | allocs/op    |
|-----------------------------------|------:|---------------:|----------:|:-----------|:-------------|
| `PlanarLengthFromWKB`             | 1M    | 12.2 ms        | **4.0 ms** | **−67%**   | 2 → **0**    |
| `PlanarLengthFromWKB`             | 1K    | 14.5 μs        | **3.4 μs** | **−76%**   | 2 → **0**    |
| `PlanarAreaFromWKB`               | 1M    |  6.6 ms        | **3.5 ms** | **−48%**   | 3 → **0**    |
| `PlanarAreaFromWKB`               | 1K    |  8.9 μs        | **3.5 μs** | **−61%**   | 3 → **0**    |

Geographic CRSes still take the AoS haversine / spherical-excess
path (the scanner has no CRS context from the WKB blob alone).

Slice 9 replaces the recursive AoS `douglasPeucker` with an
iterative SoA form (`SimplifyDPFromXY` /
`PointsView.SimplifyDP`). Every `Simplify` entry point routes
through the shared internal helper, so `LineString.Simplify` /
`Polygon.Simplify` / `MultiLineString.Simplify` /
`MultiPolygon.Simplify` / `series.GeomSimplify` all benefit
transparently. Wiggly-sine polyline benchmark, Apple M3 Pro:

| Path (`Simplify`)     | n    | AoS recursive             | SoA iterative           | delta wall | allocs/op    |
|-----------------------|-----:|---------------------------|-------------------------|:-----------|:-------------|
| `SimplifyDP`          | 64   | 1.80 μs / 17 allocs       | **302 ns / 3 allocs**   | **−83%**   | 17 → **3**   |
| `SimplifyDP`          | 1K   | 126 μs / 259 allocs       | **25 μs / 4 allocs**    | **−80%**   | 259 → **4**  |
| `SimplifyDP`          | 64K  | 81.5 ms / 17k / 122 MB    | **16.7 ms / 8 / 242 KB**| **−80%**   | 17k → **8**  |
| `SimplifyDP`          | 1M   | 3.85 s / 260k / 5.75 GB   | **0.75 s / 11 / 3.2 MB**| **−81%**   | 260k → **11** |

The 24k× allocation reduction on 1M-vertex input is the killer
metric — the AoS recursion appends O(log n) intermediate `[]Point`
slices at every split, which the SoA form skips entirely with a
retain-bitmap + argmax-of-cross-squared inner loop that defers
the per-split `sqrt` to a single compare.

Slice 10 closes the remaining AoS gap in the `PreparedGeometry`
build path. Before: `Prepare(ParseWKB(data))` walked the WKB
bytes to `[]Point`, then walked `[]Point` to `[]float64` slabs —
two full O(n) passes and two allocations per ring. After:
`PolygonRingViewsFromWKB` / `PrepareFromWKB` walk the WKB bytes
once into slabs. On a 1M-vertex polygon (Apple M3 Pro):

| Path                                | `ParseWKB + RingViews` | direct parse            | delta wall | delta mem  |
|-------------------------------------|-----------------------:|------------------------:|:-----------|:-----------|
| `PolygonRingViewsFromWKB`           | 8.4 ms / 80 MB / 6a    | **3.7 ms / 16 MB / 3a** | **−56%**   | **−80%**   |
| `PrepareFromWKB` (retains AoS G)    | 9.5 ms / 80 MB / 6a    | **7.4 ms / 80 MB / 6a** | **−22%**   | flat       |

The `PrepareFromWKB` gap is smaller because it still materializes
the AoS `Polygon` for `TestPrepared`'s non-fast-path fall-through
contract — but that materialization is a cheap slab-to-`Point`
copy, not a second WKB byte walk. Advanced callers wanting the
full slab-only win can build `PreparedGeometry` directly with
`PolygonRingViewsFromWKB`.

Slice 11 targets a different hot loop: `planarMinDistance`, the
internal driver behind `GeomDistance` and `WithinDistance`. The
pre-Slice-11 shape walked geometries via `forEachVertex` /
`forEachSegment` closures and called `math.Hypot` per (vertex,
segment) pair — for a Polygon×Polygon distance at n=1024 that's
~1M `Hypot` calls (each doing a `sqrt`) per row. The rewrite
extracts each input's polylines into slab form once, then tracks
running-min *squared* distance on flat coordinate arrays; the
final `sqrt` runs exactly once at the end.

| n     | Legacy AoS (closure + per-pair Hypot) | Slab-form SoA    | delta wall |
| ----- | -----------------------------------:  | ---------------: | :--------- |
| 16    | 6.9 μs                                | **1.7 μs**       | **−76%**   |
| 64    | 106 μs                                | **21 μs**        | **−80%**   |
| 256   | 1.60 ms                               | **272 μs**       | **−83%**   |
| 1024  | 28.8 ms                               | **5.3 ms**       | **−82%**   |

### h3-agg (bbox filter + groupby-h3 + aggregate, 2M rows)

A geospatial analytics workload: bbox-filter 2M `(lat, lon, eid, dt,
h3_res8)` rows down to ~500K survivors, group by H3 cell (~39K
groups), and aggregate `count`, `n_unique(eid)`, `min(dt)`,
`max(dt)`. The h3-agg bench harness lives at
[`benchmarks/gobi/gobi_h3_agg_lazy_bench.go`](benchmarks/gobi/gobi_h3_agg_lazy_bench.go)
+ [`benchmarks/python/polars_h3_agg_bench.py`](benchmarks/python/polars_h3_agg_bench.py) — precomputed h3
cells so every engine measures the same aggregation throughput, not
h3 encoding.

| Engine                                 | per-iter wall |  peak RSS |
|----------------------------------------|--------------:|----------:|
| Polars 1.42 streaming                  |     **15 ms** |   341 MB  |
| **gobi (v0.3.9)**                      |     **86 ms** | **501 MB** |
| pandas 2.3                             |        86 ms  |   792 MB  |
| gobi (v0.3.8, pre-session)             |       152 ms  |   ~500 MB |

`gobi (v0.3.9)` matches pandas within measurement noise on wall
time and uses **36% less peak RSS than pandas**. Getting here took
five stacked changes that each earn their entry in
[CHANGELOG.md](CHANGELOG.md):

1. **Filter fusion** — `Col(a) OP lit AND Col(b) OP lit AND …`
   evaluates in a single row-loop with short-circuit on first
   false; no intermediate Boolean Series per comparison.
2. **Scalar-cmp fast path for `Ne` / `Le` / `Ge`** — the pre-existing
   `binOpNode.Eval` shortcut only fired on `Eq` / `Lt` / `Gt`.
3. **Null-check hoisting in the fused filter** — when every leaf's
   column has `NullN() == 0` (typical for numeric parquet), the
   per-row × per-leaf `IsNull()` calls vanish.
4. **`aggFast` (fast-path GroupBy) covers `AggNUnique` + Timestamp
   Min/Max** — previously "one bad agg disabled the whole call."
5. **Streaming `nUniqueAcc` type-specialization** — LazyFrame
   groupby-with-nunique dispatches to native `map[string]/[int64]/
   [float64]` instead of byte-encoded keys.

Polars is still 5.7× faster wall-time on this shape. That gap is
architectural (cache-blocked kernels, arena allocator, compiled
expression graph) rather than kernel-level and won't close without
a rewrite of the same magnitude. gobi's differentiator here is
matching pandas on throughput while using less memory than pandas
— on this specific workload Polars beats gobi on both wall and RSS,
because Polars streams the parquet decode straight into its
grouper without our intermediate LazyFrame batches. The RSS story
inverts at the 1BRC scale below.

### Why the remaining compute-op gap will shrink

`Sum` / `Add` are already memory-bandwidth-bound. The remaining gap on
`Sum` is SIMD reduction (Polars and numpy both use parallel-lane
accumulators). Go 1.27 (August 2026) shipped the portable `simd`
stdlib package with arm64 NEON + amd64 AVX2/AVX-512 backends under
`GOEXPERIMENT=simd`. The `compute/` subpackage plus `series_ops_simd.go`
(v0.4.0) ship SIMD kernels behind
`//go:build goexperiment.simd && (arm64 || amd64)` — comparisons,
reductions (`SumF64` / `MinF64` / `MaxF64`), and elementwise
arithmetic all landed on portable `simd` (previously amd64-only via
`simd/archsimd`).

The remaining ~4.5× 1BRC gap vs Polars streaming breaks down (from
CPU profile, post-v0.3.9):

- ~22% arrow-go parquet decode (not our code; a custom pooled
  `memory.Allocator` wrapping arrow-go's decoder path is a
  scoped-out future win — see Future Work in CLAUDE.md)
- ~12% Go runtime `map[string]*aggGroup` probe (the fundamental
  hash-by-string cost; possibly addressable with a specialized
  Robin-Hood / Swiss-table impl)
- ~30% runtime scheduling / idle wait between workers
- ~14% GC background work (down proportionally with v0.3.9's alloc
  churn cuts)
- ~11% gobi's aggregate consume path (already tight per-batch
  typed-slice loops; the fast-path type-switch already dispatches
  once per chunk, not per row)
- remainder: misc

Wall-time gains from here need either (a) SIMD reduction kernels
wired into the streaming aggregate's `Update` methods (Go 1.27),
(b) a custom decoder-buffer allocator to cut arrow-go's contribution,
or (c) a specialized hash-map for the string-keyed groupby probe.
None are on a specific timeline; the v0.3.9 wins have already
put gobi solidly ahead of pandas (matched exactly on h3-agg,
substantially ahead on memory) and closed the wall-time gap to
Polars from 6× to 4.5×.

## Development

- `go test -race ./...` should pass before you push.
- Please keep dependencies minimal — Arrow is the one big one on purpose.

## License

MIT. See [LICENSE](LICENSE).

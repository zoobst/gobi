# gobi

`gobi` is a geospatial dataframe library for Go, built on top of
[Apache Arrow](https://arrow.apache.org). Think of it as a GeoPandas-shaped
API with Polars-shaped internals: columnar, chunk-slice fast paths, and
built around a strongly-typed schema.

> **Status:** early. The API is settled enough to build a small pipeline on;
> semver stability begins with the first tagged release. GeoParquet v1.1
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
  with rounded joins and caps, `EstimateUTMCRS` on every type.
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
  bounding-box and k-nearest queries. `Frame.SJoin(right, ..., pred)`
  with `SPIntersects` / `SPContains` / `SPWithin` predicates,
  multi-threaded across left rows, tunable via `Workers(n)`.
- **DataFrame ops.** `Filter`, `Take`, `Head`, `Tail`, `SortBy`
  (multi-key stable, nulls-last), `WithColumn`, `DropColumn`,
  `GroupBy(...).Agg(count/sum/mean/min/max)`, `Join`
  (inner / left / right / full / semi / anti with coalesced keys),
  `Explode`. Series arithmetic (Add/Sub/Mul/Div + scalar), comparisons,
  aggregations — all with single-chunk bulk fast paths.
- **User-defined aggregations.** `type Aggregator interface { ... }`
  plugs directly into `GroupBy.Agg` alongside the built-ins — mode,
  percentile, weighted mean, H3-of-centroid, whatever you need,
  without forking the package.
- **Expression IR.** `gobi.Col("price").Mul(gobi.Lit(1.08)).Gt(gobi.Lit(100))`
  builds a data tree, not a chain of already-executed calls.
  `Frame.FilterExpr` and `Frame.WithColumnExpr` evaluate it; a
  `Custom(node ExprNode)` escape hatch lets sibling packages (H3,
  hashes, ML inference) plug in their own expression types alongside
  the built-ins.
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
  size. Filter, Project, WithColumn, Drop, Limit, ScanFrame, and
  ScanFile all stream natively. Aggregate (built-in Kinds) and
  hash-join (Inner/Left/Semi/Anti) run as native streaming
  operators too — no materialization step. Parquet scan
  parallelizes across row-groups; the streaming hash aggregate
  partitions rows across workers by key hash. Both scale to
  `GOMAXPROCS` out of the box. `LazyFrame.ExplainPhysical()` prints
  what strategy each node compiles to (worker counts included).
- **Datetime + timezone-aware ops.** `Timestamp[ns]` columns with
  optional IANA tz label. Component extractors, `AddDuration` /
  `DiffDuration`, comparisons, sub-day + calendar truncation.
  `ResampleEvery(timeCol, interval)` for downsampling and
  `RollingBy(timeCol, period)` for trailing time windows; plus fixed-size
  `Series.RollingSum` / `Mean` / `Min` / `Max` / `Count`.
- **Multi-format I/O.** Every format has `ReadFile` / `WriteFile` at
  Frame level plus a `ScanFile` LazyFrame entry point where the
  underlying source supports it. Formats: CSV (with `.gz` / `.zst`
  / `.bz2` auto-detect), Parquet with proper GeoParquet 1.1
  metadata (snappy / gzip / brotli / zstd / lz4), full RFC 7946
  GeoJSON (every geometry type + XYZ, `.geojsonl` streaming), OGC
  GeoPackage 1.3 (SQLite, RTree spatial index, spec-compliant
  metadata), PostgreSQL / PostGIS (via `pgx/v5`, native `CopyFrom`
  bulk load), **KML read/write**, and **Shapefile read/write**
  (`.shp` + `.shx` + `.dbf` + optional `.prj`).
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
  Polars / pyarrow readers today; gobi's own reader when the query
  optimizer lands).
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

### Reproject

```go
p := geometry.Point{X: -73.9857, Y: 40.7484, CRSValue: geometry.WGS84}
utm, _ := p.EstimateUTMCRS()   // EPSG:32618 (WGS 84 / UTM zone 18N)
proj,  _ := p.ToCRS(utm)       // coordinates now in meters
```

### Buffer + simplify

```go
poly := geometry.SimplePolygon(points, geometry.PseudoMercator)
buffered := poly.Buffer(100, 32) // 100-unit buffer, 32-segment circle approx
simpler  := poly.Simplify(5.0)   // Douglas-Peucker at 5-unit tolerance
```

### R-tree

```go
tree := geometry.NewRTree(bboxes)
hits    := tree.Search(query)      // IDs whose bounds intersect query
nearest := tree.Nearest(x, y, k)   // k closest bounds, sorted
```

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

### KML / Shapefile

```go
// KML → Frame (auto-parses ExtendedData into columns)
places, _ := kmlio.ReadFile("places.kml")
_ = kmlio.WriteFile(places, "out.kml")

// Shapefile → Frame (reads .shp + .shx + .dbf + optional .prj)
counties, _ := shpio.ReadFile("counties")           // no .shp suffix needed
_ = shpio.WriteFile(counties, "counties_out")       // writes all four files
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
| `.../gobi/pgio`           | PostgreSQL / PostGIS via `pgx/v5` — `ReadQuery`/`ReadTable`/`ScanTable` + `WriteTable` with `CopyFrom` bulk load |
| `.../gobi/kmlio`          | Read / write KML (OGC 12-007r2) with Placemarks + ExtendedData                                  |
| `.../gobi/shpio`          | Read / write ESRI Shapefile (`.shp` + `.shx` + `.dbf` + optional `.prj`)                        |

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
  no File Geodatabase support, no polygon Union/Intersection/Difference
  (would require a Vatti / Martinez-Rueda hand-roll), no PROJ-grade
  reprojection beyond WGS84 / Web Mercator / UTM.

## Performance

All numbers Apple M3 Pro, warm cache, 10–20 iterations per op. Fixtures
and scripts live under [`benchmarks/`](benchmarks/) — regenerate with
`go run generate_fixture.go`, `go run generate_csv_fixture.go`, and
`go run generate_spatial_fixture.go`.

### Compute ops (1M-row Parquet, non-spatial)

| Op                            |    gobi | pandas 2.3 | Polars 1T | Polars all |
|-------------------------------|--------:|-----------:|----------:|-----------:|
| `Sum(value_a)`                | 0.92 ms |    0.31 ms |   0.08 ms |    0.08 ms |
| `value_a + value_b`           | 1.01 ms |    0.71 ms |   0.68 ms |    0.76 ms |
| `Filter(value_a > 500k)`      | 14.3 ms |    4.81 ms |   1.74 ms |    1.37 ms |
| `GroupBy(key).Agg(Sum,Mean)`  | 33.3 ms |   18.16 ms |   7.59 ms |    2.24 ms |

Polars 1T = `POLARS_MAX_THREADS=1`; Polars all = default (all cores).
pandas sits between gobi and single-threaded Polars on every op — numpy's
SIMD reductions and C-implemented groupby carry it past pure-Go for now.
See the SIMD note below.

### CSV read (38.6 MB / 1M rows)

| Reader                          |    per-read | notes                                              |
|---------------------------------|------------:|----------------------------------------------------|
| Polars 1.42, all threads        |     9.1 ms  | multi-threaded Rust tokenizer + SIMD (typed schema) |
| Polars 1.42, 1 thread           |    56.0 ms  | SIMD numeric parse, single core                    |
| pandas 2.3, `engine="pyarrow"`  |    25.9 ms  | pyarrow C++ tokenizer                              |
| pandas 2.3, default (C engine)  |   129.8 ms  | pandas' native C tokenizer                         |
| **gobi `csvio.Read`**           | **209.4 ms** | arrow-go's CSV wraps stdlib `encoding/csv`      |

The gap is entirely in stdlib `encoding/csv` allocating a `[]string` per
row + per-cell `strconv` — 99.5% of gobi's CSV allocations show up
there in a pprof run. Closing it means replacing that layer with a
byte-level tokenizer that writes straight into Arrow buffers; not on
the roadmap yet. Maybe the arrow-go folks will pick that up.

### Spatial ops (100k points × 100 polygons)

| Op                              |     gobi | geopandas 1.1 |     result |
|---------------------------------|---------:|--------------:|-----------:|
| Read points.parquet (100k)      |  4.37 ms |      35.2 ms  | **8.0× faster** |
| Read polygons.parquet (100)     |  0.26 ms |       0.76 ms | **2.9× faster** |
| `Area(polygons)`                |  0.02 ms |       0.13 ms | **6.5× faster** |
| `Centroid(polygons)`            |  0.02 ms |       0.16 ms | **8× faster**   |
| `SJoin(100k pts, 100 polys)`    |  3.14 ms |       2.60 ms | 1.2× slower     |

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
| `ReadFile(path, Options{Columns:[id,value_a]})` (eager)     |  6.18 ms | 1.0× (baseline) |
| `ScanFile(path).Select(id,value_a).Collect()` (optimized)   |  7.06 ms | ~1.14× — close to eager |
| `ScanFile(path).Select(id,value_a).CollectRaw()` (no rules) | 14.08 ms | 2.3× slower — reads all 4 cols |

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

| Config                                   |    wall |  user CPU | peak RSS |
|------------------------------------------|--------:|----------:|---------:|
| Serial (`ScanWorkers=1`, single agg)     | 1m 50s  |    147s   |  156 MB  |
| Parallel scan                            | 1m 52s  |    148s   | 1.28 GB  |
| Parallel scan + parallel aggregate.      | 1m 11s  |    197s   | 1.31 GB  |
| **Both + composite-key optimizations**   | **15.5s** | **141s** | **1.5 GB** |
| Polars 1.42 streaming (reference)        |   ~3 s  |     ~15s  | ~4.6 GB  |

Same query, streaming end-to-end — no `LazyFrame.CollectRaw()`
materialization, no disk spill. The gap to Polars closed to 5× across
three complementary changes: partitioning the parquet scan across row-groups,
sharding the streaming hash aggregate by key hash across workers, and
eliminating per-row key allocations in the hot path (reusable scratch buffers +
single-string-key fast path that reads the arrow value zero-copy).

Peak RSS is 3× lower than Polars because gobi keeps at most one
batch per worker in memory (~1.3 GB total on 11 workers) — Polars
buffers larger working sets by design.

### Why the remaining compute-op gap will shrink

`Sum` / `Add` are already memory-bandwidth-bound. The remaining gap on
`Sum` is SIMD reduction (Polars and numpy both use parallel-lane
accumulators). The Go `simd` and `simd/archsimd` packages gain arm64
NEON support in **Go 1.27 (August 2026)**, at which point the existing
`//go:build goexperiment.simd` kernel gets rewritten against the
portable package and closes most of the Sum gap — see
`TODO(1.27-simd)` in `series_ops_simd_*.go`.

The 1BRC gap breaks down similarly: with parallelism landed, most of
the remaining 5× vs Polars is per-row accumulator throughput
(`minMaxAcc.Update` calls `Series.numericAt` per row, dispatching
through an interface + type switch). Vectorized kernels — tight
loops over typed slices — would close a meaningful fraction on the
Go compiler alone; the last factor comes from the same Go 1.27 SIMD
package. Neither is on the roadmap until the toolchain support
lands.

## Development

- `go test -race ./...` should pass before you push.
- Please keep dependencies minimal — Arrow is the one big one on purpose.

## License

MIT. See [LICENSE](LICENSE).

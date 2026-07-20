# gobi

`gobi` is a geospatial dataframe library for Go, built on top of
[Apache Arrow](https://arrow.apache.org). Think of it as a GeoPandas-shaped
API with Polars-shaped internals: columnar, chunk-slice fast paths, and
built around a strongly-typed schema.

> **Status:** early. The API is settled enough to build a small pipeline on;
> semver stability begins with the first tagged release. GeoParquet 1.1
> output has been verified against GeoPandas and QGIS.

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
- **Reprojection engine.** WGS84 ↔ Web Mercator ↔ all 120 UTM zones,
  using the ellipsoidal Redfearn/Snyder formulas. Sub-cm round-trip
  accuracy verified against reference cities worldwide.
- **Spatial index and join.** Static Sort-Tile-Recursive R-tree with
  bounding-box and k-nearest queries. `Frame.SJoin(right, ..., pred)`
  with `SPIntersects` / `SPContains` / `SPWithin` predicates,
  multi-threaded across left rows, tunable via `Workers(n)`.
- **DataFrame ops.** `Filter`, `Take`, `Head`, `Tail`, `WithColumn`,
  `DropColumn`, `GroupBy(...).Agg(count/sum/mean/min/max)`, `Join`
  (inner / left), `Explode`. Series arithmetic (Add/Sub/Mul/Div +
  scalar), comparisons, aggregations — all with single-chunk bulk fast
  paths.
- **User-defined aggregations.** `type Aggregator interface { ... }`
  plugs directly into `GroupBy.Agg` alongside the built-ins — mode,
  percentile, weighted mean, H3-of-centroid, whatever you need,
  without forking the package.
- **Datetime + timezone-aware ops.** `Timestamp[ns]` columns with
  optional IANA tz label. Component extractors, `AddDuration` /
  `DiffDuration`, comparisons, sub-day + calendar truncation.
  `ResampleEvery(timeCol, interval)` for downsampling and
  `RollingBy(timeCol, period)` for trailing time windows; plus fixed-size
  `Series.RollingSum` / `Mean` / `Min` / `Max` / `Count`.
- **Multi-format I/O.** CSV (with `.gz` / `.zst` / `.bz2` auto-detect),
  Parquet with proper GeoParquet 1.1 metadata (snappy / gzip / brotli /
  zstd / lz4), GeoJSON, GeoPackage read, **KML read/write**, and
  **Shapefile read/write** (`.shp` + `.shx` + `.dbf` + optional `.prj`).
- **Streaming readers.** `csvio.ReadFileChunksFunc` and
  `parquetio.ReadFileChunksFunc` yield one Frame per record batch
  (~64k rows), releasing arrow buffers after each callback. Peak
  memory is bounded regardless of source-file size — good for ETL
  over multi-GB inputs.
- **Column projection.** `parquetio.Options{Columns: ...}` skips fetch,
  decompress, and arrow materialization for the columns you don't
  need. Composes with streaming.
- **Parallelism controls.** Package-level `SetMaxParallelism(n)` or
  per-op `Workers(n)`.
- **Pure Go, no cgo.** No GDAL, no GEOS, no libproj. Cross-compiles
  cleanly to every architecture Go supports.

## Install

```bash
go get github.com/zoobst/gobi
```

Requires Go **1.26** or newer.

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
    // Options.Compression overrides.
    df, err := csvio.ReadFile[city]("cities.csv.gz", &csvio.Options{CRSHint: 4326})
    if err != nil { panic(err) }
    defer df.Release()

    // The output file carries a spec-compliant GeoParquet 1.1 metadata
    // blob and reads cleanly in GeoPandas / QGIS.
    _ = parquetio.WriteFile(df, "cities.parquet", parquetio.CodecSnappy)
}
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

### Streaming ETL

```go
// Reads a 5 GB parquet file at ~15 MB peak memory. Only two columns are
// fetched off disk; the rest are never decompressed.
err := parquetio.ReadFileChunksFunc(
    "events.parquet",
    &parquetio.Options{Columns: []string{"user_id", "ts"}, ChunkRows: 64_000},
    func(batch *gobi.Frame) error {
        // Process ~64k rows at a time. The batch is released after
        // return; call batch.Retain() to keep it past this callback.
        return sink.Write(batch)
    },
)
```

CSV has the same shape: `csvio.ReadFileChunksFunc[Row](path, opts, fn)`.

### Derived columns

```go
// A user-space helper produces the derived Series any way it likes
// (external library call, vectorized loop, whatever). WithColumn wires
// it back into the Frame — appending, or replacing an existing column
// of the same name.
lat, _ := df.Column("lat")
lng, _ := df.Column("lng")
cells, _ := h3x.Encode(lat, lng, 9)
df, _ = df.WithColumn("h3", cells)

// And DropColumn for the inverse.
df, _ = df.DropColumn("raw_geometry")
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

```go
pops, _ := df.Column("population")
mask, _ := pops.GtScalar(1_000_000)
big,  _ := df.Filter(mask)
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
| `.../gobi/parquetio`      | Parquet read/write + streaming + column projection; snappy/gzip/brotli/zstd/lz4 + GeoParquet 1.1 |
| `.../gobi/geojson`        | Marshal / unmarshal GeoJSON geometries and Features                                             |
| `.../gobi/gpkg`           | Read features from an OGC GeoPackage (SQLite)                                                   |
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

Two extension points cover most add-on work today, without forking:

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

**Group-by key types.** Hashable key columns: `String`, `Bool`,
`Int32`, `Int64`, `Uint32`, `Uint64`, `Float64`, `Timestamp`.
`Uint64` is what makes H3-cell grouping ergonomic.

## Parallelism

Parallel operations (currently `SJoin`, more to come) resolve their worker
count in this priority order:

1. Per-op `gobi.Workers(n)` option
2. Package default via `gobi.SetMaxParallelism(n)`
3. `GOMAXPROCS`

```go
gobi.SetMaxParallelism(4)                 // process-wide default
df.SJoin(..., gobi.Workers(8))            // override for one call
df.SJoin(..., gobi.Workers(1))            // force sequential
```

## Design constraints

- **Pure Go, no cgo.** GDAL, GEOS, libproj, and other C libraries are
  intentionally off the table. This keeps `go build` clean across every
  platform Go targets and avoids the LGPL/toolchain overhead. The trade:
  no File Geodatabase support, no polygon Union/Intersection/Difference
  (would require a Vatti / Martinez-Rueda hand-roll), no PROJ-grade
  reprojection beyond WGS84 / Web Mercator / UTM.

## Performance

Single-threaded, 1M-row Parquet fixture:

| Op                          | gobi     | Polars 1T | gap    |
|-----------------------------|---------:|----------:|-------:|
| `Sum(value_a)`              |  0.81 ms |   0.08 ms | 10.1×  |
| `value_a + value_b`         |  1.26 ms |   0.66 ms |  1.9×  |
| `Filter(value_a > 500k)`    | 15.1 ms  |   1.61 ms |  9.4×  |
| `GroupBy(key).Agg(Sum,Mean)`| 37.1 ms  |   7.62 ms |  4.9×  |

`Sum` / `Add` are already memory-bandwidth-bound. The remaining gap on
`Sum` is SIMD reduction (Polars uses parallel-lane accumulators). The Go
`simd` and `simd/archsimd` packages gain arm64 NEON support in **Go 1.27
(August 2026)**, at which point the existing `//go:build goexperiment.simd`
kernel gets rewritten against the portable package and closes most of the
Sum gap — see `TODO(1.27-simd)` in `series_ops_simd_*.go`.

## Development

- `go test -race ./...` should pass before you push.
- Please keep dependencies minimal — Arrow is the one big one on purpose.

## License

MIT. See [LICENSE](LICENSE).

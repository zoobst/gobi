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
- **DataFrame ops.** `Filter`, `Take`, `Head`, `Tail`,
  `GroupBy(...).Agg(count/sum/mean/min/max)`, `Join` (inner / left),
  `Explode`. Series arithmetic (Add/Sub/Mul/Div + scalar), comparisons,
  aggregations — all with single-chunk bulk fast paths.
- **GeoParquet 1.1 output.** Proper `geo` metadata blob at file level
  with `bbox`, `geometry_types`, and CRS. Snappy / gzip / brotli / zstd /
  lz4 compression.
- **Parallelism controls.** Package-level `SetMaxParallelism(n)` or
  per-op `Workers(n)`.

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
    df, err := csvio.ReadFile[city]("cities.csv", &csvio.Options{CRSHint: 4326})
    if err != nil { panic(err) }
    defer df.Release()

    // The output file carries a spec-compliant GeoParquet 1.1 metadata
    // blob and reads cleanly in GeoPandas / QGIS.
    _ = parquetio.WriteFile(df, "cities.parquet", parquetio.CodecSnappy)
}
```

### Spatial join

```go
cities, _ := parquetio.ReadFile("cities.parquet")   // 1M points
regions, _ := parquetio.ReadFile("regions.parquet") // 5k polygons

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

## Packages

| Package                   | What it does                                                                                    |
|---------------------------|-------------------------------------------------------------------------------------------------|
| `github.com/zoobst/gobi`  | `Frame`, `Series`, `GroupBy`, `Join`, `SJoin`, `Explode`, options                               |
| `.../gobi/geometry`       | 2D + XYZ primitives, WKB / WKT, CRS + reprojection, predicates, R-tree, Buffer / Simplify / Centroid |
| `.../gobi/csvio`          | Typed CSV read (struct-tag driven)                                                              |
| `.../gobi/parquetio`      | Parquet read/write with snappy/gzip/brotli/zstd/lz4 + GeoParquet 1.1 metadata                   |
| `.../gobi/geojson`        | Marshal / unmarshal GeoJSON geometries and Features                                             |
| `.../gobi/gpkg`           | Read features from an OGC GeoPackage (SQLite)                                                   |

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

## Performance

Single-threaded, 1M-row Parquet fixture, Apple M3 Pro:

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
Sum gap on this hardware — see `TODO(1.27-simd)` in `series_ops_simd_*.go`.

Harnesses live under `benchmarks/` (gitignored). Regenerate the fixture
with `go run benchmarks/generate_fixture.go`, then run either
`go run benchmarks/gobi_bench.go` or
`conda run -n py313 python benchmarks/polars_bench.py`.

## Development

- `go test -race ./...` should pass before you push.
- Please keep dependencies minimal — Arrow is the one big one on purpose.
- Benchmarks live in `benchmarks/` (gitignored) and pair against Polars 1.42+.

## License

MIT. See [LICENSE](LICENSE).

# gobi

`gobi` is a small geospatial dataframe library for Go, built on top of
[Apache Arrow](https://arrow.apache.org). Think of it as a GeoPandas-shaped
API with Polars-shaped internals: columnar, zero-copy where possible, and
built around a strongly-typed schema.

> **Status:** early. The API is settled enough to build a small pipeline on;
> semver stability begins with the first tagged release.

## Highlights

- **Arrow-native.** A `Frame` is a set of Arrow `Column`s. Slicing, `Head`,
  `Tail`, and row selection do not copy data.
- **GeoParquet-compatible geometry columns.** Geometries are stored as WKB
  in Arrow `Binary` columns and tagged in the schema metadata. Writing and
  reading Parquet round-trips both the data and the tag.
- **Real geometry types.** `Point`, `LineString`, `Polygon` (with holes),
  and `MultiPoint`. Area, perimeter, centroid, convex hull, containment,
  and haversine/planar distance all have tests against known values.
- **CSV, Parquet, GeoJSON, GeoPackage.** Small, focused packages per format.

## Install

```bash
go get github.com/zoobst/gobi
```

Requires Go **1.26** or newer.

## Quick start

```go
package main

import (
    "fmt"

    "github.com/zoobst/gobi/csvio"
    "github.com/zoobst/gobi/geometry"
    "github.com/zoobst/gobi/parquetio"
)

type city struct {
    Name       string `csv:"name"`
    Population int64  `csv:"population"`
    Geom       string `csv:"geometry" geom:"true"`
}

func main() {
    df, err := csvio.ReadFile[city]("testdata/cities.csv", &csvio.Options{CRSHint: 4326})
    if err != nil {
        panic(err)
    }
    defer df.Release()

    rows, cols := df.Shape()
    fmt.Printf("%d rows × %d columns\n", rows, cols)

    g, _ := df.Geometry("geometry", 0)
    if p, ok := g.(geometry.Point); ok {
        fmt.Printf("first city @ (%.4f, %.4f)\n", p.X, p.Y)
    }

    _ = parquetio.WriteFile(df, "cities.parquet", parquetio.CodecSnappy)
}
```

## Packages

| Package                        | What it does                                                  |
|--------------------------------|---------------------------------------------------------------|
| `github.com/zoobst/gobi`       | `Frame` + `Series` public API                                 |
| `.../gobi/geometry`            | 2D primitives, WKB/WKT, CRS, area/distance/hull               |
| `.../gobi/csvio`               | Typed CSV read (struct-tag driven)                            |
| `.../gobi/parquetio`           | Parquet read/write with snappy/gzip/brotli/zstd/lz4           |
| `.../gobi/geojson`             | Marshal/unmarshal individual GeoJSON geometries and Features  |
| `.../gobi/gpkg`                | Read features from an OGC GeoPackage (SQLite)                 |

## Geometry columns

`gobi` intentionally does *not* use an Arrow custom-extension type for
geometry. Geometries are Arrow `Binary` columns holding WKB, with the
column marked in schema metadata:

```
"gobi:geometry_type" = "WKB"
"gobi:crs_epsg"      = "4326"
```

This mirrors the GeoParquet spec closely enough that files written by
`gobi` are readable by GeoPandas and other GeoParquet-aware tools for the
primitive geometry types. Use `gobi.GeometryField(name, epsg)` to construct
a tagged field manually.

## Contributing

- `go test -race ./...` should pass before you push.
- Please keep dependencies minimal — Arrow is the one big one on purpose.

## License

MIT. See [LICENSE](LICENSE).

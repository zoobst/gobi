// Package gobi is a geospatial dataframe library for Go, built on Apache Arrow.
//
// A DataFrame is a set of named, equal-length Series backed by Arrow columns.
// Geometry columns are stored as Well-Known Binary (WKB) in Arrow Binary
// arrays and marked as geometries in the schema metadata (following the
// GeoParquet convention). This keeps gobi interoperable with other Arrow-
// aware tools without a custom extension type registry.
//
// # Reading data
//
//	df, err := gobi.ReadCSV("cities.csv", nil)
//	df, err := gobi.ReadParquet("cities.parquet")
//
// # Selecting columns and rows
//
//	col, _ := df.Column("name")
//	head, _ := df.Head(10)
//
// # Working with geometries
//
//	geom, _ := df.Geometry("geometry", 0) // decode the WKB at row 0
//
// See the subpackages for more:
//
//   - geometry:  2D primitives, WKB/WKT, CRS, area/distance/hull
//   - csvio:     typed CSV read/write
//   - parquetio: Parquet + GeoParquet read/write, LazyFrame scan
//   - geojsonio: GeoJSON encoding/decoding for individual features
//   - gpkgio:    OGC GeoPackage read/write with RTree spatial index
//   - kmlio:     KML read/write with ExtendedData attributes
//   - shpio:     ESRI Shapefile read/write
package gobi

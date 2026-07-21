package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// PointsFromXY builds a geometry Series of 2D WKB Points from two
// coordinate columns. x and y must be numeric (Float64, Float32,
// Int64, or Int32) and the same length. Mixed-type inputs are
// promoted to Float64. Null values on either side emit a null
// geometry for that row.
//
// The returned Series is a WKB Binary column tagged with geometry
// metadata + the given EPSG code, so it plugs directly into
// Frame.WithColumn, Frame.SJoin, GeoParquet write paths, and other
// geometry-aware operations.
//
// Modeled on geopandas.points_from_xy — the intended flow is to build
// a geometry column from two attribute columns without hand-rolling
// the WKB encoding:
//
//	lat, _ := df.Column("lat")
//	lng, _ := df.Column("lng")
//	geom, _ := gobi.PointsFromXY(lng, lat, 4326)   // x=lng, y=lat
//	df, _ = df.WithColumn("geometry", geom)
//
// Note the argument order: x first, y second. In geographic
// coordinates that means longitude first, latitude second — matching
// GeoJSON / WKB / shapefile conventions (and geopandas).
func PointsFromXY(x, y Series, crs int32) (Series, error) {
	n := x.Len()
	if y.Len() != n {
		return Series{}, fmt.Errorf("%w: x has %d rows, y has %d",
			ErrColumnLenMismatch, n, y.Len())
	}
	if !x.isNumeric() {
		return Series{}, fmt.Errorf("PointsFromXY: x column: %w", ErrNotNumeric)
	}
	if !y.isNumeric() {
		return Series{}, fmt.Errorf("PointsFromXY: y column: %w", ErrNotNumeric)
	}

	crsVal, _ := geometry.LookupCRS(crs)

	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()

	for i := range n {
		xv, xValid, err := x.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		yv, yValid, err := y.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !xValid || !yValid {
			b.AppendNull()
			continue
		}
		b.Append(geometry.WKB(geometry.Point{X: xv, Y: yv, CRSValue: crsVal}))
	}

	arr := b.NewArray()
	defer arr.Release()
	field := GeometryField("geometry", crs)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked)), nil
}

// PointsFromXYZ is the 3D variant of PointsFromXY. z must be numeric
// and the same length as x and y; rows with a null z produce null
// geometries even if x and y are valid.
//
// The resulting Point geometries carry HasZ=true so downstream WKB
// encoding emits XYZ type codes (1001..) rather than 2D (1..).
func PointsFromXYZ(x, y, z Series, crs int32) (Series, error) {
	n := x.Len()
	if y.Len() != n || z.Len() != n {
		return Series{}, fmt.Errorf("%w: x=%d y=%d z=%d",
			ErrColumnLenMismatch, n, y.Len(), z.Len())
	}
	if !x.isNumeric() || !y.isNumeric() || !z.isNumeric() {
		return Series{}, fmt.Errorf("PointsFromXYZ: %w (all of x, y, z must be numeric)",
			ErrNotNumeric)
	}

	crsVal, _ := geometry.LookupCRS(crs)

	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()

	for i := range n {
		xv, xValid, err := x.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		yv, yValid, err := y.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		zv, zValid, err := z.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !xValid || !yValid || !zValid {
			b.AppendNull()
			continue
		}
		b.Append(geometry.WKB(geometry.Point{
			X: xv, Y: yv, Z: zv, HasZ: true, CRSValue: crsVal,
		}))
	}

	arr := b.NewArray()
	defer arr.Release()
	field := GeometryField("geometry", crs)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked)), nil
}

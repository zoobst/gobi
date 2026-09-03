package gobi

import (
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// BenchmarkSeries_GeomIntersects — Slice 14 bbox-reject fast path
// via the Series driver.
func BenchmarkSeries_GeomIntersects_SoA(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomCol.GeomIntersects(aoi)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSeries_GeomIntersects_LegacyAoS reproduces the
// pre-Slice-14 body inline (no bbox reject; full ParseWKB every
// row). Kept for delta measurement.
func BenchmarkSeries_GeomIntersects_LegacyAoS(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomIntersects(geomCol, aoi)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSeries_GeomDWithin_SoA / LegacyAoS — Slice 15 bbox-min-
// distance reject vs pre-Slice-15 full ParseWKB per row.
func BenchmarkSeries_GeomDWithin_SoA(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomCol.GeomDWithin(aoi, 50)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomDWithin_LegacyAoS(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomDWithin(geomCol, aoi, 50)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func legacyGeomDWithin(s Series, other geometry.Geometry, distance float64) (Series, error) {
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	pool := memory.DefaultAllocator
	bb := array.NewBooleanBuilder(pool)
	defer bb.Release()
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				bb.AppendNull()
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			g = attachCRS(g, crs)
			bb.Append(geometry.WithinDistance(g, other, distance))
		}
	}
	field := arrow.Field{Name: s.name + "_dwithin", Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, bb.NewArray()), nil
}

// BenchmarkSeries_GeomType_SoA / LegacyAoS — Slice 15 WKBTypeCode
// direct vs pre-Slice-15 full ParseWKB per row.
func BenchmarkSeries_GeomType_SoA(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomCol.GeomType()
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomType_LegacyAoS(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomType(geomCol)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func legacyGeomType(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	bb := array.NewStringBuilder(pool)
	defer bb.Release()
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				bb.AppendNull()
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			bb.Append(geometry.TypeString(g))
		}
	}
	field := arrow.Field{Name: s.name + "_geom_type", Type: arrow.BinaryTypes.String, Nullable: true}
	return SeriesFromArray(field, bb.NewArray()), nil
}

// legacyGeomIntersects reproduces the pre-Slice-14 geomPredicateOp
// body (no BoundsFromWKB pre-reject, full ParseWKB per row).
func legacyGeomIntersects(s Series, other geometry.Geometry) (Series, error) {
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	pool := memory.DefaultAllocator
	bb := array.NewBooleanBuilder(pool)
	defer bb.Release()
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				bb.AppendNull()
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			g = attachCRS(g, crs)
			bb.Append(geometry.Test(geometry.PredIntersects, g, other))
		}
	}
	field := arrow.Field{Name: s.name + "_intersects", Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, bb.NewArray()), nil
}

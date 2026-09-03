package gobi

import (
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// BenchmarkSeries_GeomClip_SoA — Slice 17 bbox-reject fast path.
// Corpus is 5k scattered small polygons; the clip mask covers ~1%
// of the corpus bbox so ~99% of rows are bbox-disjoint and take
// the emit-empty fast path.
func BenchmarkSeries_GeomClip_SoA(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	mask := projectedSquare(500, 500, 20)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomCol.GeomClip(mask)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomClip_LegacyAoS(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	mask := projectedSquare(500, 500, 20)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomBinaryOp(geomCol, mask, geometry.OpIntersection, "_clip")
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSeries_GeomDifference_SoA — same corpus, difference
// op. Bbox-disjoint rows re-emit their own WKB (a - b = a).
func BenchmarkSeries_GeomDifference_SoA(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	other := projectedSquare(500, 500, 20)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomCol.GeomDifference(other)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomDifference_LegacyAoS(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	other := projectedSquare(500, 500, 20)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomBinaryOp(geomCol, other, geometry.OpDifference, "_diff")
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSeries_GeomUnion_SoA / LegacyAoS — Slice 21a bbox-
// disjoint fast path. ~99% of rows disjoint from small mask →
// concat-MultiPolygon fast path fires.
func BenchmarkSeries_GeomUnion_SoA(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	other := projectedSquare(500, 500, 20)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomCol.GeomUnion(other)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomUnion_LegacyAoS(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	other := projectedSquare(500, 500, 20)
	geomCol, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomBinaryOp(geomCol, other, geometry.OpUnion, "_union")
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// legacyGeomBinaryOp reproduces the pre-Slice-17 body (no
// BoundsFromWKB reject, full ParseWKB + Boolean per row).
func legacyGeomBinaryOp(s Series, other geometry.Geometry, op geometry.BoolOp, nameSuffix string) (Series, error) {
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	pool := memory.DefaultAllocator
	bb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
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
			result, err := geometry.Boolean(g, other, op, geometry.ClipOptions{})
			if err != nil {
				return Series{}, err
			}
			bb.Append(geometry.WKB(result))
		}
	}
	field := GeometryField(s.name+nameSuffix, epsg)
	return SeriesFromArray(field, bb.NewArray()), nil
}

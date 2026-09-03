package gobi

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// distanceCorpusManyVertex builds nRows polygons each with `verts`
// vertices arranged on a small circle. Same scatter shape as
// distanceCorpus but the per-row ParseWKB cost scales with vertex
// count — the shape that shows the Slice-13 fast-path benefit.
func distanceCorpusManyVertex(b *testing.B, nRows, verts int) (*Frame, geometry.Polygon) {
	b.Helper()
	pool := memory.DefaultAllocator
	rng := rand.New(rand.NewPCG(31, 42))
	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for range nRows {
		cx := rng.Float64() * 950
		cy := rng.Float64() * 950
		pts := make([]geometry.Point, verts+1)
		for i := range verts {
			theta := 2.0 * math.Pi * float64(i) / float64(verts)
			pts[i] = geometry.Point{
				X: cx + 20*math.Cos(theta),
				Y: cy + 20*math.Sin(theta),
			}
		}
		pts[verts] = pts[0]
		g := geometry.Polygon{Rings: [][]geometry.Point{pts}, CRSValue: geometry.PseudoMercator}
		gb.Append(geometry.WKB(g))
	}
	fields := []arrow.Field{
		GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	schema := arrow.NewSchema(fields, nil)
	arr := gb.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := *arrow.NewColumn(fields[0], chunked)
	chunked.Release()
	f, err := NewFrame(schema, []arrow.Column{col})
	if err != nil {
		b.Fatal(err)
	}
	target := projectedSquare(500, 500, 20)
	return f, target
}

func BenchmarkSeries_GeomDistance_10k_64vPolys_SoA(b *testing.B) {
	f, target := distanceCorpusManyVertex(b, 10_000, 64)
	geomSeries, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomSeries.GeomDistance(target, geometry.UnitMeters)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomDistance_10k_64vPolys_LegacyAoS(b *testing.B) {
	f, target := distanceCorpusManyVertex(b, 10_000, 64)
	geomSeries, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomDistance(geomSeries, target, geometry.UnitMeters)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// distanceCorpus builds a corpus of nRows polygons scattered
// over a 1000×1000 plane with `size` per polygon. The distance
// target is a small polygon centered on the plane — most rows'
// bboxes are disjoint from the target, so the Slice-13 fast path
// applies to almost every row.
func distanceCorpus(b *testing.B, nRows int, size float64) (*Frame, geometry.Polygon) {
	b.Helper()
	pool := memory.DefaultAllocator
	rng := rand.New(rand.NewPCG(11, 22))

	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for range nRows {
		x := rng.Float64() * (1000 - size)
		y := rng.Float64() * (1000 - size)
		gb.Append(geometry.WKB(projectedSquare(x, y, size)))
	}

	fields := []arrow.Field{
		GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	schema := arrow.NewSchema(fields, nil)
	arr := gb.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := *arrow.NewColumn(fields[0], chunked)
	chunked.Release()
	f, err := NewFrame(schema, []arrow.Column{col})
	if err != nil {
		b.Fatal(err)
	}
	// Target: a 20x20 polygon at (500, 500). Roughly (10 / 1000)² =
	// 1% of the corpus bbox area, so ~99% of random rows have
	// bboxes disjoint from the target → SoA fast path.
	target := projectedSquare(500, 500, 20)
	return f, target
}

// BenchmarkSeries_GeomDistance_10k_smallPolys exercises the
// Slice-13 wire-in on 10k small (4-vertex) polygons vs a fixed
// small target. Realistic shape for row-filter workloads
// (bbox-disjoint fast path dominates).
func BenchmarkSeries_GeomDistance_10k_smallPolys_SoA(b *testing.B) {
	f, target := distanceCorpus(b, 10_000, 4)
	geomSeries, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomSeries.GeomDistance(target, geometry.UnitMeters)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSeries_GeomDistance_10k_biggerPolys — larger polygons
// mean more bbox overlaps with the target, driving more rows
// through the AoS fallback. Sanity check that the mixed workload
// still wins.
func BenchmarkSeries_GeomDistance_10k_biggerPolys_SoA(b *testing.B) {
	f, target := distanceCorpus(b, 10_000, 50)
	geomSeries, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := geomSeries.GeomDistance(target, geometry.UnitMeters)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSeries_GeomDistance_10k_smallPolys_LegacyAoS forces the
// pre-Slice-13 AoS path (ParseWKB per row → GeomDistance) so we
// can measure the delta. Reproduces the legacy body inline;
// removed if Slice 13's fast-path becomes universally applied.
func BenchmarkSeries_GeomDistance_10k_smallPolys_LegacyAoS(b *testing.B) {
	f, target := distanceCorpus(b, 10_000, 4)
	geomSeries, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomDistance(geomSeries, target, geometry.UnitMeters)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkSeries_GeomDistance_10k_biggerPolys_LegacyAoS(b *testing.B) {
	f, target := distanceCorpus(b, 10_000, 50)
	geomSeries, _ := f.Column("geometry")
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacyGeomDistance(geomSeries, target, geometry.UnitMeters)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// legacyGeomDistance reproduces the pre-Slice-13 Series.GeomDistance
// body (universal AoS ParseWKB → geometry.GeomDistance) so the
// bench captures the SoA wire-in delta.
func legacyGeomDistance(s Series, other geometry.Geometry, u geometry.Unit) (Series, error) {
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	return geomFloat64Op(s, s.name+"_distance", func(g geometry.Geometry) (float64, bool, error) {
		g = attachCRS(g, crs)
		d, err := geometry.GeomDistance(g, other, u)
		if err != nil {
			return 0, false, err
		}
		return d, true, nil
	})
}

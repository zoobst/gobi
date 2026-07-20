package gobi

import (
	"math/rand/v2"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

var sinkFrame *Frame

// buildBenchPolygonGrid returns a frame of gridSize x gridSize non-overlapping
// unit-square polygons tiling a plane.
func buildBenchPolygonGrid(b testing.TB, gridSize int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for y := range gridSize {
		for x := range gridSize {
			nameB.Append("")
			poly := geometry.SimplePolygon([]geometry.Point{
				{X: float64(x), Y: float64(y)},
				{X: float64(x + 1), Y: float64(y)},
				{X: float64(x + 1), Y: float64(y + 1)},
				{X: float64(x), Y: float64(y + 1)},
				{X: float64(x), Y: float64(y)},
			}, geometry.WGS84)
			geomB.Append(geometry.WKB(poly))
		}
	}
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// buildBenchPointCloud returns a frame of n random points uniformly scattered
// across the given [0, extent) x [0, extent) area.
func buildBenchPointCloud(b testing.TB, n int, extent float64) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(0xC0FFEE, 0xF00D))
	for range n {
		nameB.Append("")
		p := geometry.Point{X: rng.Float64() * extent, Y: rng.Float64() * extent}
		geomB.Append(geometry.WKB(p))
	}
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkSJoin_10kPointsIn100Polygons exercises the R-tree pre-filter +
// exact predicate check on a realistic workload.
func BenchmarkSJoin_10kPointsIn100Polygons(b *testing.B) {
	polygons := buildBenchPolygonGrid(b, 10) // 100 polygons over [0,10)x[0,10)
	points := buildBenchPointCloud(b, 10_000, 10)

	b.ReportAllocs()
	for b.Loop() {
		out, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

func BenchmarkSJoin_100kPointsIn10kPolygons(b *testing.B) {
	polygons := buildBenchPolygonGrid(b, 100) // 10,000 polygons over [0,100)x[0,100)
	points := buildBenchPointCloud(b, 100_000, 100)

	b.ReportAllocs()
	for b.Loop() {
		out, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

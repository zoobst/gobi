package gobi

import (
	"math/rand/v2"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// spatialFilterCorpus builds a deterministic single-chunk Frame of
// nRows polygons distributed within [0, 1000) × [0, 1000) with a
// "level" Float64 column (1.0 or 2.0, roughly half each) and a
// "geometry" WKB column. Fixed seed so the benchmark and its
// baselines produce identical row shapes across runs.
func spatialFilterCorpus(b *testing.B, nRows int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	rng := rand.New(rand.NewPCG(1, 2))

	lb := array.NewFloat64Builder(pool)
	defer lb.Release()
	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gb.Release()

	for range nRows {
		var level float64 = 2
		if rng.IntN(2) == 0 {
			level = 1
		}
		lb.Append(level)
		x := rng.Float64() * 990
		y := rng.Float64() * 990
		gb.Append(geometry.WKB(projectedSquare(x, y, 8)))
	}

	fields := []arrow.Field{
		{Name: "level", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{lb.NewArray(), gb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkSpatialFilter_CompoundExpr is the v0.3.4 path — one Filter
// chain, evaluated per request. Re-scans the scalar predicate each
// iteration (no cache).
func BenchmarkSpatialFilter_CompoundExpr(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100) // 10% of the corpus bbox by area
	b.ReportAllocs()

	for b.Loop() {
		out, err := f.Lazy().Filter(
			Col("level").Eq(Lit(1.0)).
				And(Col("geometry").GeomIntersects(Lit(aoi))),
		).Collect()
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSpatialFilter_TwoPhaseEager is the "cached scalar filter,
// per-request geometry filter" pattern the motivation calls out. The
// scalar filter runs once at setup; each iteration does only the
// spatial predicate + mask apply. This is the fastest a caching
// consumer can reasonably go on today's API.
func BenchmarkSpatialFilter_TwoPhaseEager(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100)

	cached, err := f.FilterExpr(Col("level").Eq(Lit(1.0)))
	if err != nil {
		b.Fatal(err)
	}
	geomCol, err := cached.Column("geometry")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		mask, err := geomCol.GeomIntersects(aoi)
		if err != nil {
			b.Fatal(err)
		}
		out, err := cached.Filter(mask)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkSpatialFilter_CachedScalarPlusExpr is the hybrid — the
// scalar filter is still cached, but the per-request geometry
// predicate uses the new Expr form via LazyFrame.Filter. Isolates
// the cost of routing through the Expr executor vs. calling
// Series.GeomIntersects directly.
func BenchmarkSpatialFilter_CachedScalarPlusExpr(b *testing.B) {
	f := spatialFilterCorpus(b, 5000)
	aoi := projectedSquare(100, 100, 100)

	cached, err := f.FilterExpr(Col("level").Eq(Lit(1.0)))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		out, err := cached.Lazy().Filter(
			Col("geometry").GeomIntersects(Lit(aoi)),
		).Collect()
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

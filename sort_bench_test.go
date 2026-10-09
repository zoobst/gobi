package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// BenchmarkBboxCoveringColumns quantifies the write-path bbox
// compute isolated from any sort work. This is the parquetio
// bbox-covering-column write hot path — one call per gobi-written
// geo-parquet file, over every row. Slice 2's BoundsFromWKB fast
// path targets this benchmark directly.
func BenchmarkBboxCoveringColumns(b *testing.B) {
	f := benchGrid(b, 316) // 316×316 = 99,856 ≈ 100k rows
	defer f.Release()

	b.ReportAllocs()
	for b.Loop() {
		aug, _, err := WithBboxCoveringColumns(f)
		if err != nil {
			b.Fatal(err)
		}
		aug.Release()
	}
}

// BenchmarkSortByHilbert exercises the two-pass SortByHilbertWith
// path — one Hilbert-index-per-row centroid extract via WKB.
// Slice 3's CentroidFromWKB fast path targets this benchmark.
func BenchmarkSortByHilbert(b *testing.B) {
	f := benchGrid(b, 316) // ~100k rows
	defer f.Release()

	b.ReportAllocs()
	for b.Loop() {
		sorted, err := f.SortByHilbert("geometry")
		if err != nil {
			b.Fatal(err)
		}
		sorted.Release()
	}
}

// BenchmarkHilbertSortWithCovering exercises the fused single-pass
// path — the sort's centroid extract AND the bbox-covering columns
// share one WKB scan per row. Slice 3's CentroidAndBoundsFromWKB
// fast path targets this benchmark; on parquetio HilbertSort writes
// this is the primary user-visible cost.
func BenchmarkHilbertSortWithCovering(b *testing.B) {
	f := benchGrid(b, 316) // ~100k rows
	defer f.Release()

	b.ReportAllocs()
	for b.Loop() {
		aug, _, err := HilbertSortWithCovering(f, "geometry")
		if err != nil {
			b.Fatal(err)
		}
		aug.Release()
	}
}

// BenchmarkHilbertSort_TwoPassVsFused quantifies the write-path
// speedup from fusing the sort's centroid parse with the bbox-
// covering column parse. Two-pass form parses every row's WKB
// twice; fused form parses once. On a 40k-row corpus with
// non-trivial polygon vertex counts, the fused form should be
// roughly half the wall time.
func BenchmarkHilbertSort_TwoPassVsFused(b *testing.B) {
	f := benchGrid(b, 200) // 40,000 rows
	defer f.Release()

	b.Run("two_pass", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sorted, err := f.SortByHilbert("geometry")
			if err != nil {
				b.Fatal(err)
			}
			aug, _, err := WithBboxCoveringColumns(sorted)
			sorted.Release()
			if err != nil {
				b.Fatal(err)
			}
			aug.Release()
		}
	})

	b.Run("fused", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			aug, _, err := HilbertSortWithCovering(f, "geometry")
			if err != nil {
				b.Fatal(err)
			}
			aug.Release()
		}
	})
}

// benchGrid builds an N×N grid of 8-vertex polygons in row-major
// insertion order. Vertex count picked to make WKB parsing a
// nontrivial share of runtime (a 5-vertex square parses fast enough
// that the sort's own overhead dominates the parse cost).
func benchGrid(b *testing.B, gridSize int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()

	for i := range gridSize {
		for j := range gridSize {
			cx := float64(i) * 10
			cy := float64(j) * 10
			// 8-vertex octagon around (cx, cy) — small enough to parse
			// fast, big enough to matter in aggregate.
			pts := []geometry.Point{
				{X: cx + 1, Y: cy},
				{X: cx + 0.7, Y: cy + 0.7},
				{X: cx, Y: cy + 1},
				{X: cx - 0.7, Y: cy + 0.7},
				{X: cx - 1, Y: cy},
				{X: cx - 0.7, Y: cy - 0.7},
				{X: cx, Y: cy - 1},
				{X: cx + 0.7, Y: cy - 0.7},
				{X: cx + 1, Y: cy},
			}
			geomB.Append(geometry.WKB(geometry.SimplePolygon(pts, geometry.PseudoMercator)))
		}
	}
	field := GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		b.Fatal(err)
	}
	return f
}

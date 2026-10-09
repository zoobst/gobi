package parquetio_test

import (
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

// gridPolygonFrame builds N² 1×1 polygons on an N×N grid,
// SHUFFLED into a spatially-incoherent insertion order using a
// fixed PCG seed. This mirrors the shape a raw shp→parquet dump
// takes on real datasets (see GSHHS_i_all): rows are inserted in
// whatever order the source produced them, which for many spatial
// sources means adjacent rows can be anywhere on the map. Without
// Hilbert sort, each row group's bbox spans most of the plane.
func gridPolygonFrame(b *testing.B, gridSize int) *gobi.Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	rng := rand.New(rand.NewPCG(1, 2))

	// Build a shuffled list of (i, j) cell indices.
	n := gridSize * gridSize
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	rng.Shuffle(n, func(a, b int) { order[a], order[b] = order[b], order[a] })

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, idx := range order {
		i := idx / gridSize
		j := idx % gridSize
		x := float64(i) * 10
		y := float64(j) * 10
		poly := geometry.SimplePolygon([]geometry.Point{
			{X: x, Y: y},
			{X: x + 1, Y: y},
			{X: x + 1, Y: y + 1},
			{X: x, Y: y + 1},
			{X: x, Y: y},
		}, geometry.PseudoMercator)
		geomB.Append(geometry.WKB(poly))
	}
	field := gobi.GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	col := arrow.NewColumn(field, arrow.NewChunked(field.Type, []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := gobi.NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkHilbertPushdown_UnsortedVsSorted quantifies the v0.3.5
// spatial-sort story: on a corpus written in insertion order, the
// pushdown story is muted; after Hilbert pre-sort, an AOI predicate
// prunes a large fraction of the file.
//
// Corpus: 40k polygons spread over a 200×200 grid (bbox
// [0..2000]²), inserted in row-major order (so insertion-order
// row groups span the full X range). AOI is a small corner region.
//
// Reports on both write cost (insertion-order write is trivial,
// Hilbert-sorted write pays for the O(N log N) sort) AND read
// cost (Hilbert-sorted read prunes most row groups, insertion-
// order read prunes almost none).
func BenchmarkHilbertPushdown_UnsortedVsSorted(b *testing.B) {
	df := gridPolygonFrame(b, 200)
	defer df.Release()

	aoi := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0},
		{X: 200, Y: 0},
		{X: 200, Y: 200},
		{X: 0, Y: 200},
		{X: 0, Y: 0},
	}, geometry.PseudoMercator)
	pred := gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi))

	// Write the two file variants once, outside the benchmark loop,
	// then measure just the read+prune path (which is the request-
	// time cost that matters in a serving loop).
	dir := b.TempDir()
	unsortedPath := filepath.Join(dir, "unsorted.parquet")
	sortedPath := filepath.Join(dir, "sorted.parquet")

	if err := parquetio.WriteFile(df, unsortedPath, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 5000,
	}); err != nil {
		b.Fatal(err)
	}
	if err := parquetio.WriteFile(df, sortedPath, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 5000,
		HilbertSort:  true,
	}); err != nil {
		b.Fatal(err)
	}

	b.Run("read_unsorted_no_pushdown", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			f, err := parquetio.ReadFile(unsortedPath, nil)
			if err != nil {
				b.Fatal(err)
			}
			f.Release()
		}
	})
	b.Run("read_unsorted_with_pushdown", func(b *testing.B) {
		opts := &parquetio.ReadOptions{Predicate: pred}
		b.ReportAllocs()
		for b.Loop() {
			f, err := parquetio.ReadFile(unsortedPath, opts)
			if err != nil {
				b.Fatal(err)
			}
			f.Release()
		}
	})
	b.Run("read_sorted_with_pushdown", func(b *testing.B) {
		opts := &parquetio.ReadOptions{Predicate: pred}
		b.ReportAllocs()
		for b.Loop() {
			f, err := parquetio.ReadFile(sortedPath, opts)
			if err != nil {
				b.Fatal(err)
			}
			f.Release()
		}
	})
}

// BenchmarkSpatialPushdown_ClusteredFile writes a spatially-clustered
// two-cluster corpus (like the correctness test), then measures the
// per-request read time with and without the predicate hint. The
// pushdown skips half the file, so read time roughly halves. This is
// the mechanism working at its intended shape — a spatially-sorted
// GeoParquet in production would see similar wins per AOI.
func BenchmarkSpatialPushdown_ClusteredFile(b *testing.B) {
	clusterA := geometry.Point{X: 10, Y: 10}
	clusterB := geometry.Point{X: 5000, Y: 5000}
	df := spatialFrameB(b, 10_000, clusterA, clusterB)
	defer df.Release()

	path := filepath.Join(b.TempDir(), "spatial_bench.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 5000, // → 2 row groups, one per cluster
	}); err != nil {
		b.Fatal(err)
	}

	aoi := geometry.SimplePolygon([]geometry.Point{
		{X: -100, Y: -100},
		{X: 500, Y: -100},
		{X: 500, Y: 500},
		{X: -100, Y: 500},
		{X: -100, Y: -100},
	}, geometry.PseudoMercator)

	b.Run("no_pushdown", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			f, err := parquetio.ReadFile(path, nil)
			if err != nil {
				b.Fatal(err)
			}
			f.Release()
		}
	})

	b.Run("pushdown", func(b *testing.B) {
		pred := gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi))
		b.ReportAllocs()
		for b.Loop() {
			f, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
				Predicate: pred,
			})
			if err != nil {
				b.Fatal(err)
			}
			f.Release()
		}
	})
}

// spatialFrameB is the *testing.B variant of the spatialFrame helper
// from spatial_pushdown_test.go — same shape, different receiver type.
func spatialFrameB(b *testing.B, N int, clusterA, clusterB geometry.Point) *gobi.Frame {
	b.Helper()
	// Delegate through the testing.TB interface by using a t-shim.
	return spatialFrameTB(b, N, clusterA, clusterB)
}

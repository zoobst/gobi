package parquetio_test

import (
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

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

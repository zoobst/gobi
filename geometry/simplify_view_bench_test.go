package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// Bench sizes match the wkb / rtree grids.
var simplifyBenchSizes = []int{64, 1_024, 65_536, 1_000_000}

// makeBenchPolylineXY builds a wiggly polyline that survives DP at
// moderate tolerance — an amplitude of 1.0 with 100-unit spacing
// gives a curvature that ~50% of vertices survive at tol=0.1 and
// ~10% survive at tol=1.0. This is the shape most DP callers see
// on real polylines (coastlines, contour lines) — enough real
// splits to exercise the recursion depth, not so many that every
// vertex survives.
func makeBenchPolylineXY(n int) (xs, ys, xs2, ys2 []float64, pts []Point) {
	rng := rand.New(rand.NewSource(int64(n)))
	xs = make([]float64, n)
	ys = make([]float64, n)
	pts = make([]Point, n)
	for i := range n {
		x := float64(i) * 0.5
		y := math.Sin(float64(i)*0.1)*10 + rng.Float64()*0.5
		xs[i] = x
		ys[i] = y
		pts[i] = Point{X: x, Y: y}
	}
	xs2 = append([]float64(nil), xs...)
	ys2 = append([]float64(nil), ys...)
	return
}

// BenchmarkSimplifyDP_AoS_douglasPeucker — pre-Slice-9 baseline.
// Recursive AoS on []Point.
func BenchmarkSimplifyDP_AoS_douglasPeucker(b *testing.B) {
	for _, n := range simplifyBenchSizes {
		_, _, _, _, pts := makeBenchPolylineXY(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				out := douglasPeucker(pts, 0.5)
				sink += len(out)
			}
			_ = sink
		})
	}
}

// BenchmarkSimplifyDP_SoA_FromXY — Slice-9 iterative form on
// parallel slabs. Fewer allocations (bitmap + output slabs
// only, no per-split appends).
func BenchmarkSimplifyDP_SoA_FromXY(b *testing.B) {
	for _, n := range simplifyBenchSizes {
		xs, ys, _, _, _ := makeBenchPolylineXY(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				ox, _ := SimplifyDPFromXY(xs, ys, 0.5)
				sink += len(ox)
			}
			_ = sink
		})
	}
}

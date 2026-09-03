package compute

import (
	"math"
	"math/rand/v2"
	"testing"
)

// Bench sizes covering the range from below-lane-threshold to
// coastline-scale rings. Match the geometry-side sizes so wins
// can be cross-referenced.
var geomBenchSizes = []int{4, 8, 64, 1_024, 65_536, 1_000_000}

func buildBenchXY(n int) (xs, ys []float64) {
	rng := rand.New(rand.NewPCG(uint64(n), 0x1234))
	xs = make([]float64, n)
	ys = make([]float64, n)
	for i := range xs {
		xs[i] = rng.Float64() * 1000
		ys[i] = rng.Float64() * 1000
	}
	return
}

func buildBenchConvexRing(n int) (xs, ys []float64) {
	// Convex closed ring so PolygonCentroidShoelace hits the primary
	// (non-fallback) formula path. Extra vertex duplicates the first
	// so the ring is properly closed.
	xs = make([]float64, n+1)
	ys = make([]float64, n+1)
	for i := range n {
		theta := 2.0 * math.Pi * float64(i) / float64(n)
		xs[i] = 500 + 400*math.Cos(theta)
		ys[i] = 500 + 400*math.Sin(theta)
	}
	xs[n] = xs[0]
	ys[n] = ys[0]
	return
}

// BenchmarkBoundsF64_scalar_vs_simd — Phase 6a validation.
// Directly measures the compute-package kernel without going
// through geometry.BoundsFromXY. Same bench under both builds
// (go test / GOEXPERIMENT=simd go test) — the delta is the
// SIMD win.
func BenchmarkBoundsF64(b *testing.B) {
	for _, n := range geomBenchSizes {
		xs, ys := buildBenchXY(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sinkMinX float64
			for i := 0; i < b.N; i++ {
				mx, _, _, _, _ := BoundsF64(xs, ys)
				sinkMinX = mx
			}
			_ = sinkMinX
		})
	}
}

// BenchmarkPolygonCentroidShoelace_scalar_vs_simd — Phase 6b
// validation. Convex closed ring so the primary formula fires.
func BenchmarkPolygonCentroidShoelace(b *testing.B) {
	for _, n := range geomBenchSizes {
		xs, ys := buildBenchConvexRing(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sinkCx float64
			for i := 0; i < b.N; i++ {
				cx, _, _ := PolygonCentroidShoelace(xs, ys)
				sinkCx = cx
			}
			_ = sinkCx
		})
	}
}

// BenchmarkPIPCrossingCount — Phase 6c validation target.
// Currently scalar in both builds; SIMD landing in Phase 6c will
// show up as a delta between GOEXPERIMENT=simd and default.
func BenchmarkPIPCrossingCount(b *testing.B) {
	// Fixed query point at the ring centroid — always inside so
	// the crossing count exercises the "found a crossing" branch
	// per segment. Realistic mix would test both inside/outside.
	tx, ty := 500.0, 500.0
	for _, n := range geomBenchSizes {
		xs, ys := buildBenchConvexRing(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink bool
			for i := 0; i < b.N; i++ {
				sink = PIPCrossingCount(xs, ys, tx, ty)
			}
			_ = sink
		})
	}
}

func sizeLabel(n int) string {
	switch n {
	case 4:
		return "n=4"
	case 8:
		return "n=8"
	case 64:
		return "n=64"
	case 1_024:
		return "n=1K"
	case 65_536:
		return "n=64K"
	case 1_000_000:
		return "n=1M"
	}
	return "n=?"
}

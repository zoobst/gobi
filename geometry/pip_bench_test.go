package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// Bench sizes covering realistic PIP-hot-path shapes:
//
//	n=5      — the rectangular AOI (closed square)
//	n=64     — hand-drawn AOI
//	n=1_024  — mid-detail admin boundary
//	n=65_536 — coastline / high-res boundary
var pipBenchSizes = []int{5, 64, 1_024, 65_536}

// buildPIPBenchPolygon returns a random-vertex n-point closed
// ring centered at (500, 500) with radius ~400. Deterministic
// seed. Prefixed with PIP so it doesn't collide with
// buildBenchPolygon in wkb_bench_test.go.
func buildPIPBenchPolygon(n int) ([]Point, []float64, []float64) {
	rng := rand.New(rand.NewSource(int64(n) ^ 0x504950))
	pts := make([]Point, n+1)
	for i := range n {
		// Perturb around a rough circular walk to give a
		// non-degenerate ring.
		theta := 2.0 * math.Pi * float64(i) / float64(n)
		r := 300 + rng.Float64()*100
		pts[i] = Point{X: 500 + r*math.Cos(theta), Y: 500 + r*math.Sin(theta)}
	}
	pts[n] = pts[0] // close the ring
	xs, ys := ringToXY(pts)
	return pts, xs, ys
}

// Test query points — inside, outside, and near-boundary. Multiple
// query points per iter so per-iter overhead is amortized and we
// really measure the PIP kernel cost.
var pipQueries = [][2]float64{
	{500, 500}, {200, 200}, {800, 800}, {50, 50}, {950, 950},
	{500, 100}, {500, 900}, {100, 500}, {900, 500}, {600, 400},
}

// BenchmarkPIP_AoS_pointInRing — pre-Slice-4 baseline. Runs the
// AoS pointInRing kernel over `[]Point` for each query.
func BenchmarkPIP_AoS_pointInRing(b *testing.B) {
	for _, n := range pipBenchSizes {
		pts, _, _ := buildPIPBenchPolygon(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				for _, q := range pipQueries {
					if pointInRing(Point{X: q[0], Y: q[1]}, pts) {
						sink++
					}
				}
			}
			_ = sink
		})
	}
}

// BenchmarkPIP_SoA_PIPRingFromXY — Slice-4 SoA fast path. Runs
// the even-odd crossing kernel over parallel Xs/Ys slabs.
func BenchmarkPIP_SoA_PIPRingFromXY(b *testing.B) {
	for _, n := range pipBenchSizes {
		_, xs, ys := buildPIPBenchPolygon(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				for _, q := range pipQueries {
					if PIPRingFromXY(xs, ys, q[0], q[1]) {
						sink++
					}
				}
			}
			_ = sink
		})
	}
}

// BenchmarkPIP_ManyPointsPerPolygon simulates the spatial-join
// refine loop shape: one polygon, many candidate points to test.
// The three variants isolate where the SoA slice pays off:
//
//   - AoS: p.Contains(pt) per point — walks []Point per call
//   - SoA_freshView: RingViews() + PIPPolygonFromRings per point.
//     Fresh materialization every call — worst case, pure overhead.
//   - SoA_heldView: RingViews() once + PIPPolygonFromRings per
//     point. Amortization pattern — the shape SJoin's refine loop
//     should adopt.
//
// The n-points-per-polygon axis is where Slice 4's win actually
// lands: at 100 candidates per polygon (typical SJoin R-tree
// output), held-view should beat AoS by the SoA-loop-vs-AoS
// margin from PIPRingFromXY, undiluted by materialization.
func BenchmarkPIP_ManyPointsPerPolygon(b *testing.B) {
	const polyVerts = 64
	const pointsPerPoly = 100
	pts, _, _ := buildPIPBenchPolygon(polyVerts)
	poly := Polygon{Rings: [][]Point{pts}}

	// Deterministic candidate points scattered around the polygon.
	rng := rand.New(rand.NewSource(0xAA55))
	candidates := make([]Point, pointsPerPoly)
	for i := range candidates {
		candidates[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}

	b.Run("AoS", func(b *testing.B) {
		b.ReportAllocs()
		var sink int
		for i := 0; i < b.N; i++ {
			for _, pt := range candidates {
				if poly.Contains(pt) {
					sink++
				}
			}
		}
		_ = sink
	})

	b.Run("SoA_freshView", func(b *testing.B) {
		b.ReportAllocs()
		var sink int
		for i := 0; i < b.N; i++ {
			for _, pt := range candidates {
				rings := poly.RingViews()
				if PIPPolygonFromRings(rings, pt.X, pt.Y) {
					sink++
				}
			}
		}
		_ = sink
	})

	b.Run("SoA_heldView", func(b *testing.B) {
		b.ReportAllocs()
		rings := poly.RingViews()
		var sink int
		for i := 0; i < b.N; i++ {
			for _, pt := range candidates {
				if PIPPolygonFromRings(rings, pt.X, pt.Y) {
					sink++
				}
			}
		}
		_ = sink
	})
}

// BenchmarkPIP_FromWKB — WKB-shaped fast path. Includes the
// per-call byte-stream scan overhead; measures what a caller
// holding raw WKB (arrow-backed polygon column) sees.
func BenchmarkPIP_FromWKB(b *testing.B) {
	for _, n := range pipBenchSizes {
		pts, _, _ := buildPIPBenchPolygon(n)
		data := WKB(Polygon{Rings: [][]Point{pts}})
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				for _, q := range pipQueries {
					in, err := PIPFromWKB(data, q[0], q[1])
					if err != nil {
						b.Fatal(err)
					}
					if in {
						sink++
					}
				}
			}
			_ = sink
		})
	}
}

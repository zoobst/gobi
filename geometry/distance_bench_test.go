package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// buildBenchPolygonPair builds two disjoint n-vertex convex
// polygons for the min-distance bench. Positioned so the closest
// approach is between the interior edges — exercises the full
// nested inner loop, not an early-out on the first vertex-pair.
func buildBenchPolygonPair(n int) (a, b Polygon) {
	rng := rand.New(rand.NewSource(int64(n)))
	build := func(cx, cy float64) Polygon {
		pts := make([]Point, n+1)
		for i := range n {
			theta := 2.0 * math.Pi * float64(i) / float64(n)
			pts[i] = Point{
				X: cx + 10*math.Cos(theta) + rng.Float64()*0.1,
				Y: cy + 10*math.Sin(theta) + rng.Float64()*0.1,
			}
		}
		pts[n] = pts[0]
		return Polygon{Rings: [][]Point{pts}}
	}
	return build(0, 0), build(30, 0)
}

// BenchmarkPlanarMinDistance_PolyVsPoly — the shape that
// Series.GeomDistance drives per row when the two sides are
// polygons. Pre-Slice-11 baseline (BenchmarkPlanarMinDistance_LegacyAoS
// below): forEachVertex+forEachSegment closure loop with per-pair
// math.Hypot. Post-Slice-11: slab-form nested loop with squared-
// distance tracking + one final sqrt.
var distSink float64

func BenchmarkPlanarMinDistance_PolyVsPoly_SoA(b *testing.B) {
	sizes := []int{16, 64, 256, 1024}
	for _, n := range sizes {
		polyA, polyB := buildBenchPolygonPair(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				distSink += planarMinDistance(polyA, polyB)
			}
		})
	}
}

// BenchmarkPlanarMinDistance_PolyVsPoly_LegacyAoS — kept as the
// pre-Slice-11 baseline reference. The body is the original
// closure-based path. Not called from planarMinDistance in
// production; only the bench + parity oracle in distance_view_test.go
// use it.
func BenchmarkPlanarMinDistance_PolyVsPoly_LegacyAoS(b *testing.B) {
	sizes := []int{16, 64, 256, 1024}
	for _, n := range sizes {
		polyA, polyB := buildBenchPolygonPair(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				distSink += aosPlanarMinDistanceOracle(polyA, polyB)
			}
		})
	}
}

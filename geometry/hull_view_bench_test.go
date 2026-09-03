package geometry

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

var hullBenchSizes = []int{16, 64, 1_024, 65_536, 1_000_000}

// makeHullBenchPolygon builds a "wobbly circle" polygon: n
// vertices on a unit circle with small random radial perturbation
// so ~O(sqrt(n)) survive the hull. Matches the shape hull
// callers see on real data (dense polylines with a limited convex
// envelope).
func makeHullBenchPolygon(n int) Polygon {
	rng := rand.New(rand.NewSource(int64(n)))
	pts := make([]Point, n+1)
	for i := range n {
		theta := 2.0 * math.Pi * float64(i) / float64(n)
		r := 100 + rng.Float64()*5
		pts[i] = Point{X: r * math.Cos(theta), Y: r * math.Sin(theta)}
	}
	pts[n] = pts[0]
	return Polygon{Rings: [][]Point{pts}}
}

// BenchmarkConvexHull_AoS_Graham — pre-Slice-12 baseline.
// The original body inlined here so the bench survives even after
// Polygon.ConvexHull is rewritten.
func BenchmarkConvexHull_AoS_Graham(b *testing.B) {
	for _, n := range hullBenchSizes {
		poly := makeHullBenchPolygon(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				h := grahamScanAoSLegacy(poly)
				sink += len(h.Rings[0])
			}
			_ = sink
		})
	}
}

// BenchmarkConvexHull_SoA_Andrew — Slice-12 kernel via
// Polygon.ConvexHull (which now routes through
// ConvexHullFromXY).
func BenchmarkConvexHull_SoA_Andrew(b *testing.B) {
	for _, n := range hullBenchSizes {
		poly := makeHullBenchPolygon(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				h := poly.ConvexHull()
				sink += len(h.Rings[0])
			}
			_ = sink
		})
	}
}

// grahamScanAoSLegacy reproduces the pre-Slice-12
// Polygon.ConvexHull body (Graham scan with sort.Slice on
// []Point). Kept in the bench file so we can measure the delta
// without maintaining two production copies.
func grahamScanAoSLegacy(p Polygon) Polygon {
	src := p.Exterior()
	if len(src) < 3 {
		return Polygon{Rings: [][]Point{append([]Point(nil), src...)}, CRSValue: p.CRSValue}
	}
	pts := append([]Point(nil), src...)
	lowIdx := 0
	for i, pt := range pts {
		if pt.Y < pts[lowIdx].Y || (pt.Y == pts[lowIdx].Y && pt.X < pts[lowIdx].X) {
			lowIdx = i
		}
	}
	pts[0], pts[lowIdx] = pts[lowIdx], pts[0]
	pivot := pts[0]
	rest := pts[1:]
	sort.Slice(rest, func(i, j int) bool {
		c := crossPts(pivot, rest[i], rest[j])
		if c == 0 {
			return distSqPts(pivot, rest[i]) < distSqPts(pivot, rest[j])
		}
		return c > 0
	})
	hull := make([]Point, 0, len(pts))
	hull = append(hull, pivot)
	for _, pt := range rest {
		for len(hull) >= 2 && crossPts(hull[len(hull)-2], hull[len(hull)-1], pt) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, pt)
	}
	return Polygon{Rings: [][]Point{closedRing(hull)}, CRSValue: p.CRSValue}
}

func crossPts(o, a, b Point) float64 {
	return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X)
}

func distSqPts(a, b Point) float64 {
	dx, dy := a.X-b.X, a.Y-b.Y
	return dx*dx + dy*dy
}

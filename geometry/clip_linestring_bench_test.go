package geometry

import (
	"math"
	"testing"
)

// hexagon returns a convex 6-vertex regular polygon centered at (cx, cy)
// with circumradius r — a stand-in for an h3 cell polygon.
func hexagon(cx, cy, r float64) Polygon {
	return regularPolygon(cx, cy, r, 6)
}

// zigzag returns a linestring with n vertices oscillating around y=0
// across an x-range of xRange units. Used to exercise the "long line,
// small polygon" workload where most segments miss the polygon and the
// bbox short-circuit fires.
func zigzag(n int, xRange, amp float64) LineString {
	pts := make([]Point, n)
	for i := range n {
		t := float64(i) / float64(n-1)
		pts[i] = Point{
			X: t * xRange,
			Y: amp * math.Sin(t*10*math.Pi),
		}
	}
	return NewLineString(pts, CRS{})
}

// BenchmarkLineStringClip_HexAgainst100Vertex measures the Overture-target
// case: a 100-vertex linestring clipped against a hexagon covering ~1/10
// of its extent. Short-circuit fires on most segments.
func BenchmarkLineStringClip_HexAgainst100Vertex(b *testing.B) {
	line := zigzag(100, 100, 5)
	hex := hexagon(50, 0, 5)

	b.ReportAllocs()
	for b.Loop() {
		_ = line.Clip(hex)
	}
}

// BenchmarkLineStringClip_HexAgainst1000Vertex stresses the same shape
// with a longer linestring — bbox reject should keep this near-linear in
// vertices-inside-the-cell, not total vertices.
func BenchmarkLineStringClip_HexAgainst1000Vertex(b *testing.B) {
	line := zigzag(1000, 1000, 5)
	hex := hexagon(500, 0, 5)

	b.ReportAllocs()
	for b.Loop() {
		_ = line.Clip(hex)
	}
}

// BenchmarkLineStringClip_FullyInside pins down the worst case for the
// bbox short-circuit: linestring lies entirely inside the polygon, so the
// four-compare AABB test never fires and is pure overhead. If this
// regresses vs. the un-short-circuited version, the check isn't paying
// for itself.
func BenchmarkLineStringClip_FullyInside(b *testing.B) {
	// Linestring inside a large hexagon.
	pts := make([]Point, 100)
	for i := range pts {
		t := float64(i) / 99
		pts[i] = pt(t*10, math.Sin(t*4*math.Pi)*2)
	}
	line := NewLineString(pts, CRS{})
	hex := hexagon(5, 0, 20)

	b.ReportAllocs()
	for b.Loop() {
		_ = line.Clip(hex)
	}
}

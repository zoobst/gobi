package geometry

import (
	"math"
	"testing"
)

// regularPolygon returns a Polygon approximating a circle of radius r
// centered at (cx, cy) using n edges.
func regularPolygon(cx, cy, r float64, n int) Polygon {
	pts := make([]Point, n+1)
	for i := range n {
		theta := 2 * math.Pi * float64(i) / float64(n)
		pts[i] = pt(cx+r*math.Cos(theta), cy+r*math.Sin(theta))
	}
	pts[n] = pts[0]
	return SimplePolygon(pts, CRS{})
}

// BenchmarkClip_ConvexFastPath measures the Sutherland-Hodgman fast
// path (both operands convex Polygons). BenchmarkClip_ConvexSweepPath
// runs the same shapes but wraps one side in a MultiPolygon so the
// convex-check in Boolean() falls through to the general sweep. The
// difference is the fast-path win.
func BenchmarkClip_ConvexFastPath(b *testing.B) {
	a := regularPolygon(0, 0, 10, 8)
	c := regularPolygon(5, 3, 10, 8)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(a, c)
	}
}

func BenchmarkClip_ConvexSweepPath(b *testing.B) {
	a := regularPolygon(0, 0, 10, 8)
	c := MultiPolygon{Polygons: []Polygon{regularPolygon(5, 3, 10, 8)}}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(a, c)
	}
}

func BenchmarkClip_16x16(b *testing.B) {
	a := regularPolygon(0, 0, 10, 16)
	c := regularPolygon(5, 5, 10, 16)

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(a, c)
	}
}

func BenchmarkClip_64x64(b *testing.B) {
	a := regularPolygon(0, 0, 10, 64)
	c := regularPolygon(5, 5, 10, 64)

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(a, c)
	}
}

func BenchmarkClip_256x256(b *testing.B) {
	a := regularPolygon(0, 0, 10, 256)
	c := regularPolygon(5, 5, 10, 256)

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(a, c)
	}
}

func BenchmarkClip_SquareVsSquare(b *testing.B) {
	a := unitSquare(0, 0, 10)
	c := unitSquare(5, 5, 10)

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(a, c)
	}
}

// BenchmarkClip_MCellLoop simulates the customer's "M-cell loop" pattern:
// a single user polygon clipped against many small cells.
func BenchmarkClip_MCellLoop(b *testing.B) {
	user := regularPolygon(50, 50, 40, 64)
	cells := make([]Polygon, 0, 100)
	for i := range 10 {
		for j := range 10 {
			cells = append(cells, unitSquare(float64(i*10), float64(j*10), 10))
		}
	}

	b.ReportAllocs()
	for b.Loop() {
		for _, c := range cells {
			_, _ = Clip(user, c)
		}
	}
}

func BenchmarkUnion_16x16(b *testing.B) {
	a := regularPolygon(0, 0, 10, 16)
	c := regularPolygon(5, 5, 10, 16)

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Union(a, c)
	}
}

func BenchmarkDissolve_100Disjoint(b *testing.B) {
	geoms := make([]Geometry, 100)
	for i := range geoms {
		geoms[i] = unitSquare(float64(i)*20, 0, 10)
	}

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Dissolve(geoms)
	}
}

func BenchmarkDissolve_100Overlapping(b *testing.B) {
	geoms := make([]Geometry, 100)
	for i := range geoms {
		geoms[i] = unitSquare(float64(i)*5, 0, 10)
	}

	b.ReportAllocs()
	for b.Loop() {
		_, _ = Dissolve(geoms)
	}
}

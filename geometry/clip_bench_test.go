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

// BenchmarkClip_ContainmentFastPath — Slice 18. Subject (small
// cell) fully inside convex clipper (big disc). Pre-Slice-18 this
// ran the full Sutherland-Hodgman clipper and allocated a fresh
// output ring; post-Slice-18 the containment check short-circuits
// and returns the subject unchanged.
func BenchmarkClip_ContainmentFastPath(b *testing.B) {
	disc := regularPolygon(50, 50, 40, 64) // convex, big
	cellInside := unitSquare(45, 45, 5)    // fully inside disc
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(cellInside, disc)
	}
}

// BenchmarkClip_ContainmentAoS — for comparison. Reproduces the
// pre-Slice-18 shape: both convex → SH fast path (not the containment
// short-circuit). Same shapes as ContainmentFastPath.
func BenchmarkClip_ContainmentSHOnly(b *testing.B) {
	disc := regularPolygon(50, 50, 40, 64)
	cellInside := unitSquare(45, 45, 5)
	b.ReportAllocs()
	for b.Loop() {
		_ = sutherlandHodgman(cellInside.Rings[0], disc.Rings[0], CRS{})
	}
}

// BenchmarkClip_RelaxedSH_LShapeInAOI — Slice 19. Concave L-shape
// clipped by a convex AOI. Pre-Slice-19 this took the full
// Martinez sweep (both convex gate rejected the L); post-Slice-19
// the transition-count gate lets SH run when intersection is
// simply connected. Note the AOI position matters: an AOI that
// hits the horizontal arm only produces a single-component
// intersection (SH safe); one that spans the concavity produces
// 2 components and falls back to sweep.
func BenchmarkClip_RelaxedSH_LShapeInAOI(b *testing.B) {
	lShape := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 4},
		{X: 4, Y: 4}, {X: 4, Y: 10}, {X: 0, Y: 10},
		{X: 0, Y: 0},
	}}}
	aoi := unitSquare(1, 1, 5) // hits horizontal arm only
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(lShape, aoi)
	}
}

// BenchmarkClip_RelaxedSH_LShapeInAOI_SweepBaseline — reproduces
// the pre-Slice-19 path by wrapping the AOI in a MultiPolygon,
// forcing the sweep. Delta between this and the SoA bench above
// is the Slice-19 win.
func BenchmarkClip_RelaxedSH_LShapeInAOI_SweepBaseline(b *testing.B) {
	lShape := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 4},
		{X: 4, Y: 4}, {X: 4, Y: 10}, {X: 0, Y: 10},
		{X: 0, Y: 0},
	}}}
	aoi := MultiPolygon{Polygons: []Polygon{unitSquare(1, 1, 5)}}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Clip(lShape, aoi)
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

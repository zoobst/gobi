package geometry

import (
	"math"
	"testing"
)

func TestWithinDistance_PointPoint(t *testing.T) {
	a := Point{X: 0, Y: 0}
	b := Point{X: 3, Y: 4} // distance = 5
	cases := []struct {
		d    float64
		want bool
	}{
		{4.9, false},
		{5.0, true}, // inclusive
		{5.1, true},
		{0, false}, // same as Intersects: not intersecting
		{100, true},
	}
	for _, c := range cases {
		if got := WithinDistance(a, b, c.d); got != c.want {
			t.Errorf("d=%v: got %v, want %v", c.d, got, c.want)
		}
	}
}

// TestWithinDistance_ZeroIsIntersects — d=0 must match Intersects
// (identical points → true, disjoint → false, touching → true).
func TestWithinDistance_ZeroIsIntersects(t *testing.T) {
	sq := func(x, y, size float64) Polygon {
		return SimplePolygon([]Point{
			{X: x, Y: y},
			{X: x + size, Y: y},
			{X: x + size, Y: y + size},
			{X: x, Y: y + size},
			{X: x, Y: y},
		}, CRS{})
	}
	overlap := sq(0, 0, 10)
	touching := sq(10, 0, 5)  // touches overlap on right edge
	disjoint := sq(50, 50, 5) // far away

	if !WithinDistance(overlap, touching, 0) {
		t.Error("touching squares at d=0: expected true (edge-touching = Intersects)")
	}
	if WithinDistance(overlap, disjoint, 0) {
		t.Error("disjoint squares at d=0: expected false")
	}
}

// TestWithinDistance_BboxShortCircuit — the whole point of DWithin's
// perf story. Two far-apart polygons with lots of vertices should
// bail out at the bbox check without walking edges. We can't directly
// count edge visits, but we can verify correctness of the fast path
// (returns false for provably-too-far pairs).
func TestWithinDistance_BboxShortCircuit(t *testing.T) {
	// Two 1000-vertex polygons 10,000 units apart.
	buildDensePoly := func(cx, cy float64) Polygon {
		pts := make([]Point, 1001)
		for i := range 1000 {
			theta := 2 * math.Pi * float64(i) / 1000
			pts[i] = Point{X: cx + math.Cos(theta), Y: cy + math.Sin(theta)}
		}
		pts[1000] = pts[0]
		return SimplePolygon(pts, CRS{})
	}
	a := buildDensePoly(0, 0)
	b := buildDensePoly(10_000, 0)
	if WithinDistance(a, b, 100) {
		t.Error("10,000-unit apart polygons within distance 100: expected false")
	}
	if !WithinDistance(a, b, 10_000) {
		t.Error("10,000-unit apart polygons within distance 10,000: expected true (bboxes ~9998 apart, minus radius)")
	}
}

// TestWithinDistance_PointPolygon — point inside polygon → 0
// distance, so any non-negative d passes.
func TestWithinDistance_PointPolygon(t *testing.T) {
	poly := SimplePolygon([]Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}, CRS{})
	inside := Point{X: 5, Y: 5}
	nearBoundary := Point{X: 11, Y: 5} // 1 unit outside +X edge
	far := Point{X: 50, Y: 50}

	if !WithinDistance(inside, poly, 0) {
		t.Error("point inside polygon at d=0: expected true")
	}
	if !WithinDistance(nearBoundary, poly, 1) {
		t.Error("point 1 unit outside boundary at d=1: expected true (inclusive)")
	}
	if WithinDistance(nearBoundary, poly, 0.5) {
		t.Error("point 1 unit outside boundary at d=0.5: expected false")
	}
	if WithinDistance(far, poly, 5) {
		t.Error("far point at d=5: expected false")
	}
}

// TestWithinDistance_NegativeD / NaN → false.
func TestWithinDistance_InvalidD(t *testing.T) {
	a := Point{X: 0, Y: 0}
	b := Point{X: 0, Y: 0} // identical
	if WithinDistance(a, b, -1) {
		t.Error("negative distance: expected false")
	}
	if WithinDistance(a, b, math.NaN()) {
		t.Error("NaN distance: expected false")
	}
}

// TestBboxMinDistance covers the private helper directly for
// clarity — the geometry-level tests exercise it too, but with
// axis-aligned bboxes we can hand-verify.
func TestBboxMinDistance(t *testing.T) {
	cases := []struct {
		name string
		a, b Bounds
		want float64
	}{
		{"disjoint on X", Bounds{0, 0, 10, 10}, Bounds{20, 0, 30, 10}, 10},
		{"disjoint on Y", Bounds{0, 0, 10, 10}, Bounds{0, 20, 10, 30}, 10},
		{"disjoint diagonal", Bounds{0, 0, 10, 10}, Bounds{13, 14, 20, 20}, 5},
		{"overlapping", Bounds{0, 0, 10, 10}, Bounds{5, 5, 15, 15}, 0},
		{"touching edge", Bounds{0, 0, 10, 10}, Bounds{10, 5, 20, 15}, 0},
	}
	for _, c := range cases {
		if got := bboxMinDistance(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

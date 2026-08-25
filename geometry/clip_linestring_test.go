package geometry

import (
	"math"
	"testing"
)

// lsSquare returns the CCW single-ring square [0,10] x [0,10]. Distinct
// name from the (x, y, size)-parameterized unitSquare helper in clip_test.go.
func lsSquare() Polygon {
	return SimplePolygon([]Point{
		pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0),
	}, CRS{})
}

// squareWithHole returns a [0,10]^2 square with a [3,7]^2 hole (both CCW
// outer / CW hole per the shoelace convention gobi uses).
func squareWithHole() Polygon {
	return Polygon{Rings: [][]Point{
		{pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0)},
		{pt(3, 3), pt(3, 7), pt(7, 7), pt(7, 3), pt(3, 3)},
	}}
}

// lShape returns a concave L-shape:
//
//	(0,20) - (10,20)
//	   |        |
//	   |     (10,10) - (20,10)
//	   |                  |
//	(0,0)  ----------- (20,0)
func lShape() Polygon {
	return SimplePolygon([]Point{
		pt(0, 0), pt(20, 0), pt(20, 10), pt(10, 10),
		pt(10, 20), pt(0, 20), pt(0, 0),
	}, CRS{})
}

func ls(pts ...Point) LineString {
	return NewLineString(pts, CRS{})
}

func pointsAlmostEqual(a, b Point, tol float64) bool {
	return math.Abs(a.X-b.X) <= tol && math.Abs(a.Y-b.Y) <= tol
}

func linesAlmostEqual(a, b LineString, tol float64) bool {
	if len(a.Points) != len(b.Points) {
		return false
	}
	for i := range a.Points {
		if !pointsAlmostEqual(a.Points[i], b.Points[i], tol) {
			return false
		}
	}
	return true
}

func mls(lines ...LineString) MultiLineString {
	return NewMultiLineString(lines, CRS{})
}

func TestLineStringClip_FullyInside(t *testing.T) {
	line := ls(pt(2, 2), pt(4, 4), pt(6, 6))
	got := line.Clip(lsSquare())
	if len(got) != 1 || !linesAlmostEqual(got[0], line, 1e-12) {
		t.Fatalf("expected [ls] unchanged, got %+v", got)
	}
}

func TestLineStringClip_FullyOutside(t *testing.T) {
	line := ls(pt(20, 20), pt(30, 30))
	if got := line.Clip(lsSquare()); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestLineStringClip_CrossesTwice(t *testing.T) {
	// Horizontal line at y=5, from x=-5 to x=15, then to x=25, then to x=5,
	// crossing the square four times: in, out, in, out.
	line := ls(pt(-5, 5), pt(15, 5), pt(25, 5), pt(5, 5))
	inside, outside := line.SplitBy(lsSquare())
	if len(inside) != 2 {
		t.Fatalf("expected 2 inside fragments, got %d: %+v", len(inside), inside)
	}
	if len(outside) != 2 {
		t.Fatalf("expected 2 outside fragments, got %d: %+v", len(outside), outside)
	}
}

func TestLineStringClip_SingleVertexTouch(t *testing.T) {
	// Line runs outside, touches the square's (10, 5) edge midpoint via a
	// single vertex, and returns outside.
	line := ls(pt(15, 0), pt(10, 5), pt(15, 10))
	if got := line.Clip(lsSquare()); got != nil {
		t.Fatalf("expected nil for single-vertex touch, got %+v", got)
	}
}

func TestLineStringClip_CoincidentWithEdge(t *testing.T) {
	// Line from x=-5 to x=15 along y=0 (bottom edge of square).
	// Expected: one inside fragment from (0,0) to (10,0).
	line := ls(pt(-5, 0), pt(15, 0))
	got := line.Clip(lsSquare())
	if len(got) != 1 {
		t.Fatalf("expected 1 fragment, got %d: %+v", len(got), got)
	}
	want := ls(pt(0, 0), pt(10, 0))
	if !linesAlmostEqual(got[0], want, 1e-12) {
		t.Fatalf("expected %+v, got %+v", want, got[0])
	}
}

func TestLineStringClip_PolygonWithHole(t *testing.T) {
	// Horizontal line at y=5 from x=0 to x=10 crosses the hole [3,7]x[3,7].
	// Expected: two fragments — (0,5)->(3,5) and (7,5)->(10,5).
	line := ls(pt(0, 5), pt(10, 5))
	got := line.Clip(squareWithHole())
	if len(got) != 2 {
		t.Fatalf("expected 2 fragments (holes carve one out), got %d: %+v", len(got), got)
	}
}

func TestLineStringClip_RepeatedVertices(t *testing.T) {
	// Adjacent duplicate vertices must not crash and must not affect output.
	line := ls(pt(2, 2), pt(2, 2), pt(8, 8), pt(8, 8))
	got := line.Clip(lsSquare())
	if len(got) != 1 {
		t.Fatalf("expected 1 fragment, got %d: %+v", len(got), got)
	}
}

func TestLineStringClip_ConcavePolygon(t *testing.T) {
	// Horizontal line at y=15 from x=-5 to x=25. The L-shape at y=15
	// occupies only x in [0, 10]. Expected: one inside fragment covering
	// x=0..10.
	line := ls(pt(-5, 15), pt(25, 15))
	inside, outside := line.SplitBy(lShape())
	if len(inside) != 1 {
		t.Fatalf("expected 1 inside fragment, got %d: %+v", len(inside), inside)
	}
	if len(outside) != 2 {
		t.Fatalf("expected 2 outside fragments, got %d: %+v", len(outside), outside)
	}
}

func TestLineStringSplitBy_RoundTrip(t *testing.T) {
	// Sum of planar segment lengths across inside + outside fragments must
	// equal the sum across the original linestring within tol. Compared in
	// planar XY (not Length(UnitMeters), which is geodesic for unset CRS
	// and non-additive across polyline fragments).
	line := ls(pt(-5, 5), pt(15, 5), pt(15, -5), pt(-5, -5), pt(-5, 5))
	inside, outside := line.SplitBy(lsSquare())
	total := 0.0
	for _, f := range inside {
		total += planarLen(f)
	}
	for _, f := range outside {
		total += planarLen(f)
	}
	orig := planarLen(line)
	if math.Abs(total-orig) > 1e-9 {
		t.Fatalf("fragments planar length %.12f != original %.12f", total, orig)
	}
}

func planarLen(l LineString) float64 {
	var s float64
	for i := 0; i+1 < len(l.Points); i++ {
		dx := l.Points[i+1].X - l.Points[i].X
		dy := l.Points[i+1].Y - l.Points[i].Y
		s += math.Hypot(dx, dy)
	}
	return s
}

func TestMultiLineStringClip_MixedComponents(t *testing.T) {
	// Two components: one fully inside the square, one fully outside.
	m := mls(
		ls(pt(2, 2), pt(8, 8)),
		ls(pt(20, 20), pt(30, 30)),
	)
	inside, outside := m.SplitBy(lsSquare())
	if len(inside) != 1 {
		t.Fatalf("expected 1 inside fragment, got %d: %+v", len(inside), inside)
	}
	if len(outside) != 1 {
		t.Fatalf("expected 1 outside fragment, got %d: %+v", len(outside), outside)
	}
}

func TestMultiLineStringClip_Ordering(t *testing.T) {
	// Component order must be preserved: first component's inside fragment
	// should list before second component's inside fragment.
	first := ls(pt(1, 5), pt(9, 5))    // fully inside
	second := ls(pt(-5, 5), pt(15, 5)) // enters and exits
	got := mls(first, second).Clip(lsSquare())
	if len(got) != 2 {
		t.Fatalf("expected 2 inside fragments, got %d: %+v", len(got), got)
	}
	// First fragment should start at (1,5) — the first component's entry.
	if !pointsAlmostEqual(got[0].Points[0], pt(1, 5), 1e-12) {
		t.Fatalf("expected first fragment to start at (1,5), got %+v", got[0].Points[0])
	}
}

func TestMultiLineStringClip_EmptyContainer(t *testing.T) {
	if got := mls().Clip(lsSquare()); got != nil {
		t.Fatalf("expected nil for empty MultiLineString, got %+v", got)
	}
}

func TestMultiLineStringClip_BoundsRejectFastPath(t *testing.T) {
	// MultiLineString bounds disjoint from polygon — SplitBy should return
	// every component in outside without running per-component work.
	m := mls(
		ls(pt(100, 100), pt(200, 200)),
		ls(pt(300, 300), pt(400, 400)),
	)
	inside, outside := m.SplitBy(lsSquare())
	if inside != nil {
		t.Fatalf("expected nil inside, got %+v", inside)
	}
	if len(outside) != 2 {
		t.Fatalf("expected 2 outside fragments (whole components), got %d", len(outside))
	}
}

func TestLineStringClip_PreservesZ_ConvexPath(t *testing.T) {
	// 3D linestring crossing the square from (-5, 5, 0) to (15, 5, 20).
	// Entry at (0, 5) is at t=0.25 -> Z=5; exit at (10, 5) is at t=0.75 -> Z=15.
	line := NewLineStringZ([]Point{
		{X: -5, Y: 5, Z: 0, HasZ: true},
		{X: 15, Y: 5, Z: 20, HasZ: true},
	}, CRS{})
	got := line.Clip(lsSquare())
	if len(got) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(got))
	}
	if !got[0].HasZ {
		t.Fatalf("fragment lost HasZ flag")
	}
	if math.Abs(got[0].Points[0].Z-5) > 1e-9 {
		t.Fatalf("entry Z = %v, want 5", got[0].Points[0].Z)
	}
	if math.Abs(got[0].Points[1].Z-15) > 1e-9 {
		t.Fatalf("exit Z = %v, want 15", got[0].Points[1].Z)
	}
}

func TestLineStringClip_PreservesZ_GeneralPath(t *testing.T) {
	// Same test against the concave L-shape to exercise clipLineGeneral.
	// Linestring (-5, 15, 0) -> (25, 15, 30) crosses the L's vertical arm
	// (interior at y=15 spans x in [0, 10]). Entry at x=0 is t=1/6 -> Z=5;
	// exit at x=10 is t=1/2 -> Z=15.
	line := NewLineStringZ([]Point{
		{X: -5, Y: 15, Z: 0, HasZ: true},
		{X: 25, Y: 15, Z: 30, HasZ: true},
	}, CRS{})
	got := line.Clip(lShape())
	if len(got) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(got))
	}
	if !got[0].HasZ {
		t.Fatalf("fragment lost HasZ flag")
	}
	if math.Abs(got[0].Points[0].Z-5) > 1e-9 {
		t.Fatalf("entry Z = %v, want 5", got[0].Points[0].Z)
	}
	if math.Abs(got[0].Points[1].Z-15) > 1e-9 {
		t.Fatalf("exit Z = %v, want 15", got[0].Points[1].Z)
	}
}

func TestLineStringClip_ConcaveCoincidentWithEdge_KnownLimitation(t *testing.T) {
	t.Skip("known limitation: clipLineGeneral classifies sub-intervals via Polygon.Contains(midpoint), which is undefined on the boundary. Follow-up: replace with a vertex-marching Weiler-Atherton pass that treats coincident-with-edge sub-segments as inside (matching the convex path).")
	// Target once fixed: a linestring lying exactly along the L-shape's
	// bottom edge should yield one inside fragment covering [0, 20] on y=0.
	line := ls(pt(-5, 0), pt(25, 0))
	got := line.Clip(lShape())
	if len(got) != 1 {
		t.Fatalf("expected 1 fragment along bottom edge, got %d", len(got))
	}
}

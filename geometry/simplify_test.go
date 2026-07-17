package geometry

import (
	"math"
	"testing"
)

func TestSimplify_LineString_CollinearPointsRemoved(t *testing.T) {
	// Five collinear points along y=0. Any tolerance > 0 should collapse
	// them to the endpoints.
	l := LineString{Points: []Point{pt(0, 0), pt(1, 0), pt(2, 0), pt(3, 0), pt(4, 0)}}
	simp := l.Simplify(0.001)
	if len(simp.Points) != 2 {
		t.Fatalf("simplified points = %d, want 2 (endpoints only)", len(simp.Points))
	}
	if simp.Points[0].X != 0 || simp.Points[1].X != 4 {
		t.Fatalf("endpoints not preserved: %+v", simp.Points)
	}
}

func TestSimplify_LineString_PreservesShape(t *testing.T) {
	// A zig-zag with a large tolerance should keep only the peaks.
	l := LineString{Points: []Point{
		pt(0, 0), pt(1, 0.05), pt(2, 0), pt(3, 5), pt(4, 0), pt(5, -0.05), pt(6, 0),
	}}
	simp := l.Simplify(1.0)
	// The point (3, 5) is 5 units off the y=0 chord — must be retained.
	found := false
	for _, p := range simp.Points {
		if p.X == 3 && p.Y == 5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("peak point missing after simplify: %+v", simp.Points)
	}
	if len(simp.Points) >= len(l.Points) {
		t.Fatalf("simplification produced no reduction: %d → %d", len(l.Points), len(simp.Points))
	}
}

func TestSimplify_LineString_TinyLineUnchanged(t *testing.T) {
	l := LineString{Points: []Point{pt(0, 0), pt(1, 1)}}
	if simp := l.Simplify(1.0); len(simp.Points) != 2 {
		t.Fatalf("two-point line should be preserved as-is, got %d", len(simp.Points))
	}
}

func TestSimplify_Polygon_RingClosureMaintained(t *testing.T) {
	poly := SimplePolygon([]Point{
		pt(0, 0), pt(1, 0.01), pt(2, 0), pt(3, 0.01),
		pt(4, 0), pt(4, 4), pt(0, 4), pt(0, 0),
	}, PseudoMercator)
	simp := poly.Simplify(0.1)
	if len(simp.Rings) == 0 {
		t.Fatalf("no rings after simplify")
	}
	r := simp.Rings[0]
	if r[0] != r[len(r)-1] {
		t.Fatalf("ring not closed after simplify: first=%+v last=%+v", r[0], r[len(r)-1])
	}
}

func TestSimplify_Polygon_TinyRingsUntouched(t *testing.T) {
	// A triangle (4 points including close) shouldn't lose vertices at any
	// tolerance — otherwise it'd collapse to a degenerate polygon.
	poly := SimplePolygon([]Point{
		pt(0, 0), pt(10, 0), pt(5, 10), pt(0, 0),
	}, PseudoMercator)
	simp := poly.Simplify(1000)
	if len(simp.Rings[0]) != 4 {
		t.Fatalf("triangle got simplified away: %+v", simp.Rings)
	}
}

func TestSimplify_DispatchUnsupportedIsNoop(t *testing.T) {
	// Point is unsupported — should pass through unchanged.
	g, err := Simplify(Point{X: 1, Y: 2}, 5.0)
	if err != nil {
		t.Fatal(err)
	}
	if p := g.(Point); p.X != 1 || p.Y != 2 {
		t.Fatalf("point mutated: %+v", p)
	}
}

func TestPerpDistance_Zero(t *testing.T) {
	// Point on the line has distance 0.
	got := perpDistance(pt(1, 0), pt(0, 0), pt(2, 0))
	if math.Abs(got) > 1e-12 {
		t.Fatalf("perpDistance on-line = %v, want 0", got)
	}
}

func TestPerpDistance_ExpectedValue(t *testing.T) {
	// (0, 3) to the x-axis line from (-1,0) to (1,0) should be exactly 3.
	got := perpDistance(pt(0, 3), pt(-1, 0), pt(1, 0))
	if math.Abs(got-3) > 1e-9 {
		t.Fatalf("perpDistance = %v, want 3", got)
	}
}

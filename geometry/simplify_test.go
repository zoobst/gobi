package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// TestSimplify_LineString_PreservesZ — regression guard for the
// review-caught bug where the SoA-backed douglasPeucker silently
// dropped Point.Z (rebuilt Point{X, Y} without Z / HasZ). 3D
// linestrings must preserve altitude on retained vertices.
func TestSimplify_LineString_PreservesZ(t *testing.T) {
	l := LineString{
		Points: []Point{
			{X: 0, Y: 0, Z: 100, HasZ: true},
			{X: 1, Y: 0, Z: 101, HasZ: true},
			{X: 2, Y: 0, Z: 102, HasZ: true},
			{X: 3, Y: 0, Z: 103, HasZ: true},
			{X: 4, Y: 0, Z: 104, HasZ: true},
		},
		HasZ: true,
	}
	simp := l.Simplify(0.001)
	if len(simp.Points) != 2 {
		t.Fatalf("collinear collapse: %d points, want 2", len(simp.Points))
	}
	// Endpoints must retain Z and HasZ.
	for i, p := range simp.Points {
		if !p.HasZ {
			t.Errorf("point %d: HasZ = false, want true", i)
		}
	}
	if simp.Points[0].Z != 100 || simp.Points[1].Z != 104 {
		t.Errorf("Z not preserved: got Z0=%v Z1=%v, want 100/104",
			simp.Points[0].Z, simp.Points[1].Z)
	}
}

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

// TestSimplifyDPFromXY_MatchesAoS — the SoA iterative DP must
// return the same retained coordinates as the AoS recursive
// douglasPeucker on every well-formed input. Tie-breaking + scan
// order match by construction (both use strict `>` argmax, both
// process left-then-right).
func TestSimplifyDPFromXY_MatchesAoS(t *testing.T) {
	cases := []struct {
		name      string
		pts       []Point
		tolerance float64
	}{
		{
			name: "straight-line-collapses",
			pts: []Point{
				{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 2, Y: 0}, {X: 3, Y: 0}, {X: 4, Y: 0},
			},
			tolerance: 0.5,
		},
		{
			name: "kink-preserved",
			pts: []Point{
				{X: 0, Y: 0}, {X: 5, Y: 5}, {X: 10, Y: 0},
			},
			tolerance: 0.5,
		},
		{
			name: "nearly-straight-collapses",
			pts: []Point{
				{X: 0, Y: 0}, {X: 5, Y: 0.001}, {X: 10, Y: 0},
			},
			tolerance: 0.01,
		},
		{
			name: "hairpin",
			pts: []Point{
				{X: 0, Y: 0}, {X: 5, Y: 10}, {X: 10, Y: 0}, {X: 5, Y: -10}, {X: 0, Y: 0},
			},
			tolerance: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			xs := make([]float64, len(c.pts))
			ys := make([]float64, len(c.pts))
			for i, p := range c.pts {
				xs[i] = p.X
				ys[i] = p.Y
			}
			gotXs, gotYs := SimplifyDPFromXY(xs, ys, c.tolerance)
			wantPts := douglasPeucker(c.pts, c.tolerance)
			if len(gotXs) != len(wantPts) {
				t.Fatalf("len got=%d, want=%d (got %v,%v, want %v)",
					len(gotXs), len(wantPts), gotXs, gotYs, wantPts)
			}
			for i := range gotXs {
				if gotXs[i] != wantPts[i].X || gotYs[i] != wantPts[i].Y {
					t.Errorf("i=%d: got (%v,%v), want (%v,%v)",
						i, gotXs[i], gotYs[i], wantPts[i].X, wantPts[i].Y)
				}
			}
		})
	}
}

// TestSimplifyDPFromXY_RandomLineStrings — fuzz against the AoS
// oracle across random polylines.
func TestSimplifyDPFromXY_RandomLineStrings(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := range 100 {
		n := 3 + rng.Intn(50)
		pts := make([]Point, n)
		xs := make([]float64, n)
		ys := make([]float64, n)
		for i := range pts {
			x := rng.Float64() * 100
			y := rng.Float64() * 100
			pts[i] = Point{X: x, Y: y}
			xs[i] = x
			ys[i] = y
		}
		tol := 0.1 + rng.Float64()*10
		gotXs, gotYs := SimplifyDPFromXY(xs, ys, tol)
		wantPts := douglasPeucker(pts, tol)
		if len(gotXs) != len(wantPts) {
			t.Fatalf("iter %d n=%d tol=%v: len got=%d want=%d",
				iter, n, tol, len(gotXs), len(wantPts))
		}
		for i := range gotXs {
			if math.Abs(gotXs[i]-wantPts[i].X) > 1e-12 ||
				math.Abs(gotYs[i]-wantPts[i].Y) > 1e-12 {
				t.Errorf("iter %d i=%d: got (%v,%v), want (%v,%v)",
					iter, i, gotXs[i], gotYs[i], wantPts[i].X, wantPts[i].Y)
			}
		}
	}
}

// TestSimplifyDPFromXY_EdgeCases — tolerance ≤ 0, n < 3, and the
// coincident-endpoint fallback all match the AoS shape.
func TestSimplifyDPFromXY_EdgeCases(t *testing.T) {
	cases := []struct {
		name      string
		xs, ys    []float64
		tolerance float64
		wantLen   int
	}{
		{"empty", []float64{}, []float64{}, 1, 0},
		{"one-point", []float64{5}, []float64{5}, 1, 1},
		{"two-points", []float64{0, 10}, []float64{0, 10}, 1, 2},
		{"zero-tolerance-keeps-all", []float64{0, 5, 10}, []float64{0, 0, 0}, 0, 3},
		{"negative-tolerance-keeps-all", []float64{0, 5, 10}, []float64{0, 0, 0}, -1, 3},
		{"coincident-endpoints-within-tol",
			[]float64{0, 0.5, 0}, []float64{0, 0.5, 0}, 10, 2}, // interior collapses
		{"coincident-endpoints-outside-tol",
			[]float64{0, 5, 0}, []float64{0, 5, 0}, 1, 3}, // interior kept
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotXs, gotYs := SimplifyDPFromXY(c.xs, c.ys, c.tolerance)
			if len(gotXs) != c.wantLen || len(gotYs) != c.wantLen {
				t.Errorf("got len (%d,%d), want %d", len(gotXs), len(gotYs), c.wantLen)
			}
		})
	}
}

// TestPointsView_SimplifyDP_XYZ — XYZ input retains Z at every
// kept index.
func TestPointsView_SimplifyDP_XYZ(t *testing.T) {
	// Straight-line XY with varying Z. DP should collapse to
	// endpoints; Z values at endpoints should survive.
	v := PointsView{
		Xs:   []float64{0, 1, 2, 3, 4},
		Ys:   []float64{0, 0, 0, 0, 0},
		Zs:   []float64{100, 101, 102, 103, 104},
		HasZ: true,
	}
	out := v.SimplifyDP(0.5)
	if out.Len() != 2 {
		t.Fatalf("len = %d, want 2", out.Len())
	}
	if out.Xs[0] != 0 || out.Xs[1] != 4 {
		t.Errorf("Xs = %v, want [0 4]", out.Xs)
	}
	if out.Zs[0] != 100 || out.Zs[1] != 104 {
		t.Errorf("Zs = %v, want [100 104]", out.Zs)
	}
	if !out.HasZ {
		t.Error("HasZ = false, want true")
	}
}

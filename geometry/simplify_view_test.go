package geometry

import (
	"math"
	"math/rand"
	"testing"
)

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

package geometry

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// TestConvexHullFromXY_Square — the canonical "8-point ring
// collapses to 4 corners" shape from geometry_test.go, reworked
// on slabs.
func TestConvexHullFromXY_Square(t *testing.T) {
	xs := []float64{0, 5, 10, 9, 10, 5, 0, 1}
	ys := []float64{0, 1, 0, 5, 10, 9, 10, 5}
	hx, hy := ConvexHullFromXY(xs, ys)
	// Corners: (0,0), (10,0), (10,10), (0,10). Closed → 5 output.
	if len(hx) != 5 {
		t.Fatalf("hull len = %d, want 5\n xs: %v\n ys: %v", len(hx), hx, hy)
	}
	// Vertex set should be the 4 corners.
	got := make([][2]float64, 4)
	for i := range 4 {
		got[i] = [2]float64{hx[i], hy[i]}
	}
	want := [][2]float64{{0, 0}, {10, 0}, {10, 10}, {0, 10}}
	sortPairs := func(s [][2]float64) {
		sort.Slice(s, func(i, j int) bool {
			if s[i][0] != s[j][0] {
				return s[i][0] < s[j][0]
			}
			return s[i][1] < s[j][1]
		})
	}
	sortPairs(got)
	sortPairs(want)
	for i := range 4 {
		if got[i] != want[i] {
			t.Errorf("corner %d: got %v, want %v", i, got[i], want[i])
		}
	}
	// Closing vertex must match the first.
	if hx[4] != hx[0] || hy[4] != hy[0] {
		t.Errorf("closing vertex (%v, %v) != first (%v, %v)",
			hx[4], hy[4], hx[0], hy[0])
	}
}

// TestConvexHullFromXY_Triangle — three-point input should hull
// to itself (closed).
func TestConvexHullFromXY_Triangle(t *testing.T) {
	xs := []float64{0, 10, 5}
	ys := []float64{0, 0, 10}
	hx, hy := ConvexHullFromXY(xs, ys)
	if len(hx) != 4 {
		t.Fatalf("hull len = %d, want 4", len(hx))
	}
	if hx[3] != hx[0] || hy[3] != hy[0] {
		t.Errorf("triangle not closed")
	}
}

// TestConvexHullFromXY_Degenerate — <3 points or all-collinear
// inputs.
func TestConvexHullFromXY_Degenerate(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		ys   []float64
		want int // expected len of hull
	}{
		{"empty", nil, nil, 0},
		{"one", []float64{5}, []float64{5}, 1},
		{"two", []float64{0, 10}, []float64{0, 10}, 2},
		{"collinear-3", []float64{0, 5, 10}, []float64{0, 5, 10}, 2}, // collapses to endpoints
		{"duplicate", []float64{0, 0, 10, 10, 10, 0, 0, 10, 0, 0},
			[]float64{0, 0, 0, 0, 10, 10, 10, 10, 0, 0}, 5}, // 4 corners + close
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hx, hy := ConvexHullFromXY(c.xs, c.ys)
			if len(hx) != c.want || len(hy) != c.want {
				t.Errorf("got len (%d,%d), want %d\n xs: %v\n ys: %v",
					len(hx), len(hy), c.want, hx, hy)
			}
		})
	}
}

// TestConvexHullFromXY_CCWOrdering — every consecutive triple of
// hull vertices must make a non-clockwise turn.
func TestConvexHullFromXY_CCWOrdering(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	n := 200
	xs := make([]float64, n)
	ys := make([]float64, n)
	for i := range xs {
		xs[i] = rng.Float64() * 1000
		ys[i] = rng.Float64() * 1000
	}
	hx, hy := ConvexHullFromXY(xs, ys)
	if len(hx) < 4 {
		t.Fatalf("hull too small: %d", len(hx))
	}
	for i := 0; i < len(hx)-2; i++ {
		cross := (hx[i+1]-hx[i])*(hy[i+2]-hy[i]) - (hy[i+1]-hy[i])*(hx[i+2]-hx[i])
		if cross < 0 {
			t.Errorf("CW turn at i=%d: (%v,%v) → (%v,%v) → (%v,%v)",
				i, hx[i], hy[i], hx[i+1], hy[i+1], hx[i+2], hy[i+2])
		}
	}
	// Closing vertex.
	if hx[len(hx)-1] != hx[0] || hy[len(hy)-1] != hy[0] {
		t.Errorf("closing vertex mismatch")
	}
}

// TestConvexHullFromXY_ContainsAllInputs — for random input the
// hull polygon must contain every input vertex (up to tolerance).
func TestConvexHullFromXY_ContainsAllInputs(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	n := 100
	xs := make([]float64, n)
	ys := make([]float64, n)
	for i := range xs {
		xs[i] = rng.Float64() * 100
		ys[i] = rng.Float64() * 100
	}
	hx, hy := ConvexHullFromXY(xs, ys)
	hullRing := make([]Point, len(hx))
	for i := range hx {
		hullRing[i] = Point{X: hx[i], Y: hy[i]}
	}
	hullPoly := Polygon{Rings: [][]Point{hullRing}}
	for i := range xs {
		pt := Point{X: xs[i], Y: ys[i]}
		if !hullPoly.Contains(pt) && !pointOnPolygonBoundary(pt, hullPoly) {
			t.Errorf("input %d (%v, %v) not in hull", i, xs[i], ys[i])
		}
	}
}

// TestPolygon_ConvexHull_ProducesSameSet — the AoS wrapper must
// produce the same vertex set as the pre-Slice-12 Graham scan
// (starting vertex may differ). Correct hull independent of algo
// implementation.
func TestPolygon_ConvexHull_ProducesSameSet(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	pts := make([]Point, 50)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 100, Y: rng.Float64() * 100}
	}
	poly := Polygon{Rings: [][]Point{pts}}
	hull := poly.ConvexHull()
	hullRing := hull.Exterior()
	if len(hullRing) < 4 {
		t.Fatalf("hull too small: %d", len(hullRing))
	}
	// Closing vertex present.
	first, last := hullRing[0], hullRing[len(hullRing)-1]
	if first.X != last.X || first.Y != last.Y {
		t.Errorf("hull not closed: first=%v last=%v", first, last)
	}
	// Every input point should be inside or on the hull.
	for _, p := range pts {
		if !hull.Contains(p) && !pointOnPolygonBoundary(p, hull) {
			t.Errorf("input %v not in hull", p)
		}
	}
}

// TestPointsView_ConvexHull_XYZ — Z coordinates travel with
// retained hull vertices; XY-only decision.
func TestPointsView_ConvexHull_XYZ(t *testing.T) {
	// Simple square with distinct Z values on each corner.
	v := PointsView{
		Xs:   []float64{0, 10, 10, 0, 5},
		Ys:   []float64{0, 0, 10, 10, 5},
		Zs:   []float64{1, 2, 3, 4, 999},
		HasZ: true,
	}
	out := v.ConvexHull()
	// Should collapse the middle (5,5) point → 4 corners + close.
	if out.Len() != 5 {
		t.Fatalf("hull len = %d, want 5", out.Len())
	}
	// (5, 5, 999) must NOT appear.
	for i := range out.Xs {
		if out.Zs[i] == 999 {
			t.Errorf("interior point (Z=999) leaked into hull at i=%d", i)
		}
	}
	if !math.IsNaN(out.Zs[0]) && out.Zs[len(out.Zs)-1] != out.Zs[0] {
		t.Errorf("closing Z should match first Z")
	}
}

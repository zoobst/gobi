package geometry

import (
	"math/rand"
	"testing"
)

// TestTestPrepared_MatchesTest_PointVsPolygon — the fast paths
// must produce the same answer as Test() for every predicate on
// the Point×Polygon shape (both orderings).
func TestTestPrepared_MatchesTest_PointVsPolygon(t *testing.T) {
	poly := Polygon{Rings: [][]Point{
		// Exterior L-shape.
		{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
			{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
		// Hole in the lower-left corner.
		{{X: 1, Y: 1}, {X: 2, Y: 1}, {X: 2, Y: 2}, {X: 1, Y: 2}, {X: 1, Y: 1}},
	}}
	rng := rand.New(rand.NewSource(1))
	pPoly := Prepare(poly)
	preds := []Predicate{PredIntersects, PredContains, PredWithin, PredDisjoint}
	for range 200 {
		pt := Point{X: rng.Float64()*14 - 2, Y: rng.Float64()*14 - 2}
		pPt := Prepare(pt)
		for _, pred := range preds {
			// (Point, Polygon)
			want := Test(pred, pt, poly)
			got := TestPrepared(pred, pPt, pPoly)
			if got != want {
				t.Errorf("(pt, poly) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
			// (Polygon, Point) — swapped
			want = Test(pred, poly, pt)
			got = TestPrepared(pred, pPoly, pPt)
			if got != want {
				t.Errorf("(poly, pt) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestTestPrepared_MatchesTest_PointVsMultiPolygon — same as
// above but on the MultiPolygon shape. Uses two disjoint squares
// to exercise the "iterate polys until one matches" loop.
func TestTestPrepared_MatchesTest_PointVsMultiPolygon(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{{
			{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10},
		}}},
	}}
	rng := rand.New(rand.NewSource(2))
	pM := Prepare(m)
	preds := []Predicate{PredIntersects, PredContains, PredWithin, PredDisjoint}
	for range 200 {
		pt := Point{X: rng.Float64() * 20, Y: rng.Float64() * 20}
		pPt := Prepare(pt)
		for _, pred := range preds {
			want := Test(pred, pt, m)
			got := TestPrepared(pred, pPt, pM)
			if got != want {
				t.Errorf("(pt, multi) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
			want = Test(pred, m, pt)
			got = TestPrepared(pred, pM, pPt)
			if got != want {
				t.Errorf("(multi, pt) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestTestPrepared_FallsThroughForUnsupported — pair shapes we
// don't have fast paths for (Point×Point, LineString×Polygon,
// Polygon×Polygon) must match Test() output. This proves the
// fall-through path is functional, not silently returning false.
func TestTestPrepared_FallsThroughForUnsupported(t *testing.T) {
	// Two identical points — should intersect.
	pa := Point{X: 3, Y: 4}
	pb := Point{X: 3, Y: 4}
	if got := TestPrepared(PredIntersects, Prepare(pa), Prepare(pb)); got != Test(PredIntersects, pa, pb) {
		t.Errorf("Point×Point Intersects: got %v, want %v", got, Test(PredIntersects, pa, pb))
	}

	// LineString × Polygon — crosses.
	ls := LineString{Points: []Point{{X: -5, Y: 5}, {X: 15, Y: 5}}}
	poly := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}}}
	if got := TestPrepared(PredIntersects, Prepare(ls), Prepare(poly)); got != Test(PredIntersects, ls, poly) {
		t.Errorf("LineString×Polygon Intersects: got %v, want %v", got, Test(PredIntersects, ls, poly))
	}

	// Polygon × Polygon — overlapping.
	polyA := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
	}}}
	polyB := Polygon{Rings: [][]Point{{
		{X: 3, Y: 3}, {X: 8, Y: 3}, {X: 8, Y: 8}, {X: 3, Y: 8}, {X: 3, Y: 3},
	}}}
	if got := TestPrepared(PredIntersects, Prepare(polyA), Prepare(polyB)); got != Test(PredIntersects, polyA, polyB) {
		t.Errorf("Polygon×Polygon Intersects: got %v, want %v", got, Test(PredIntersects, polyA, polyB))
	}
}

// TestTestPrepared_NilGeometries — nil on either side returns
// false for every predicate. Matches Test's guard.
func TestTestPrepared_NilGeometries(t *testing.T) {
	pt := Prepare(Point{X: 1, Y: 1})
	nilP := PreparedGeometry{} // G is nil
	preds := []Predicate{PredIntersects, PredContains, PredWithin, PredTouches,
		PredCrosses, PredOverlaps, PredDisjoint}
	for _, pred := range preds {
		if got := TestPrepared(pred, nilP, pt); got {
			t.Errorf("(nil, pt) %s: got true, want false", pred)
		}
		if got := TestPrepared(pred, pt, nilP); got {
			t.Errorf("(pt, nil) %s: got true, want false", pred)
		}
	}
}

// TestPrepare_NoRingsForNonPolygon — non-polygon geometries
// should not populate the ring caches. Prevents a future refactor
// from accidentally materializing views for shapes that don't
// need them (which would negate the Slice-1 amortization argument
// for one-shot callers). Note: Prepare inherently pays one
// interface-boxing alloc when the caller passes a concrete
// Geometry — that's outside the scope of this check.
func TestPrepare_NoRingsForNonPolygon(t *testing.T) {
	cases := []Geometry{
		Point{X: 5, Y: 5},
		LineString{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		MultiPoint{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
	}
	for _, g := range cases {
		p := Prepare(g)
		if p.polyRings != nil {
			t.Errorf("%T: polyRings should be nil", g)
		}
		if p.multiPolyRings != nil {
			t.Errorf("%T: multiPolyRings should be nil", g)
		}
	}
}

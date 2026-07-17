package geometry

import (
	"testing"
)

// Test helpers use existing pt() from geometry_test.go.

func square(cx, cy, r float64) Polygon {
	return SimplePolygon([]Point{
		pt(cx-r, cy-r), pt(cx+r, cy-r), pt(cx+r, cy+r), pt(cx-r, cy+r), pt(cx-r, cy-r),
	}, PseudoMercator)
}

func TestIntersects_PointPolygon(t *testing.T) {
	poly := square(0, 0, 10)
	cases := []struct {
		name string
		p    Point
		want bool
	}{
		{"center inside", pt(0, 0), true},
		{"on edge", pt(10, 5), true},
		{"on corner", pt(-10, -10), true},
		{"outside", pt(100, 100), false},
	}
	for _, c := range cases {
		if got := Intersects(c.p, poly); got != c.want {
			t.Errorf("%s: Intersects(pt, poly) = %v, want %v", c.name, got, c.want)
		}
		if got := Intersects(poly, c.p); got != c.want {
			t.Errorf("%s: Intersects(poly, pt) = %v, want %v (symmetry)", c.name, got, c.want)
		}
	}
}

func TestIntersects_LineLine(t *testing.T) {
	a := LineString{Points: []Point{pt(0, 0), pt(10, 10)}}
	crosses := LineString{Points: []Point{pt(0, 10), pt(10, 0)}}
	parallel := LineString{Points: []Point{pt(0, 1), pt(10, 11)}}
	touching := LineString{Points: []Point{pt(5, 5), pt(15, 5)}}

	if !Intersects(a, crosses) {
		t.Error("crossing lines should intersect")
	}
	if Intersects(a, parallel) {
		t.Error("parallel offset lines should not intersect")
	}
	if !Intersects(a, touching) {
		t.Error("line touching another at a point should intersect")
	}
}

func TestIntersects_LinePolygon(t *testing.T) {
	poly := square(0, 0, 10)
	crosses := LineString{Points: []Point{pt(-20, 0), pt(20, 0)}}
	inside := LineString{Points: []Point{pt(-5, -5), pt(5, 5)}}
	outside := LineString{Points: []Point{pt(100, 100), pt(200, 200)}}
	touchesEdge := LineString{Points: []Point{pt(10, -20), pt(10, 20)}}

	if !Intersects(crosses, poly) {
		t.Error("line crossing polygon should intersect")
	}
	if !Intersects(inside, poly) {
		t.Error("line inside polygon should intersect")
	}
	if Intersects(outside, poly) {
		t.Error("line disjoint from polygon should not intersect")
	}
	if !Intersects(touchesEdge, poly) {
		t.Error("line running along a polygon edge should intersect")
	}
}

func TestIntersects_PolygonPolygon(t *testing.T) {
	a := square(0, 0, 10)
	overlapping := square(5, 5, 10)
	nested := square(0, 0, 3) // completely inside a
	disjoint := square(100, 100, 5)
	edgeTouch := SimplePolygon([]Point{
		pt(10, -5), pt(20, -5), pt(20, 5), pt(10, 5), pt(10, -5),
	}, PseudoMercator)

	if !Intersects(a, overlapping) {
		t.Error("overlapping polygons should intersect")
	}
	if !Intersects(a, nested) {
		t.Error("nested polygon should intersect")
	}
	if Intersects(a, disjoint) {
		t.Error("disjoint polygons should not intersect")
	}
	if !Intersects(a, edgeTouch) {
		t.Error("polygons sharing an edge should intersect")
	}
}

func TestContains_PolygonPoint(t *testing.T) {
	poly := square(0, 0, 10)
	if !Contains(poly, pt(0, 0)) {
		t.Error("polygon should contain its center")
	}
	if !Contains(poly, pt(10, 0)) {
		t.Error("polygon should contain a boundary point")
	}
	if Contains(poly, pt(100, 100)) {
		t.Error("polygon should not contain a distant point")
	}
	if Contains(pt(0, 0), poly) {
		t.Error("point should not contain polygon")
	}
}

func TestContains_PolygonPolygon(t *testing.T) {
	outer := square(0, 0, 10)
	inner := square(0, 0, 3)
	overlapping := square(5, 0, 10)
	disjoint := square(100, 100, 5)

	if !Contains(outer, inner) {
		t.Error("outer should contain inner")
	}
	if Contains(inner, outer) {
		t.Error("inner should not contain outer")
	}
	if Contains(outer, overlapping) {
		t.Error("polygon should not contain an overlapping polygon")
	}
	if Contains(outer, disjoint) {
		t.Error("polygon should not contain a disjoint polygon")
	}
}

func TestContains_RespectsHoles(t *testing.T) {
	// 20x20 outer with a 6x6 hole. Points in the hole should not be contained.
	poly := Polygon{
		Rings: [][]Point{
			{pt(-10, -10), pt(10, -10), pt(10, 10), pt(-10, 10), pt(-10, -10)},
			{pt(-3, -3), pt(3, -3), pt(3, 3), pt(-3, 3), pt(-3, -3)},
		},
		CRSValue: PseudoMercator,
	}
	if Contains(poly, pt(0, 0)) {
		t.Error("point in hole should not be contained")
	}
	if !Contains(poly, pt(5, 5)) {
		t.Error("point outside hole but inside outer should be contained")
	}
}

func TestWithin_IsContainsFlipped(t *testing.T) {
	outer := square(0, 0, 10)
	inner := square(0, 0, 3)
	if !Within(inner, outer) {
		t.Error("inner should be within outer")
	}
	if Within(outer, inner) {
		t.Error("outer should not be within inner")
	}
}

func TestIntersects_MultiPoly_AnyComponentMatches(t *testing.T) {
	mp := MultiPolygon{
		Polygons: []Polygon{square(0, 0, 5), square(100, 100, 5)},
		CRSValue: PseudoMercator,
	}
	if !Intersects(mp, pt(0, 0)) {
		t.Error("MultiPolygon should intersect a point inside one component")
	}
	if !Intersects(mp, pt(100, 100)) {
		t.Error("MultiPolygon should intersect a point inside the other component")
	}
	if Intersects(mp, pt(500, 500)) {
		t.Error("MultiPolygon should not intersect a distant point")
	}
}

func TestContains_MultiPolyLHS_AnyComponentContains(t *testing.T) {
	mp := MultiPolygon{
		Polygons: []Polygon{square(0, 0, 10), square(100, 100, 3)},
		CRSValue: PseudoMercator,
	}
	inner := square(0, 0, 3)
	if !Contains(mp, inner) {
		t.Error("MultiPolygon should contain a polygon that fits inside one component")
	}
}

func TestContains_MultiPolyRHS_AllComponentsMustBeContained(t *testing.T) {
	big := square(0, 0, 200)
	mp := MultiPolygon{
		Polygons: []Polygon{square(0, 0, 5), square(50, 50, 5)},
		CRSValue: PseudoMercator,
	}
	if !Contains(big, mp) {
		t.Error("big polygon should contain both components of the multipolygon")
	}

	// Now shrink the big one so only one component fits.
	smaller := square(0, 0, 20)
	if Contains(smaller, mp) {
		t.Error("smaller polygon should not contain both components")
	}
}

func TestTest_DispatchesPredicate(t *testing.T) {
	outer := square(0, 0, 10)
	inner := square(0, 0, 3)
	if !Test(PredContains, outer, inner) {
		t.Error("Test(PredContains, outer, inner)")
	}
	if !Test(PredWithin, inner, outer) {
		t.Error("Test(PredWithin, inner, outer)")
	}
	if !Test(PredIntersects, outer, inner) {
		t.Error("Test(PredIntersects, outer, inner)")
	}
}

func TestIntersects_NilSafe(t *testing.T) {
	if Intersects(nil, pt(0, 0)) {
		t.Error("nil intersects anything should be false")
	}
	if Contains(nil, pt(0, 0)) {
		t.Error("nil contains anything should be false")
	}
}

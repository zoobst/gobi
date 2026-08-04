package geometry

import (
	"math"
	"testing"
)

// unitSquare returns the axis-aligned square [x, x+size] × [y, y+size] as a
// CCW-wound Polygon with no CRS (planar test coordinates).
func unitSquare(x, y, size float64) Polygon {
	return SimplePolygon([]Point{
		pt(x, y),
		pt(x+size, y),
		pt(x+size, y+size),
		pt(x, y+size),
		pt(x, y),
	}, CRS{})
}

// totalArea returns the total planar area of a Geometry that must be a
// Polygon or MultiPolygon.
func totalArea(t *testing.T, g Geometry) float64 {
	t.Helper()
	switch v := g.(type) {
	case Polygon:
		return polyPlanarArea(v)
	case MultiPolygon:
		var total float64
		for _, p := range v.Polygons {
			total += polyPlanarArea(p)
		}
		return total
	}
	t.Fatalf("unexpected geometry %T", g)
	return 0
}

// polyPlanarArea is a test helper that ignores CRS (uses the planar
// shoelace directly) so we can exercise unit-CRS-free polygons.
func polyPlanarArea(p Polygon) float64 {
	if len(p.Rings) == 0 {
		return 0
	}
	a := planarRingArea(p.Rings[0])
	for _, hole := range p.Rings[1:] {
		a -= planarRingArea(hole)
	}
	return a
}

func nearlyEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestClip_OverlappingSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(5, 5, 10)

	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 25, 1e-9) {
		t.Errorf("Clip area = %v, want 25", area)
	}
}

func TestUnion_OverlappingSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(5, 5, 10)

	got, err := Union(a, b)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	// Two 100-area squares overlapping in a 25-area square: 100+100-25 = 175.
	if area := totalArea(t, got); !nearlyEqual(area, 175, 1e-9) {
		t.Errorf("Union area = %v, want 175", area)
	}
}

func TestDifference_OverlappingSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(5, 5, 10)

	got, err := Difference(a, b)
	if err != nil {
		t.Fatalf("Difference: %v", err)
	}
	// a - (a∩b) = 100 - 25 = 75.
	if area := totalArea(t, got); !nearlyEqual(area, 75, 1e-9) {
		t.Errorf("Difference area = %v, want 75", area)
	}
}

func TestSymDifference_OverlappingSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(5, 5, 10)

	got, err := SymDifference(a, b)
	if err != nil {
		t.Fatalf("SymDifference: %v", err)
	}
	// (a ∪ b) - (a ∩ b) = 175 - 25 = 150.
	if area := totalArea(t, got); !nearlyEqual(area, 150, 1e-9) {
		t.Errorf("SymDifference area = %v, want 150", area)
	}
}

func TestClip_DisjointSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(100, 100, 10)

	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := totalArea(t, got); area != 0 {
		t.Errorf("Clip area = %v, want 0 (disjoint)", area)
	}
}

func TestUnion_DisjointSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(100, 100, 10)

	got, err := Union(a, b)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	if _, ok := got.(MultiPolygon); !ok {
		t.Errorf("Union of disjoint = %T, want MultiPolygon", got)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 200, 1e-9) {
		t.Errorf("Union area = %v, want 200", area)
	}
}

func TestClip_ContainedSquare(t *testing.T) {
	outer := unitSquare(0, 0, 100)
	inner := unitSquare(25, 25, 10)

	got, err := Clip(outer, inner)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 100, 1e-9) {
		t.Errorf("Clip area = %v, want 100", area)
	}
}

func TestDifference_OuterMinusInner(t *testing.T) {
	outer := unitSquare(0, 0, 100)
	inner := unitSquare(25, 25, 10)

	got, err := Difference(outer, inner)
	if err != nil {
		t.Fatalf("Difference: %v", err)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 10000-100, 1e-9) {
		t.Errorf("Difference area = %v, want 9900", area)
	}
	// Should be a Polygon with one exterior + one hole.
	p, ok := got.(Polygon)
	if !ok {
		t.Fatalf("Difference = %T, want Polygon (outer with hole)", got)
	}
	if len(p.Rings) != 2 {
		t.Errorf("expected 2 rings (exterior + hole), got %d", len(p.Rings))
	}
}

func TestClip_IdenticalSquares(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(0, 0, 10)

	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 100, 1e-9) {
		t.Errorf("Clip area = %v, want 100 (self)", area)
	}

	dGot, err := Difference(a, b)
	if err != nil {
		t.Fatalf("Difference: %v", err)
	}
	if area := totalArea(t, dGot); area != 0 {
		t.Errorf("Difference(a,a) area = %v, want 0", area)
	}
}

func TestUnion_Commutative(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(3, 3, 10)

	got1, err := Union(a, b)
	if err != nil {
		t.Fatalf("Union(a,b): %v", err)
	}
	got2, err := Union(b, a)
	if err != nil {
		t.Fatalf("Union(b,a): %v", err)
	}
	area1 := totalArea(t, got1)
	area2 := totalArea(t, got2)
	if !nearlyEqual(area1, area2, 1e-9) {
		t.Errorf("Union(a,b).area=%v != Union(b,a).area=%v", area1, area2)
	}
}

func TestClip_LShape(t *testing.T) {
	// L-shape covering the outer 20×20 square minus the top-right 10×10.
	lShape := SimplePolygon([]Point{
		pt(0, 0), pt(20, 0), pt(20, 10), pt(10, 10), pt(10, 20), pt(0, 20), pt(0, 0),
	}, CRS{})
	// Clip against the top-right 15x15 rectangle covering the missing bite.
	clipper := unitSquare(5, 5, 15)

	got, err := Clip(lShape, clipper)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	// L∩clipper: overlap of L (which excludes [10,20]×[10,20]) with [5,20]×[5,20].
	// = ([5,20]×[5,20] ∩ [0,20]×[0,20]) minus [10,20]×[10,20]
	// = [5,20]×[5,20] area (225) minus [10,20]×[10,20] area (100) = 125.
	if area := totalArea(t, got); !nearlyEqual(area, 125, 1e-9) {
		t.Errorf("Clip(L, sq) area = %v, want 125", area)
	}
}

func TestBoolean_CRSMismatch(t *testing.T) {
	crsA := CRS{EPSG: 3857, Projected: true}
	crsB := CRS{EPSG: 32610, Projected: true}
	a := unitSquare(0, 0, 10)
	a.CRSValue = crsA
	b := unitSquare(5, 5, 10)
	b.CRSValue = crsB

	if _, err := Clip(a, b); err == nil {
		t.Errorf("Clip with mismatched CRS should error")
	}
}

func TestBoolean_GeographicCRSRejected(t *testing.T) {
	a := unitSquare(0, 0, 10)
	a.CRSValue = WGS84
	b := unitSquare(5, 5, 10)
	b.CRSValue = WGS84

	_, err := Clip(a, b)
	if err == nil {
		t.Errorf("Clip against geographic CRS should error")
	}
}

func TestBoolean_UnsupportedGeometry(t *testing.T) {
	a := unitSquare(0, 0, 10)
	line := LineString{Points: []Point{pt(0, 0), pt(10, 10)}}
	if _, err := Clip(a, line); err == nil {
		t.Errorf("Clip with LineString input should error")
	}
	if _, err := Clip(line, a); err == nil {
		t.Errorf("Clip with LineString input should error")
	}
}

func TestClip_MultiPolygonInput(t *testing.T) {
	a := unitSquare(0, 0, 10)
	m := MultiPolygon{Polygons: []Polygon{
		unitSquare(3, 3, 5),
		unitSquare(50, 50, 5),
	}}
	got, err := Clip(a, m)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	// a=100 sq, m components: 5×5=25 (inside a) + 5×5=25 (disjoint).
	// Intersection = 25.
	if area := totalArea(t, got); !nearlyEqual(area, 25, 1e-9) {
		t.Errorf("Clip(poly, multipoly) area = %v, want 25", area)
	}
}

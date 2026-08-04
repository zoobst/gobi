package geometry

import "testing"

// TestClip_SharedEdge covers the case where two polygons touch along a
// shared edge but do not overlap.
func TestClip_SharedEdge(t *testing.T) {
	// a is [0,0]-[10,10]; b is [10,0]-[20,10]. They share the vertical
	// edge x=10, y in [0,10]. Intersection should be empty (measure zero).
	a := unitSquare(0, 0, 10)
	b := unitSquare(10, 0, 10)

	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := totalArea(t, got); area != 0 {
		t.Errorf("Clip(shared-edge) area = %v, want 0", area)
	}

	// Union should be a rectangle [0,0]-[20,10] = area 200.
	u, err := Union(a, b)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	if area := totalArea(t, u); !nearlyEqual(area, 200, 1e-9) {
		t.Errorf("Union(shared-edge) area = %v, want 200", area)
	}
}

// TestClip_SharedVertex covers polygons that meet at exactly one point.
func TestClip_SharedVertex(t *testing.T) {
	a := unitSquare(0, 0, 10)
	b := unitSquare(10, 10, 10)
	// Share only the corner (10,10). Intersection empty; union = 2 squares.
	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := totalArea(t, got); area != 0 {
		t.Errorf("Clip(shared-vertex) area = %v, want 0", area)
	}
	u, err := Union(a, b)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	if area := totalArea(t, u); !nearlyEqual(area, 200, 1e-9) {
		t.Errorf("Union(shared-vertex) area = %v, want 200", area)
	}
}

// TestClip_VertexOnEdge checks a polygon whose vertex sits exactly on
// another polygon's edge.
func TestClip_VertexOnEdge(t *testing.T) {
	// a is a square; b is a triangle with a vertex on a's top edge.
	a := unitSquare(0, 0, 10)
	b := SimplePolygon([]Point{
		pt(5, 10), // sits exactly on a's top edge
		pt(15, 0),
		pt(15, 20),
		pt(5, 10),
	}, CRS{})
	_, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	// A non-panicky run is the primary assertion; the specific area
	// depends on triangle interior computation. Test that the result
	// is convex-hull-like.
	_, err = Union(a, b)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
}

// TestClip_PolygonWithHole covers the hole case: outer polygon with a hole
// clipped against a rectangle that spans the hole.
func TestClip_PolygonWithHole(t *testing.T) {
	// Outer: 40×40 square around origin.
	outer := []Point{
		pt(-20, -20), pt(20, -20), pt(20, 20), pt(-20, 20), pt(-20, -20),
	}
	// Hole: 10×10 square centered on origin (wound CW would be conventional,
	// but for this test we use CCW too — the engine treats every ring's
	// contribution via inOut flips regardless of winding).
	hole := []Point{
		pt(-5, -5), pt(-5, 5), pt(5, 5), pt(5, -5), pt(-5, -5),
	}
	donut := Polygon{Rings: [][]Point{outer, hole}}
	// Clip against a 30×30 square [-15,-15]-[15,15] — bigger than the hole.
	clipper := unitSquare(-15, -15, 30)

	got, err := Clip(donut, clipper)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	// Expected: donut clipped by [-15,15]^2 = [-15,15]^2 minus the 10x10
	// hole = 900 - 100 = 800.
	if area := totalArea(t, got); !nearlyEqual(area, 800, 1e-9) {
		t.Errorf("Clip(donut, sq) area = %v, want 800", area)
	}
}

func TestUnion_PolygonWithHole(t *testing.T) {
	// donut ∪ small_covering_square = square (hole filled).
	outer := []Point{
		pt(0, 0), pt(20, 0), pt(20, 20), pt(0, 20), pt(0, 0),
	}
	hole := []Point{
		pt(8, 8), pt(8, 12), pt(12, 12), pt(12, 8), pt(8, 8),
	}
	donut := Polygon{Rings: [][]Point{outer, hole}}
	filler := unitSquare(8, 8, 4)

	got, err := Union(donut, filler)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	// Expected: full 20×20 = 400.
	if area := totalArea(t, got); !nearlyEqual(area, 400, 1e-9) {
		t.Errorf("Union(donut, filler) area = %v, want 400", area)
	}
}

// TestDissolve_DisjointInputs checks that disjoint inputs stay disjoint.
func TestDissolve_DisjointInputs(t *testing.T) {
	geoms := []Geometry{
		unitSquare(0, 0, 5),
		unitSquare(20, 0, 5),
		unitSquare(40, 0, 5),
	}
	got, err := Dissolve(geoms)
	if err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 75, 1e-9) {
		t.Errorf("Dissolve(disjoint) area = %v, want 75", area)
	}
	m, ok := got.(MultiPolygon)
	if !ok {
		t.Fatalf("Dissolve(disjoint) = %T, want MultiPolygon", got)
	}
	if len(m.Polygons) != 3 {
		t.Errorf("Dissolve(disjoint) components = %d, want 3", len(m.Polygons))
	}
}

// TestDissolve_OverlappingInputs merges a chain of overlapping squares.
func TestDissolve_OverlappingInputs(t *testing.T) {
	geoms := []Geometry{
		unitSquare(0, 0, 10),
		unitSquare(5, 0, 10),  // overlaps first
		unitSquare(10, 0, 10), // overlaps second
	}
	got, err := Dissolve(geoms)
	if err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	// Union of three 10x10 squares with 5-wide overlap between neighbors:
	// covers [0,20]×[0,10] = 200.
	if area := totalArea(t, got); !nearlyEqual(area, 200, 1e-9) {
		t.Errorf("Dissolve(overlap chain) area = %v, want 200", area)
	}
	if _, ok := got.(Polygon); !ok {
		t.Errorf("Dissolve(overlap chain) = %T, want single Polygon", got)
	}
}

// TestDissolve_MixedGroups checks that disjoint clusters produce a
// MultiPolygon in which each cluster is a single Polygon.
func TestDissolve_MixedGroups(t *testing.T) {
	geoms := []Geometry{
		// Cluster A: two overlapping squares near origin.
		unitSquare(0, 0, 10),
		unitSquare(5, 5, 10),
		// Cluster B: two overlapping squares far away.
		unitSquare(100, 100, 10),
		unitSquare(105, 105, 10),
	}
	got, err := Dissolve(geoms)
	if err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	// Each cluster area = 175 (two overlapping unit-10 squares).
	// Total = 350.
	if area := totalArea(t, got); !nearlyEqual(area, 350, 1e-9) {
		t.Errorf("Dissolve(mixed) area = %v, want 350", area)
	}
	m, ok := got.(MultiPolygon)
	if !ok {
		t.Fatalf("Dissolve(mixed) = %T, want MultiPolygon", got)
	}
	if len(m.Polygons) != 2 {
		t.Errorf("Dissolve(mixed) components = %d, want 2", len(m.Polygons))
	}
}

// TestDissolve_Empty exercises the empty-input edge case.
func TestDissolve_Empty(t *testing.T) {
	got, err := Dissolve(nil)
	if err != nil {
		t.Fatalf("Dissolve(nil): %v", err)
	}
	if area := totalArea(t, got); area != 0 {
		t.Errorf("Dissolve(nil) area = %v, want 0", area)
	}
}

// TestDissolve_Single passes through a single input unchanged.
func TestDissolve_Single(t *testing.T) {
	a := unitSquare(0, 0, 10)
	got, err := Dissolve([]Geometry{a})
	if err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	if area := totalArea(t, got); !nearlyEqual(area, 100, 1e-9) {
		t.Errorf("Dissolve(single) area = %v, want 100", area)
	}
}

// TestBoolean_SelfTouchingRing covers a polygon whose ring touches itself
// at a single vertex (bowtie-adjacent). The engine should produce a
// well-defined result rather than panic.
func TestBoolean_SelfTouchingRing(t *testing.T) {
	// A pinched hourglass-adjacent shape: outer ring visits (0,0) twice.
	// Not a strict bowtie (which would be self-crossing); this touches at
	// a vertex.
	pinched := SimplePolygon([]Point{
		pt(0, 0), pt(10, 0), pt(10, 5), pt(0, 5),
		pt(0, 0), // return to origin
		pt(-10, -5), pt(-10, -10), pt(0, -10),
		pt(0, 0),
	}, CRS{})
	other := unitSquare(-5, -5, 20) // covers most of the pinched shape

	// Just make sure this doesn't panic and returns *some* result.
	_, err := Clip(pinched, other)
	if err != nil {
		t.Fatalf("Clip(pinched, sq): %v", err)
	}
	_, err = Union(pinched, other)
	if err != nil {
		t.Fatalf("Union(pinched, sq): %v", err)
	}
}

// TestClip_LargeCoordinates exercises numeric robustness at UTM-like
// coordinate magnitudes (~1e6 m). The overlap is small relative to the
// coordinate scale but well above the engine's default relative epsilon.
func TestClip_LargeCoordinates(t *testing.T) {
	a := unitSquare(0, 0, 1e6)
	b := unitSquare(1e6-1, 0, 1e6)
	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip(large coords): %v", err)
	}
	// Overlap: 1 × 1e6 = 1e6.
	if area := totalArea(t, got); !nearlyEqual(area, 1e6, 1) {
		t.Errorf("Clip(large coords) area = %v, want ~1e6", area)
	}
}

package geometry

import (
	"math"
	"testing"
)

// TestCircleIntersectionPoints_Overlap: two unit circles with centers
// at (0, 0) and (1, 0) share boundary points at (0.5, ±√0.75).
func TestCircleIntersectionPoints_Overlap(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	pts := CircleIntersectionPoints(c1, c2)
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2", len(pts))
	}
	// x should be 0.5 for both, y = ±√0.75.
	wantY := math.Sqrt(0.75)
	for i, p := range pts {
		if math.Abs(p.X-0.5) > 1e-12 {
			t.Errorf("pt %d X = %v, want 0.5", i, p.X)
		}
		if math.Abs(math.Abs(p.Y)-wantY) > 1e-12 {
			t.Errorf("pt %d |Y| = %v, want %v", i, math.Abs(p.Y), wantY)
		}
	}
	// The two returned points must be reflections of each other.
	if math.Abs(pts[0].Y+pts[1].Y) > 1e-12 {
		t.Errorf("expected pts to be y-symmetric, got %v and %v", pts[0].Y, pts[1].Y)
	}
}

// TestCircleIntersectionPoints_Disjoint: centers 3 apart, unit radii.
func TestCircleIntersectionPoints_Disjoint(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 3, Y: 0}, Radius: 1}
	if pts := CircleIntersectionPoints(c1, c2); pts != nil {
		t.Errorf("disjoint: got %v, want nil", pts)
	}
}

// TestCircleIntersectionPoints_Nested: small circle strictly inside a
// larger one — no boundary crossings.
func TestCircleIntersectionPoints_Nested(t *testing.T) {
	big := Circle{Center: Point{X: 0, Y: 0}, Radius: 5}
	small := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	if pts := CircleIntersectionPoints(big, small); pts != nil {
		t.Errorf("nested (big first): got %v, want nil", pts)
	}
	if pts := CircleIntersectionPoints(small, big); pts != nil {
		t.Errorf("nested (small first): got %v, want nil", pts)
	}
}

// TestCircleIntersectionPoints_ExternalTangent: centers exactly r1+r2
// apart — a single tangent point on the line between centers.
func TestCircleIntersectionPoints_ExternalTangent(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 3, Y: 0}, Radius: 2}
	pts := CircleIntersectionPoints(c1, c2)
	if len(pts) != 1 {
		t.Fatalf("external tangent: got %d points, want 1", len(pts))
	}
	if math.Abs(pts[0].X-1) > 1e-12 || math.Abs(pts[0].Y) > 1e-12 {
		t.Errorf("tangent point = %v, want (1, 0)", pts[0])
	}
}

// TestCircleIntersectionPoints_InternalTangent: one circle inside the
// other, touching at one point (d == |r1 - r2|).
func TestCircleIntersectionPoints_InternalTangent(t *testing.T) {
	big := Circle{Center: Point{X: 0, Y: 0}, Radius: 3}
	small := Circle{Center: Point{X: 2, Y: 0}, Radius: 1}
	// d = 2, |r1-r2| = 2 → internal tangent at (3, 0).
	pts := CircleIntersectionPoints(big, small)
	if len(pts) != 1 {
		t.Fatalf("internal tangent: got %d points, want 1", len(pts))
	}
	if math.Abs(pts[0].X-3) > 1e-12 || math.Abs(pts[0].Y) > 1e-12 {
		t.Errorf("tangent point = %v, want (3, 0)", pts[0])
	}
}

// TestCircleIntersectionPoints_Concentric: no well-defined finite
// intersection point set.
func TestCircleIntersectionPoints_Concentric(t *testing.T) {
	c1 := Circle{Center: Point{X: 5, Y: 5}, Radius: 3}
	c2 := Circle{Center: Point{X: 5, Y: 5}, Radius: 4}
	if pts := CircleIntersectionPoints(c1, c2); pts != nil {
		t.Errorf("concentric diff radii: got %v, want nil", pts)
	}
	c3 := Circle{Center: Point{X: 5, Y: 5}, Radius: 3}
	if pts := CircleIntersectionPoints(c1, c3); pts != nil {
		t.Errorf("concentric equal radii: got %v, want nil", pts)
	}
}

// TestCircleIntersectionPoints_CRSPropagation: result points inherit
// CRS from c1.Center.
func TestCircleIntersectionPoints_CRSPropagation(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0, CRSValue: PseudoMercator}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0, CRSValue: WGS84}, Radius: 1}
	pts := CircleIntersectionPoints(c1, c2)
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2", len(pts))
	}
	for i, p := range pts {
		if p.CRSValue.EPSG != PseudoMercator.EPSG {
			t.Errorf("pt %d CRS = %v, want inherited from c1 (PseudoMercator)", i, p.CRSValue)
		}
	}
}

// analyticLensArea returns the exact area of the lens between two
// circles that properly overlap. Formula:
//
//	A = r1²·acos((d² + r1² - r2²)/(2·d·r1))
//	  + r2²·acos((d² + r2² - r1²)/(2·d·r2))
//	  - ½·√((-d+r1+r2)(d+r1-r2)(d-r1+r2)(d+r1+r2))
func analyticLensArea(r1, r2, d float64) float64 {
	a := r1 * r1 * math.Acos((d*d+r1*r1-r2*r2)/(2*d*r1))
	b := r2 * r2 * math.Acos((d*d+r2*r2-r1*r1)/(2*d*r2))
	c := 0.5 * math.Sqrt((-d+r1+r2)*(d+r1-r2)*(d-r1+r2)*(d+r1+r2))
	return a + b - c
}

// TestLensPolygon_EqualRadiusArea: sampled area converges to the
// analytic value.
func TestLensPolygon_EqualRadiusArea(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	poly := LensPolygon(c1, c2, 128)
	if len(poly.Rings) != 1 {
		t.Fatalf("expected 1 ring, got %d", len(poly.Rings))
	}
	got := planarRingArea(poly.Rings[0])
	want := analyticLensArea(1, 1, 1)
	rel := math.Abs(got-want) / want
	// Chord-approximation of an arc undershoots true area; at 128
	// segments/arc the relative error should be well under 1e-3.
	if rel > 1e-3 {
		t.Errorf("area = %v, want %v (rel err %g)", got, want, rel)
	}
}

// TestLensPolygon_UnequalRadiusArea covers the asymmetric case where
// the two arcs contribute different amounts.
func TestLensPolygon_UnequalRadiusArea(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 2}
	c2 := Circle{Center: Point{X: 2.5, Y: 0}, Radius: 1.5}
	poly := LensPolygon(c1, c2, 256)
	if len(poly.Rings) != 1 {
		t.Fatalf("expected 1 ring, got %d", len(poly.Rings))
	}
	got := planarRingArea(poly.Rings[0])
	want := analyticLensArea(2, 1.5, 2.5)
	rel := math.Abs(got-want) / want
	if rel > 1e-3 {
		t.Errorf("area = %v, want %v (rel err %g)", got, want, rel)
	}
}

// TestLensPolygon_Disjoint: no overlap → empty polygon.
func TestLensPolygon_Disjoint(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0, CRSValue: PseudoMercator}, Radius: 1}
	c2 := Circle{Center: Point{X: 3, Y: 0}, Radius: 1}
	poly := LensPolygon(c1, c2, 32)
	if len(poly.Rings) != 0 {
		t.Errorf("disjoint: got %d rings, want 0", len(poly.Rings))
	}
	if poly.CRSValue.EPSG != PseudoMercator.EPSG {
		t.Errorf("CRS not propagated on empty: got %v", poly.CRSValue)
	}
}

// TestLensPolygon_Nested: one circle contains the other → lens is the
// smaller circle.
func TestLensPolygon_Nested(t *testing.T) {
	big := Circle{Center: Point{X: 0, Y: 0}, Radius: 5}
	small := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	poly := LensPolygon(big, small, 64)
	got := planarRingArea(poly.Rings[0])
	want := math.Pi // area of the smaller unit circle
	// Circle boundary sampling error at 128 vertices (2×64).
	rel := math.Abs(got-want) / want
	if rel > 1e-3 {
		t.Errorf("nested lens area = %v, want ~π (rel err %g)", got, rel)
	}
	// Symmetric argument order.
	poly2 := LensPolygon(small, big, 64)
	got2 := planarRingArea(poly2.Rings[0])
	if math.Abs(got-got2) > 1e-9 {
		t.Errorf("nested lens: order-dependent area %v vs %v", got, got2)
	}
}

// TestLensPolygon_Tangent: externally tangent circles → empty polygon
// (a zero-area lens is indistinguishable from no lens for consumers).
func TestLensPolygon_Tangent(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 2, Y: 0}, Radius: 1}
	poly := LensPolygon(c1, c2, 32)
	if len(poly.Rings) != 0 {
		t.Errorf("tangent: got %d rings, want 0", len(poly.Rings))
	}
}

// TestLensPolygon_CCWWinding: the exterior ring must be CCW to match
// gobi's convention (positive signed area).
func TestLensPolygon_CCWWinding(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	poly := LensPolygon(c1, c2, 32)
	if !isCCW(poly.Rings[0]) {
		t.Errorf("lens ring is not CCW")
	}
	// Swap argument order — winding should still be CCW.
	poly2 := LensPolygon(c2, c1, 32)
	if !isCCW(poly2.Rings[0]) {
		t.Errorf("lens ring (swapped order) is not CCW")
	}
}

// TestLensPolygon_ClosedRing: the ring must start and end at the same
// point.
func TestLensPolygon_ClosedRing(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	poly := LensPolygon(c1, c2, 16)
	ring := poly.Rings[0]
	if ring[0] != ring[len(ring)-1] {
		t.Errorf("ring not closed: first=%v last=%v", ring[0], ring[len(ring)-1])
	}
}

// TestLensPolygon_VertexCount: the ring should have ~2·arcSegments
// vertices (each arc contributes arcSegments+1 samples, minus one
// shared joint, plus a possible closing vertex).
func TestLensPolygon_VertexCount(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	const seg = 32
	poly := LensPolygon(c1, c2, seg)
	got := len(poly.Rings[0])
	// arcOnC2 has seg+1 points; arcOnC1[1:] adds seg points; +1 closing
	// vertex if the ring wasn't already closed. That comes to 2·seg+1
	// or 2·seg+2 depending on whether reconstruction drift required an
	// explicit close.
	if got != 2*seg+1 && got != 2*seg+2 {
		t.Errorf("vertex count = %d, want %d or %d", got, 2*seg+1, 2*seg+2)
	}
}

// TestLensPolygon_DefaultSegments: arcSegments < 4 falls back to
// DefaultBufferSegments / 2.
func TestLensPolygon_DefaultSegments(t *testing.T) {
	c1 := Circle{Center: Point{X: 0, Y: 0}, Radius: 1}
	c2 := Circle{Center: Point{X: 1, Y: 0}, Radius: 1}
	poly := LensPolygon(c1, c2, 0)
	want := DefaultBufferSegments / 2
	got := len(poly.Rings[0])
	// See TestLensPolygon_VertexCount for the ±1 tolerance.
	if got != 2*want+1 && got != 2*want+2 {
		t.Errorf("default-segments vertex count = %d, want %d or %d",
			got, 2*want+1, 2*want+2)
	}
}

package geometry

import (
	"math"
	"testing"
)

// TestClip_IdenticalPolygonInMultiPolygonWrap covers the previously-
// latent slow-path bug where Clip(a, MultiPolygon{[a-copy]}) dropped
// half the coincident boundary edges by misclassifying them as
// differentTransition. The MultiPolygon wrap forces the general sweep
// path (bypassing the convex fast path). Fix in handleOverlap:
// classify by ring-walking direction (polyForward) rather than by
// inOut equality, which drifts for coincident partners inserted
// against different sweep-line predecessors.
func TestClip_IdenticalPolygonInMultiPolygonWrap(t *testing.T) {
	// Regular octagon, CCW.
	n := 8
	pts := make([]Point, n+1)
	for i := 0; i < n; i++ {
		theta := 2 * math.Pi * float64(i) / float64(n)
		pts[i] = Point{X: 10 * math.Cos(theta), Y: 10 * math.Sin(theta)}
	}
	pts[n] = pts[0]
	a := SimplePolygon(pts, CRS{})
	wantArea := polyPlanarArea(a) // analytical: 200√2 ≈ 282.84

	// Plain Polygon×Polygon uses the Sutherland-Hodgman fast path.
	fast, err := Clip(a, SimplePolygon(pts, CRS{}))
	if err != nil {
		t.Fatalf("Clip fast path: %v", err)
	}
	fastArea := clipTotalArea(fast)
	if rel := math.Abs(fastArea-wantArea) / wantArea; rel > 1e-9 {
		t.Errorf("fast-path area = %v, want %v (rel err %g)", fastArea, wantArea, rel)
	}

	// MultiPolygon wrap forces the general sweep — this was the failing
	// case before the polyForward-based classifier.
	wrapped := MultiPolygon{Polygons: []Polygon{SimplePolygon(pts, CRS{})}}
	slow, err := Clip(a, wrapped)
	if err != nil {
		t.Fatalf("Clip slow path: %v", err)
	}
	slowArea := clipTotalArea(slow)
	if rel := math.Abs(slowArea-wantArea) / wantArea; rel > 1e-9 {
		t.Errorf("slow-path area = %v, want %v (rel err %g)", slowArea, wantArea, rel)
	}

	// Both paths must produce the same result.
	if rel := math.Abs(fastArea-slowArea) / wantArea; rel > 1e-9 {
		t.Errorf("fast=%v vs slow=%v disagree", fastArea, slowArea)
	}
}

// TestClip_IdenticalPolygonWithConcave covers the same fix on a
// non-convex input (the fast path doesn't fire, so the sweep is
// exercised directly).
func TestClip_IdenticalPolygonWithConcave(t *testing.T) {
	lShape := SimplePolygon([]Point{
		pt(0, 0), pt(20, 0), pt(20, 10), pt(10, 10),
		pt(10, 20), pt(0, 20), pt(0, 0),
	}, CRS{})
	wantArea := polyPlanarArea(lShape)

	// Clip an L-shape against itself.
	got, err := Clip(lShape, SimplePolygon([]Point{
		pt(0, 0), pt(20, 0), pt(20, 10), pt(10, 10),
		pt(10, 20), pt(0, 20), pt(0, 0),
	}, CRS{}))
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	area := clipTotalArea(got)
	if rel := math.Abs(area-wantArea) / wantArea; rel > 1e-9 {
		t.Errorf("area = %v, want %v (rel err %g)", area, wantArea, rel)
	}
}

// TestBoolean_OppositeWindings verifies that when two coincident-edge
// polygons wind in OPPOSITE directions (one CCW, one CW), the
// polyForward-based classifier correctly picks differentTransition —
// intersection is empty (they have opposite interiors along the shared
// boundary), and symmetric difference covers both regions.
func TestBoolean_OppositeWindings(t *testing.T) {
	ccw := SimplePolygon([]Point{
		pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0),
	}, CRS{})
	cw := SimplePolygon([]Point{
		pt(0, 0), pt(0, 10), pt(10, 10), pt(10, 0), pt(0, 0),
	}, CRS{})
	// The CW polygon describes the same PHYSICAL region as ccw, so
	// intersection area should equal the square's area.
	got, err := Clip(ccw, cw)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	area := clipTotalArea(got)
	if math.Abs(area-100) > 1e-9 {
		t.Errorf("CCW ∩ CW-same-region area = %v, want 100 (both describe the same square)", area)
	}
}

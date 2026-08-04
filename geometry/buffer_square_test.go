package geometry

import (
	"math"
	"testing"
)

func TestBuffer_Point_Square(t *testing.T) {
	p := Point{X: 5, Y: 5}
	got, err := Buffer(p, 3, BufferOptions{Style: BufferSquare})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	poly, ok := got.(Polygon)
	if !ok {
		t.Fatalf("got %T, want Polygon", got)
	}
	if len(poly.Rings) != 1 {
		t.Fatalf("rings = %d, want 1", len(poly.Rings))
	}
	// Ring is (2,2)-(8,2)-(8,8)-(2,8)-(2,2). Area = 6×6 = 36.
	if a := polyPlanarArea(poly); !nearlyEqual(a, 36, 1e-9) {
		t.Errorf("area = %v, want 36", a)
	}
	// Exactly 5 vertices (4 corners + closing).
	if n := len(poly.Rings[0]); n != 5 {
		t.Errorf("vertex count = %d, want 5", n)
	}
}

func TestBuffer_Point_Round_UnchangedByDefault(t *testing.T) {
	// Ensure round is still the default (BufferOptions zero value).
	p := Point{X: 5, Y: 5}
	got, err := Buffer(p, 3, BufferOptions{})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	poly := got.(Polygon)
	// Default 32 segments → 33-vertex ring including closing.
	if n := len(poly.Rings[0]); n != 33 {
		t.Errorf("default vertex count = %d, want 33", n)
	}
	// Area ~ π*r² = 9π ≈ 28.27; the 32-gon under-approximates a circle
	// slightly.
	a := polyPlanarArea(poly)
	if a < 28 || a > 29 {
		t.Errorf("area = %v, want ~28.3", a)
	}
}

func TestBuffer_LineString_Square(t *testing.T) {
	// Horizontal 2-point line from (0,0) to (10,0). Square buffer of
	// distance 1 → rectangle (-1, -1) to (11, 1). Area = 12×2 = 24.
	l := LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 0}}}
	got, err := Buffer(l, 1, BufferOptions{Style: BufferSquare})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	poly := got.(Polygon)
	if a := polyPlanarArea(poly); !nearlyEqual(a, 24, 1e-9) {
		t.Errorf("area = %v, want 24", a)
	}
}

func TestBuffer_Polygon_Square_Mitre(t *testing.T) {
	// Unit square. Square buffer of 1 mitres out to a 3×3 square = area 9.
	// Round buffer of 1 would be 1+π+1 = 2+π ≈ 5.14 (with rounded corners
	// approximated by arcs). Square version has sharp corners.
	poly := SimplePolygon([]Point{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}, CRS{})
	got, err := Buffer(poly, 1, BufferOptions{Style: BufferSquare})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	out := got.(Polygon)
	if a := polyPlanarArea(out); !nearlyEqual(a, 9, 1e-9) {
		t.Errorf("area = %v, want 9 (mitre corners on unit square + dist 1)", a)
	}
}

func TestBuffer_MultiPoint_Square(t *testing.T) {
	// Two disjoint points → MultiPolygon of 2 squares.
	mp := MultiPoint{Points: []Point{{X: 0, Y: 0}, {X: 100, Y: 100}}}
	got, err := Buffer(mp, 2, BufferOptions{Style: BufferSquare})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	m := got.(MultiPolygon)
	if len(m.Polygons) != 2 {
		t.Fatalf("components = %d, want 2", len(m.Polygons))
	}
	// Each square is 4×4 = 16, total 32.
	total := 0.0
	for _, p := range m.Polygons {
		total += polyPlanarArea(p)
	}
	if !nearlyEqual(total, 32, 1e-9) {
		t.Errorf("total area = %v, want 32", total)
	}
}

// Sanity: at very high round-segment counts, round buffer's area
// approaches the analytical circle area. This test just confirms that
// adding BufferSquare didn't disturb the round path.
func TestBuffer_Point_Round_ManySegments(t *testing.T) {
	p := Point{X: 0, Y: 0}
	got, err := Buffer(p, 10, BufferOptions{Segments: 1024})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	poly := got.(Polygon)
	a := polyPlanarArea(poly)
	// π*r² = 100π ≈ 314.159...; a 1024-gon inscribed in the circle
	// under-approximates by ~2e-3, so we allow 1e-2 tolerance.
	want := math.Pi * 100
	if math.Abs(a-want) > 1e-2 {
		t.Errorf("1024-segment round area = %v, want ~%v", a, want)
	}
}

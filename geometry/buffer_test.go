package geometry

import (
	"math"
	"testing"
)

// approxRel checks got is within relTol relative to want.
func approxRel(t *testing.T, got, want, relTol float64, msg string) {
	t.Helper()
	if want == 0 {
		if math.Abs(got) > relTol {
			t.Fatalf("%s: got %v, want ~0 (±%v)", msg, got, relTol)
		}
		return
	}
	if math.Abs((got-want)/want) > relTol {
		t.Fatalf("%s: got %v, want %v (relTol %v)", msg, got, want, relTol)
	}
}

func TestPointBuffer_AreaIsPiRSquared(t *testing.T) {
	p := Point{X: 100, Y: 200, CRSValue: PseudoMercator}
	poly := p.Buffer(10, 128)
	poly.CRSValue = PseudoMercator
	a, err := poly.Area(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	want := math.Pi * 100
	// 128-segment discretization underestimates area by ~π/n² per unit area.
	// Allow 0.5% relative tolerance.
	approxRel(t, a, want, 5e-3, "Point.Buffer area")
}

func TestPointBuffer_ClosedRing(t *testing.T) {
	poly := Point{X: 0, Y: 0}.Buffer(5, 16)
	r := poly.Rings[0]
	if r[0] != r[len(r)-1] {
		t.Fatalf("buffer ring not closed: first=%+v last=%+v", r[0], r[len(r)-1])
	}
}

func TestLineStringBuffer_ExpectedArea(t *testing.T) {
	// A straight line of length L buffered by radius r produces an area of
	// L*2r + pi*r² (two half-caps at the endpoints).
	l := LineString{
		Points:   []Point{{X: 0, Y: 0}, {X: 100, Y: 0}},
		CRSValue: PseudoMercator,
	}
	r := 5.0
	poly := l.Buffer(r, 128)
	poly.CRSValue = PseudoMercator
	a, err := poly.Area(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	want := 100*2*r + math.Pi*r*r
	approxRel(t, a, want, 5e-3, "LineString.Buffer area")
}

func TestLineStringBuffer_TwoSegmentTurn(t *testing.T) {
	// A 90° elbow should still produce a valid polygon.
	l := LineString{
		Points:   []Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}},
		CRSValue: PseudoMercator,
	}
	poly := l.Buffer(2, 32)
	if len(poly.Rings[0]) < 4 {
		t.Fatalf("elbow buffer ring too short: %+v", poly.Rings[0])
	}
	// Ring should be closed.
	r := poly.Rings[0]
	if r[0] != r[len(r)-1] {
		t.Fatal("elbow buffer ring not closed")
	}
}

func TestPolygonBuffer_ConvexAreaGrows(t *testing.T) {
	// Convex square of side 10, buffered outward by 2 should have area
	// approximately 10*10 + 4*10*2 + pi*4 = 100 + 80 + ~12.57 = ~192.57
	poly := SimplePolygon([]Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}, PseudoMercator)
	buffered := poly.Buffer(2, 128)
	buffered.CRSValue = PseudoMercator
	a, err := buffered.Area(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	want := 100.0 + 4*10*2 + math.Pi*4
	approxRel(t, a, want, 1e-2, "Polygon.Buffer area")
}

func TestBuffer_NegativeDistanceErrors(t *testing.T) {
	_, err := Buffer(Point{}, -1, BufferOptions{})
	if err == nil {
		t.Fatal("expected error for negative distance")
	}
}

func TestBuffer_UnsupportedGeometryErrors(t *testing.T) {
	// The GeometryCollection route through Buffer is undefined here — we
	// route Multi* explicitly but let heterogeneous collections error to
	// keep behavior predictable.
	_, err := Buffer(GeometryCollection{}, 1, BufferOptions{})
	if err == nil {
		t.Fatal("expected error for GeometryCollection input")
	}
}

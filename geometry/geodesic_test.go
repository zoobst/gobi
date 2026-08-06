package geometry

import (
	"errors"
	"math"
	"testing"
)

func TestSampleGeodesic_EndpointsPreserved(t *testing.T) {
	a := Point{X: -73.9857, Y: 40.7484, CRSValue: WGS84}     // NYC-ish
	b := Point{X: 139.6503, Y: 35.6762, CRSValue: WGS84}     // Tokyo-ish
	got, err := SampleGeodesic(a, b, 64)
	if err != nil {
		t.Fatalf("SampleGeodesic: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("len = %d, want 64", len(got))
	}
	// First and last must match input exactly.
	if got[0].X != a.X || got[0].Y != a.Y {
		t.Errorf("first = %v, want %v", got[0], a)
	}
	if got[63].X != b.X || got[63].Y != b.Y {
		t.Errorf("last = %v, want %v", got[63], b)
	}
}

func TestSampleGeodesic_ArcLengthMatchesHaversine(t *testing.T) {
	// Along a great-circle arc from A to B, the sum of Haversine
	// distances between consecutive samples should approach the
	// direct Haversine(A,B) as n → ∞. With n=100 the two should
	// agree to within ~0.01%.
	a := Point{X: 0, Y: 0, CRSValue: WGS84}
	b := Point{X: 90, Y: 45, CRSValue: WGS84}
	direct, err := Haversine(a, b, UnitMeters)
	if err != nil {
		t.Fatalf("Haversine: %v", err)
	}
	samples, err := SampleGeodesic(a, b, 100)
	if err != nil {
		t.Fatalf("SampleGeodesic: %v", err)
	}
	var summed float64
	for i := 0; i < len(samples)-1; i++ {
		s, _ := Haversine(samples[i], samples[i+1], UnitMeters)
		summed += s
	}
	rel := math.Abs(summed-direct) / direct
	if rel > 1e-4 {
		t.Errorf("summed arc %v vs direct %v (rel err %g)", summed, direct, rel)
	}
}

func TestSampleGeodesic_CoincidentPoints(t *testing.T) {
	// Coincident inputs — the arc is degenerate but not an error.
	// Should just repeat the point.
	a := Point{X: 10, Y: 20, CRSValue: WGS84}
	got, err := SampleGeodesic(a, a, 5)
	if err != nil {
		t.Fatalf("SampleGeodesic: %v", err)
	}
	for i, p := range got {
		if p.X != a.X || p.Y != a.Y {
			t.Errorf("sample %d = %v, want %v", i, p, a)
		}
	}
}

func TestSampleGeodesic_AntipodalErrors(t *testing.T) {
	a := Point{X: 0, Y: 0, CRSValue: WGS84}
	b := Point{X: 180, Y: 0, CRSValue: WGS84} // exact antipode
	_, err := SampleGeodesic(a, b, 10)
	if !errors.Is(err, ErrAntipodalPoints) {
		t.Errorf("err = %v, want ErrAntipodalPoints", err)
	}
}

func TestSampleGeodesic_RejectsProjectedCRS(t *testing.T) {
	a := Point{X: 0, Y: 0, CRSValue: PseudoMercator}
	b := Point{X: 1000, Y: 1000, CRSValue: PseudoMercator}
	_, err := SampleGeodesic(a, b, 5)
	if !errors.Is(err, ErrGeodesicRequiresGeographic) {
		t.Errorf("err = %v, want ErrGeodesicRequiresGeographic", err)
	}
}

func TestSampleGeodesic_TooFewPoints(t *testing.T) {
	a := Point{X: 0, Y: 0, CRSValue: WGS84}
	b := Point{X: 1, Y: 1, CRSValue: WGS84}
	if _, err := SampleGeodesic(a, b, 1); err == nil {
		t.Errorf("n=1: expected error")
	}
}

// TestSampleGeodesic_CorrectMidpoint verifies the geodesic midpoint
// against the analytic value. From (0°N, 0°E) to (0°N, 90°E) the
// geodesic midpoint is (0°N, 45°E) — the arc stays on the equator.
func TestSampleGeodesic_CorrectMidpoint(t *testing.T) {
	a := Point{X: 0, Y: 0, CRSValue: WGS84}
	b := Point{X: 90, Y: 0, CRSValue: WGS84}
	got, err := SampleGeodesic(a, b, 3)
	if err != nil {
		t.Fatalf("SampleGeodesic: %v", err)
	}
	mid := got[1]
	if math.Abs(mid.X-45) > 1e-9 || math.Abs(mid.Y) > 1e-9 {
		t.Errorf("midpoint = %v, want (45, 0)", mid)
	}
}

// TestSampleGeodesic_GreatCircleNotCartesian verifies that the
// midpoint of the New York → Tokyo arc lands well north of the
// straight-line midpoint (a Mercator straight line would give
// ~lat 38°; the great-circle midpoint is up around 68°N over the
// Bering Sea). This is the "map projection lies to you" case the
// densify path exists to fix.
func TestSampleGeodesic_GreatCircleNotCartesian(t *testing.T) {
	nyc := Point{X: -74.006, Y: 40.7128, CRSValue: WGS84}
	tokyo := Point{X: 139.6917, Y: 35.6895, CRSValue: WGS84}
	got, err := SampleGeodesic(nyc, tokyo, 3)
	if err != nil {
		t.Fatalf("SampleGeodesic: %v", err)
	}
	mid := got[1]
	if mid.Y < 60 || mid.Y > 75 {
		t.Errorf("NYC↔Tokyo midpoint lat = %v; expected roughly 60-75°N (great-circle route)", mid.Y)
	}
}

func TestDensifyGeodesic_StepPreservesEndpoints(t *testing.T) {
	// A single 90° arc from (0,0) to (90,0), sampled at ≤ 200 km.
	// The arc is 10,000 km, so we should get ~50 segments.
	l := LineString{
		Points:   []Point{{X: 0, Y: 0}, {X: 90, Y: 0}},
		CRSValue: WGS84,
	}
	got, err := DensifyGeodesic(l, 200_000)
	if err != nil {
		t.Fatalf("DensifyGeodesic: %v", err)
	}
	// Endpoints preserved.
	if got.Points[0].X != 0 || got.Points[0].Y != 0 {
		t.Errorf("first = %v, want (0,0)", got.Points[0])
	}
	last := got.Points[len(got.Points)-1]
	if last.X != 90 || last.Y != 0 {
		t.Errorf("last = %v, want (90,0)", last)
	}
	// Every internal step should be ≤ ~200 km + a bit of slack.
	for i := 0; i < len(got.Points)-1; i++ {
		d, _ := Haversine(got.Points[i], got.Points[i+1], UnitMeters)
		if d > 210_000 {
			t.Errorf("segment %d length = %v m, want ≤ ~200 km", i, d)
		}
	}
	// Vertex count: 10,000 km / 200 km ≈ 51 samples for the single edge.
	if len(got.Points) < 45 || len(got.Points) > 60 {
		t.Errorf("vertex count = %d, want ~50-55", len(got.Points))
	}
}

func TestDensifyGeodesic_MultiSegmentJoinsWithoutDuplicate(t *testing.T) {
	l := LineString{
		Points: []Point{
			{X: 0, Y: 0}, {X: 45, Y: 0}, {X: 90, Y: 0},
		},
		CRSValue: WGS84,
	}
	got, err := DensifyGeodesic(l, 1_000_000) // 1000 km step
	if err != nil {
		t.Fatalf("DensifyGeodesic: %v", err)
	}
	// Middle joint (X=45) should appear exactly once, not twice.
	count45 := 0
	for _, p := range got.Points {
		if math.Abs(p.X-45) < 1e-9 && math.Abs(p.Y) < 1e-9 {
			count45++
		}
	}
	if count45 != 1 {
		t.Errorf("joint at (45,0) appears %d times, want exactly 1", count45)
	}
}

func TestDensifyGeodesic_ZeroStepPassesThrough(t *testing.T) {
	l := LineString{
		Points:   []Point{{X: 0, Y: 0}, {X: 90, Y: 0}},
		CRSValue: WGS84,
	}
	got, err := DensifyGeodesic(l, 0)
	if err != nil {
		t.Fatalf("DensifyGeodesic: %v", err)
	}
	if len(got.Points) != 2 {
		t.Errorf("len = %d, want 2 (pass-through)", len(got.Points))
	}
}

func TestDensifyGeodesic_ProjectedCRSErrors(t *testing.T) {
	l := LineString{
		Points:   []Point{{X: 0, Y: 0}, {X: 1000, Y: 1000}},
		CRSValue: PseudoMercator,
	}
	_, err := DensifyGeodesic(l, 100)
	if !errors.Is(err, ErrGeodesicRequiresGeographic) {
		t.Errorf("err = %v, want ErrGeodesicRequiresGeographic", err)
	}
}

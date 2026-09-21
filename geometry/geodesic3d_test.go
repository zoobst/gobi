package geometry

import (
	"math"
	"testing"
)

// TestLonLatAltToECEFSlabs_KnownReferences — spot-check ECEF
// conversion against published reference values for a few
// well-known points. Values from NGA / EPSG worked examples;
// tolerance 1 m absolute for WGS84 (sub-cm is achievable but
// the test's fixed-decimal constants aren't that precise).
func TestLonLatAltToECEFSlabs_KnownReferences(t *testing.T) {
	cases := []struct {
		name                string
		lon, lat, alt       float64
		wantX, wantY, wantZ float64
		tol                 float64
	}{
		{
			// (0, 0, 0): on the equator at the prime meridian. ECEF
			// = (a, 0, 0) where a = WGS84 semi-major axis.
			name: "origin", lon: 0, lat: 0, alt: 0,
			wantX: 6378137.0, wantY: 0, wantZ: 0, tol: 0.001,
		},
		{
			// (0, 90, 0): north pole. ECEF = (0, 0, b) where b is
			// the semi-minor axis. b = a * (1 - f) = 6356752.314…
			name: "north-pole", lon: 0, lat: 90, alt: 0,
			wantX: 0, wantY: 0, wantZ: 6356752.3142, tol: 0.01,
		},
		{
			// (90, 0, 0): equator at 90° longitude. ECEF = (0, a, 0).
			name: "east", lon: 90, lat: 0, alt: 0,
			wantX: 0, wantY: 6378137.0, wantZ: 0, tol: 0.001,
		},
	}
	lons := make([]float64, len(cases))
	lats := make([]float64, len(cases))
	alts := make([]float64, len(cases))
	for i, c := range cases {
		lons[i] = c.lon
		lats[i] = c.lat
		alts[i] = c.alt
	}
	outX := make([]float64, len(cases))
	outY := make([]float64, len(cases))
	outZ := make([]float64, len(cases))
	LonLatAltToECEFSlabs(lons, lats, alts, outX, outY, outZ)
	for i, c := range cases {
		if math.Abs(outX[i]-c.wantX) > c.tol {
			t.Errorf("%s: X = %v, want %v ± %v", c.name, outX[i], c.wantX, c.tol)
		}
		if math.Abs(outY[i]-c.wantY) > c.tol {
			t.Errorf("%s: Y = %v, want %v ± %v", c.name, outY[i], c.wantY, c.tol)
		}
		if math.Abs(outZ[i]-c.wantZ) > c.tol {
			t.Errorf("%s: Z = %v, want %v ± %v", c.name, outZ[i], c.wantZ, c.tol)
		}
	}
}

// TestDistance3DGeodesicFromSlabs_KnownPairs — verify ECEF-Euclidean
// distance on published lat/lon pairs.
func TestDistance3DGeodesicFromSlabs_KnownPairs(t *testing.T) {
	// (0, 0) → (0, 0): zero distance.
	// (0, 0) → (180, 0): chord = 2a = ~12756274 m.
	// (0, 0) → (0, 90): chord from equator-prime-meridian to N pole
	// via ECEF ≈ sqrt(a² + b²) but really sqrt(a² + b²) is off —
	// the chord is a straight line through Earth from (a,0,0) to
	// (0,0,b), so length = sqrt(a² + b²) = ~9012126.
	lons1 := []float64{0, 0, 0}
	lats1 := []float64{0, 0, 0}
	alts1 := []float64{0, 0, 0}
	lons2 := []float64{0, 180, 0}
	lats2 := []float64{0, 0, 90}
	alts2 := []float64{0, 0, 0}
	out := make([]float64, 3)
	Distance3DGeodesicFromSlabs(lons1, lats1, alts1, lons2, lats2, alts2, out, nil)
	if out[0] != 0 {
		t.Errorf("self-distance: got %v, want 0", out[0])
	}
	// Chord across the equator: 2a = 12756274 m.
	if math.Abs(out[1]-12756274) > 1 {
		t.Errorf("antipodal on equator: got %v, want ~12756274", out[1])
	}
	// Chord from (a, 0, 0) to (0, 0, b): sqrt(a² + b²).
	a := WGS84Ellipsoid.A
	b := a * (1 - 1/WGS84Ellipsoid.FInv)
	wantChord := math.Sqrt(a*a + b*b)
	if math.Abs(out[2]-wantChord) > 1 {
		t.Errorf("equator→pole: got %v, want %v", out[2], wantChord)
	}
}

// TestDistance3DGeodesic_AltitudeSensitivity — 100m altitude
// difference at the same lat/lon should produce a 100m ECEF
// distance (Z-axis of ECEF is roughly aligned with the up vector
// at the pole; at the equator the outward direction is
// horizontal but the magnitude is still ≈ 100m ± sub-meter for
// low altitudes).
func TestDistance3DGeodesic_AltitudeSensitivity(t *testing.T) {
	lons1 := []float64{0, 0}
	lats1 := []float64{45, 45}
	alts1 := []float64{0, 1000}
	lons2 := []float64{0, 0}
	lats2 := []float64{45, 45}
	alts2 := []float64{100, 1100}
	out := make([]float64, 2)
	Distance3DGeodesicFromSlabs(lons1, lats1, alts1, lons2, lats2, alts2, out, nil)
	// Both pairs are same-position + 100m altitude → same result,
	// ≈ 100m regardless of the base altitude.
	for i, got := range out {
		if math.Abs(got-100) > 0.001 {
			t.Errorf("pair %d: got %v, want ~100", i, got)
		}
	}
}

// TestDistance3DProjectedFromSlabs_Trivial — Cartesian slab kernel.
func TestDistance3DProjectedFromSlabs_Trivial(t *testing.T) {
	xs1 := []float64{0, 1, 0}
	ys1 := []float64{0, 0, 0}
	zs1 := []float64{0, 0, 0}
	xs2 := []float64{3, 1, 0}
	ys2 := []float64{4, 0, 0}
	zs2 := []float64{0, 0, 5}
	out := make([]float64, 3)
	Distance3DProjectedFromSlabs(xs1, ys1, zs1, xs2, ys2, zs2, out)
	want := []float64{5, 0, 5}
	for i, w := range want {
		if math.Abs(out[i]-w) > 1e-12 {
			t.Errorf("[%d]: got %v, want %v", i, out[i], w)
		}
	}
}

// TestLineString3DGeodesicLength_MatchesSegmentSum — the line
// length kernel equals the sum of segment distances for a
// two-segment fixture.
func TestLineString3DGeodesicLength_MatchesSegmentSum(t *testing.T) {
	// Line: (0°E, 0°N, 0m) → (1°E, 0°N, 100m) → (2°E, 0°N, 200m).
	lons := []float64{0, 1, 2}
	lats := []float64{0, 0, 0}
	alts := []float64{0, 100, 200}
	got := LineString3DGeodesicLengthFromXYZ(lons, lats, alts, nil)
	// Compare against pair-wise segment distance sum.
	seg := make([]float64, 2)
	Distance3DGeodesicFromSlabs(
		lons[:2], lats[:2], alts[:2],
		lons[1:], lats[1:], alts[1:],
		seg, nil,
	)
	want := seg[0] + seg[1]
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("length = %v, want %v", got, want)
	}
}

// TestLineString3DProjectedLength — Cartesian, straight line.
func TestLineString3DProjectedLength(t *testing.T) {
	xs := []float64{0, 3, 3}
	ys := []float64{0, 4, 4}
	zs := []float64{0, 0, 12}
	got := LineString3DProjectedLengthFromXYZ(xs, ys, zs)
	// Seg 1: sqrt(9 + 16) = 5. Seg 2: sqrt(0 + 0 + 144) = 12.
	// Total = 17.
	if math.Abs(got-17) > 1e-12 {
		t.Errorf("got %v, want 17", got)
	}
}

// TestSampleGeodesic3D_Endpoints — first and last samples equal
// the endpoints.
func TestSampleGeodesic3D_Endpoints(t *testing.T) {
	outLons := make([]float64, 5)
	outLats := make([]float64, 5)
	outAlts := make([]float64, 5)
	SampleGeodesic3DFromSlabs(0, 0, 0, 90, 45, 500, 5, outLons, outLats, outAlts)
	if math.Abs(outLons[0]-0) > 1e-6 || math.Abs(outLats[0]-0) > 1e-6 || math.Abs(outAlts[0]-0) > 1e-6 {
		t.Errorf("start: got (%v, %v, %v), want (0, 0, 0)", outLons[0], outLats[0], outAlts[0])
	}
	if math.Abs(outLons[4]-90) > 1e-6 || math.Abs(outLats[4]-45) > 1e-6 || math.Abs(outAlts[4]-500) > 1e-6 {
		t.Errorf("end: got (%v, %v, %v), want (90, 45, 500)", outLons[4], outLats[4], outAlts[4])
	}
	// Altitude is linearly interpolated: sample 2 (t=0.5) should
	// be at 250m.
	if math.Abs(outAlts[2]-250) > 1e-6 {
		t.Errorf("mid altitude: got %v, want 250", outAlts[2])
	}
}

// TestPoint_Distance3D_Dispatch — the CRS-based dispatch. Projected
// stays Cartesian; geographic uses ECEF; the two disagree numerically
// on the same coordinate triple — verifies dispatch fires correctly.
func TestPoint_Distance3D_Dispatch(t *testing.T) {
	// Same (X, Y, Z) triple, different CRSes.
	a := Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: PseudoMercator}
	b := Point{X: 0, Y: 0, Z: 100, HasZ: true, CRSValue: PseudoMercator}
	gotProj, err := a.Distance3D(b, UnitMeters)
	if err != nil {
		t.Fatalf("projected: %v", err)
	}
	if math.Abs(gotProj-100) > 1e-12 {
		t.Errorf("projected: got %v, want 100", gotProj)
	}

	// Geographic — same numeric X/Y/Z but interpreted as (lon°,
	// lat°, alt m). At (0°E, 0°N) the two altitudes differ by
	// 100m; ECEF-Euclidean distance ≈ 100m.
	a.CRSValue = WGS84
	b.CRSValue = WGS84
	gotGeo, err := a.Distance3D(b, UnitMeters)
	if err != nil {
		t.Fatalf("geographic: %v", err)
	}
	if math.Abs(gotGeo-100) > 0.001 {
		t.Errorf("geographic: got %v, want ~100", gotGeo)
	}

	// CRS mismatch → ErrCRSMismatch.
	c := Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: WGS84}
	d := Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: PseudoMercator}
	if _, err := c.Distance3D(d, UnitMeters); err == nil {
		t.Error("CRS mismatch: expected error, got nil")
	}
}

// TestPoint_Force2D_ForceZ — coordinate promote/demote.
func TestPoint_Force2D_ForceZ(t *testing.T) {
	p := Point{X: 1, Y: 2, Z: 3, HasZ: true}
	q := p.Force2D()
	if q.HasZ || q.Z != 0 {
		t.Errorf("Force2D: HasZ=%v Z=%v, want false 0", q.HasZ, q.Z)
	}
	if q.X != 1 || q.Y != 2 {
		t.Errorf("Force2D: XY lost: %+v", q)
	}
	r := q.ForceZ(42)
	if !r.HasZ || r.Z != 42 {
		t.Errorf("ForceZ: HasZ=%v Z=%v, want true 42", r.HasZ, r.Z)
	}
}

// TestExtrudedPolygon_Contains3D — 2D PIP + Z-range band-pass.
func TestExtrudedPolygon_Contains3D(t *testing.T) {
	square := Polygon{
		Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}},
	}
	prism, err := NewExtrudedPolygon(square, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	// Inside footprint + inside Z: hit.
	if !prism.Contains3D(5, 5, 150) {
		t.Error("(5, 5, 150): expected inside")
	}
	// Inside footprint + below Z range: miss.
	if prism.Contains3D(5, 5, 50) {
		t.Error("(5, 5, 50): expected outside (below MinZ)")
	}
	// Inside footprint + above Z range: miss.
	if prism.Contains3D(5, 5, 300) {
		t.Error("(5, 5, 300): expected outside (above MaxZ)")
	}
	// Outside footprint + inside Z: miss.
	if prism.Contains3D(15, 15, 150) {
		t.Error("(15, 15, 150): expected outside (outside footprint)")
	}
	// On Z boundary (upper-inclusive): hit.
	if !prism.Contains3D(5, 5, 200) {
		t.Error("(5, 5, 200): expected inside (on upper Z boundary)")
	}
}

// TestPointsInPrismFromXYZ_MatchesScalar — SoA kernel agrees with
// scalar Contains3D on a random fixture.
func TestPointsInPrismFromXYZ_MatchesScalar(t *testing.T) {
	square := Polygon{
		Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}},
	}
	prism, _ := NewExtrudedPolygon(square, 100, 200)
	xs := []float64{5, 5, 5, 15, 0}
	ys := []float64{5, 5, 5, 15, 0}
	zs := []float64{50, 150, 300, 150, 100}
	out := make([]bool, 5)
	PointsInPrismFromXYZ(xs, ys, zs, prism, out)
	for i := range xs {
		want := prism.Contains3D(xs[i], ys[i], zs[i])
		if out[i] != want {
			t.Errorf("[%d] (%v,%v,%v): SoA=%v, scalar=%v", i, xs[i], ys[i], zs[i], out[i], want)
		}
	}
}

// TestBounds3D_Intersects — six-axis band-pass on axis-aligned
// bboxes.
func TestBounds3D_Intersects(t *testing.T) {
	a := Bounds3D{0, 0, 0, 10, 10, 10}
	b := Bounds3D{5, 5, 5, 15, 15, 15}    // overlaps
	c := Bounds3D{20, 0, 0, 30, 10, 10}   // disjoint X
	d := Bounds3D{0, 0, 100, 10, 10, 200} // disjoint Z
	if !a.Intersects(b) {
		t.Error("a ∩ b: expected true")
	}
	if a.Intersects(c) {
		t.Error("a ∩ c (disjoint X): expected false")
	}
	if a.Intersects(d) {
		t.Error("a ∩ d (disjoint Z): expected false")
	}
	if !EmptyBounds3D().Empty() {
		t.Error("EmptyBounds3D() not marked empty")
	}
	if a.Intersects(EmptyBounds3D()) {
		t.Error("intersects with empty should be false")
	}
}

// TestSphere_PointsIn_Projected — sphere containment in projected
// CRS is Cartesian.
func TestSphere_PointsIn_Projected(t *testing.T) {
	pt := Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: PseudoMercator}
	sphere, err := Buffer3DPoint(pt, 100)
	if err != nil {
		t.Fatal(err)
	}
	xs := []float64{50, 100, 101, 0}
	ys := []float64{50, 0, 0, 0}
	zs := []float64{50, 0, 0, 99.9}
	out := make([]bool, 4)
	PointsInSphereFromXYZ(xs, ys, zs, sphere, out, nil)
	want := []bool{
		math.Sqrt(3*50*50) <= 100, // sqrt(7500) ≈ 86.6 ≤ 100 → true
		true,                      // exactly on boundary
		false,                     // just outside
		true,                      // 99.9 < 100
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("[%d]: got %v, want %v", i, out[i], w)
		}
	}
}

// TestCapsule_PointsIn_Projected — capsule containment: within R
// of a line segment.
func TestCapsule_PointsIn_Projected(t *testing.T) {
	ls := LineString{
		Points: []Point{
			{X: 0, Y: 0, Z: 0, HasZ: true},
			{X: 10, Y: 0, Z: 0, HasZ: true},
		},
		HasZ:     true,
		CRSValue: PseudoMercator,
	}
	caps, err := Buffer3DLineString(ls, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 {
		t.Fatalf("caps: got %d, want 1", len(caps))
	}
	c := caps[0]
	xs := []float64{5, 5, -1.5, 11.5, 0}
	ys := []float64{0.5, 2, 0, 0, 0}
	zs := []float64{0, 0, 0, 0, 1.5}
	out := make([]bool, 5)
	PointsInCapsuleFromXYZ(xs, ys, zs, c, out)
	// Capsule is a swept sphere — points within R of the segment
	// (including the hemispherical caps at each endpoint). Kernel
	// uses clamped projection: t clamps to [0,1] so points past
	// an endpoint measure to that endpoint, not the extended line.
	want := []bool{
		true,  // 0.5 away from mid of segment
		false, // 2 > R=1
		false, // 1.5 past A → dist to A = 1.5 > R=1
		false, // 1.5 past B → dist to B = 1.5 > R=1
		false, // 1.5 above A → dist to A = 1.5 > R=1
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("[%d]: got %v, want %v", i, out[i], w)
		}
	}
}

// TestConvexHull3D_PrismApproximation — 2D-hull-of-XY wrapped in
// Z-range prism. On a coplanar-Z fixture the prism collapses to a
// flat slab.
func TestConvexHull3D_PrismApproximation(t *testing.T) {
	// Four corners of a unit square, all at z=100.
	xs := []float64{0, 1, 1, 0, 0.5}
	ys := []float64{0, 0, 1, 1, 0.5}
	zs := []float64{100, 100, 100, 100, 100}
	h := ConvexHull3DFromXYZ(xs, ys, zs, PseudoMercator)
	if h.Prism == nil {
		t.Fatal("Prism nil")
	}
	if h.Prism.MinZ != 100 || h.Prism.MaxZ != 100 {
		t.Errorf("Z range: got [%v, %v], want [100, 100]", h.Prism.MinZ, h.Prism.MaxZ)
	}
	// Centroid of the square must be contained; the corner interior
	// point at (0.5, 0.5) is inside the hull.
	if !h.Contains3D(0.5, 0.5, 100) {
		t.Error("(0.5, 0.5, 100) should be inside the hull")
	}
	// Outside the XY hull:
	if h.Contains3D(2, 2, 100) {
		t.Error("(2, 2, 100) should be outside the hull")
	}
	// Below Z range:
	if h.Contains3D(0.5, 0.5, 99) {
		t.Error("(0.5, 0.5, 99) should be outside (below Z range)")
	}
}

// TestConvexHull3D_ZRange — non-coplanar Z produces a prism with
// non-trivial Z extent.
func TestConvexHull3D_ZRange(t *testing.T) {
	xs := []float64{0, 1, 1, 0}
	ys := []float64{0, 0, 1, 1}
	zs := []float64{10, 20, 30, 15}
	h := ConvexHull3DFromXYZ(xs, ys, zs, PseudoMercator)
	if h.Prism == nil {
		t.Fatal("Prism nil")
	}
	if h.Prism.MinZ != 10 || h.Prism.MaxZ != 30 {
		t.Errorf("Z range: got [%v, %v], want [10, 30]", h.Prism.MinZ, h.Prism.MaxZ)
	}
}

// TestConvexHull3D_DegenerateInput — <3 unique points can't form a
// valid polygon ring. Fewer than 4 hull vertices (closed-ring form)
// must return an empty Hull3D sentinel, not a Prism holding a broken
// footprint.
func TestConvexHull3D_DegenerateInput(t *testing.T) {
	cases := []struct {
		name       string
		xs, ys, zs []float64
	}{
		{"empty", nil, nil, nil},
		{"single-point", []float64{0}, []float64{0}, []float64{0}},
		{"two-points", []float64{0, 1}, []float64{0, 0}, []float64{0, 0}},
		{"collinear", []float64{0, 1, 2, 3}, []float64{0, 0, 0, 0}, []float64{0, 0, 0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := ConvexHull3DFromXYZ(c.xs, c.ys, c.zs, PseudoMercator)
			if h.Prism != nil {
				t.Errorf("degenerate input produced Prism=%+v, want nil", h.Prism)
			}
		})
	}
}

// TestBuffer3DLineString_Degenerate — empty and single-vertex lines
// return an empty capsule slice with no error; 2D-only lines error.
func TestBuffer3DLineString_Degenerate(t *testing.T) {
	// Empty
	empty := LineString{HasZ: true, CRSValue: PseudoMercator}
	caps, err := Buffer3DLineString(empty, 5)
	if err != nil {
		t.Errorf("empty: unexpected error %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("empty: got %d capsules, want 0", len(caps))
	}
	// Single vertex
	single := LineString{
		Points:   []Point{{X: 0, Y: 0, Z: 0, HasZ: true}},
		HasZ:     true,
		CRSValue: PseudoMercator,
	}
	caps, err = Buffer3DLineString(single, 5)
	if err != nil {
		t.Errorf("single-vertex: unexpected error %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("single-vertex: got %d capsules, want 0", len(caps))
	}
	// 2D input → error
	twoDee := LineString{
		Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}},
		HasZ:   false, CRSValue: PseudoMercator,
	}
	if _, err := Buffer3DLineString(twoDee, 5); err == nil {
		t.Error("2D LineString: expected ErrTypeMismatch, got nil")
	}
	// Negative radius → error
	valid := LineString{
		Points:   []Point{{X: 0, Y: 0, Z: 0, HasZ: true}, {X: 10, Y: 0, Z: 0, HasZ: true}},
		HasZ:     true,
		CRSValue: PseudoMercator,
	}
	if _, err := Buffer3DLineString(valid, -1); err == nil {
		t.Error("negative radius: expected error, got nil")
	}
}

// TestPointsInAnyCapsuleFromXYZ_ORsSegments — multi-segment line
// buffer coverage. A point that's outside every individual capsule
// but in the union should be reported false; a point in any single
// capsule should be true.
func TestPointsInAnyCapsuleFromXYZ_ORsSegments(t *testing.T) {
	// Two-segment L-shape: (0,0,0) → (10,0,0) → (10,10,0). r=1.
	ls := LineString{
		Points: []Point{
			{X: 0, Y: 0, Z: 0, HasZ: true},
			{X: 10, Y: 0, Z: 0, HasZ: true},
			{X: 10, Y: 10, Z: 0, HasZ: true},
		},
		HasZ:     true,
		CRSValue: PseudoMercator,
	}
	caps, err := Buffer3DLineString(ls, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 2 {
		t.Fatalf("caps: got %d, want 2", len(caps))
	}
	xs := []float64{5, 10, 10, 5, 20}
	ys := []float64{0.5, 5, 0.5, 5, 0}
	zs := []float64{0, 0, 0, 0, 0}
	out := make([]bool, 5)
	PointsInAnyCapsuleFromXYZ(xs, ys, zs, caps, out)
	want := []bool{
		true,  // near cap 0 midpoint
		true,  // near cap 1 midpoint
		true,  // corner — inside both capsule caps
		false, // interior of the L, but 5 units from either segment
		false, // way past the endpoint of cap 0
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("[%d] (%v,%v,%v): got %v, want %v", i, xs[i], ys[i], zs[i], out[i], w)
		}
	}
	// Empty capsule slice → all false; zero rows → no writes past out.
	empty := make([]bool, 3)
	PointsInAnyCapsuleFromXYZ(xs[:3], ys[:3], zs[:3], nil, empty)
	for i, v := range empty {
		if v {
			t.Errorf("empty capsules: [%d] got true, want false", i)
		}
	}
}

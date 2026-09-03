package geometry

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// centroidsAlmostEqual is a NaN-aware Point comparison used by the
// centroid tests. Distinct from the package-level pointsAlmostEqual
// (clip_linestring_test.go) so NaN handling stays local to the
// centroid tests where the pathological single-point-ring polygon
// case is expected to produce NaN.
func centroidsAlmostEqual(a, b Point, tol float64) bool {
	xOK := (math.IsNaN(a.X) && math.IsNaN(b.X)) || math.Abs(a.X-b.X) <= tol
	yOK := (math.IsNaN(a.Y) && math.IsNaN(b.Y)) || math.Abs(a.Y-b.Y) <= tol
	return xOK && yOK
}

// TestCentroidFromWKB_MatchesGCentroid — SoA scanner produces the
// same centroid as ParseWKB(data).Centroid() for every type where
// the SoA formula is defined to match. See CentroidFromWKB
// docstring for MultiPolygon / GeometryCollection divergences —
// those are covered by a separate test.
func TestCentroidFromWKB_MatchesGCentroid(t *testing.T) {
	cases := []struct {
		name string
		g    Geometry
	}{
		{"Point", Point{X: 5, Y: -3}},
		{"PointZ", Point{X: 5, Y: -3, Z: 7, HasZ: true}},
		{"LineString_closed_square", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}},
		{"LineString_irregular", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 3, Y: 4}, {X: 10, Y: 4}, {X: 15, Y: 12},
		}}},
		{"LineString_single_point", LineString{Points: []Point{{X: 7, Y: 3}}}},
		{"LineString_all_coincident", LineString{Points: []Point{
			{X: 5, Y: 5}, {X: 5, Y: 5}, {X: 5, Y: 5},
		}}},
		{"Polygon_closed_square", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}}},
		{"Polygon_unclosed_square", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
		}}}},
		{"Polygon_with_hole", Polygon{Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 2, Y: 2}, {X: 4, Y: 2}, {X: 4, Y: 4}, {X: 2, Y: 4}, {X: 2, Y: 2}},
		}}},
		{"MultiPoint", MultiPoint{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
		}}},
		{"MultiPoint_single", MultiPoint{Points: []Point{{X: 42, Y: -7}}}},
		{"MultiLineString_two_lines", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 4, Y: 0}}},     // length 4, mid (2, 0)
			{Points: []Point{{X: 10, Y: 10}, {X: 10, Y: 20}}}, // length 10, mid (10, 15)
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := WKB(c.g)
			want := c.g.Centroid()
			// Bench-frame CRS on returned centroid: AoS sets CRSValue
			// from the source geometry; SoA returns unset. Compare
			// only X/Y — that's the contract.
			got, err := CentroidFromWKB(data)
			if err != nil {
				t.Fatalf("CentroidFromWKB: %v", err)
			}
			if !centroidsAlmostEqual(got, want, 1e-9) {
				t.Errorf("SoA centroid %+v, AoS centroid %+v", got, want)
			}
		})
	}
}

// TestCentroidFromWKB_MultiPolygon_UsesBBoxCenter — documents +
// verifies the intentional divergence from
// MultiPolygon.Centroid() (which uses geodesic area-weighting).
// The SoA fast path returns bbox-center; verify against a
// manually-computed bbox-center for the test case.
func TestCentroidFromWKB_MultiPolygon_UsesBBoxCenter(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{{
			{X: 10, Y: 10}, {X: 11, Y: 10}, {X: 11, Y: 11}, {X: 10, Y: 11}, {X: 10, Y: 10},
		}}},
	}}
	got, err := CentroidFromWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	// Combined bbox is (0, 0)-(11, 11), center (5.5, 5.5).
	want := Point{X: 5.5, Y: 5.5}
	if got != want {
		t.Errorf("bbox-center = %+v, want %+v", got, want)
	}
}

// TestCentroidFromWKB_GeometryCollection_MatchesBBoxCenter —
// GeometryCollection.Centroid IS bbox-center in the AoS
// implementation, so SoA should agree exactly on the (X, Y).
func TestCentroidFromWKB_GeometryCollection_MatchesBBoxCenter(t *testing.T) {
	gc := GeometryCollection{Geometries: []Geometry{
		Point{X: 0, Y: 0},
		LineString{Points: []Point{{X: 5, Y: 5}, {X: 10, Y: 10}}},
	}}
	got, err := CentroidFromWKB(WKB(gc))
	if err != nil {
		t.Fatal(err)
	}
	want := gc.Centroid() // AoS = bbox-center — should match exactly.
	if got.X != want.X || got.Y != want.Y {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestCentroidAndBoundsFromWKB_MatchesSeparately — the fused
// scanner produces the same centroid AND bounds as calling
// CentroidFromWKB and BoundsFromWKB separately.
func TestCentroidAndBoundsFromWKB_MatchesSeparately(t *testing.T) {
	cases := []Geometry{
		Point{X: 5, Y: -3},
		LineString{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}, {X: 10, Y: 4}}},
		Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}},
		MultiPoint{Points: []Point{{X: 1, Y: 2}, {X: 3, Y: 4}}},
		MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
			}}},
		}},
	}
	for _, g := range cases {
		data := WKB(g)
		wantC, err := CentroidFromWKB(data)
		if err != nil {
			t.Fatalf("%T CentroidFromWKB: %v", g, err)
		}
		wantB, err := BoundsFromWKB(data)
		if err != nil {
			t.Fatalf("%T BoundsFromWKB: %v", g, err)
		}
		gotC, gotB, err := CentroidAndBoundsFromWKB(data)
		if err != nil {
			t.Fatalf("%T CentroidAndBoundsFromWKB: %v", g, err)
		}
		if !centroidsAlmostEqual(gotC, wantC, 1e-9) {
			t.Errorf("%T: fused centroid %+v, standalone %+v", g, gotC, wantC)
		}
		if gotB != wantB {
			t.Errorf("%T: fused bounds %+v, standalone %+v", g, gotB, wantB)
		}
	}
}

// TestCentroidFromWKB_ShortInput — every entry point must reject
// truncated input without panicking.
func TestCentroidFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x01},
		{0x01, 0x01, 0x00, 0x00, 0x00},
		{0x01, 0x02, 0x00, 0x00, 0x00, 0x03, 0, 0},
	}
	for i, data := range cases {
		_, err := CentroidFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB or ErrInvalidByteOrder", i, err)
		}
	}
}

// TestCentroidFromWKB_ZeroAllocations — parity with BoundsFromWKB's
// zero-alloc contract. The whole point of the Slice 3 path is to
// eliminate per-row allocation in the SortByHilbert loop.
func TestCentroidFromWKB_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	pts := make([]Point, 100)
	for i := range pts {
		pts[i] = Point{X: rng.Float64(), Y: rng.Float64()}
	}
	data := WKB(LineString{Points: pts})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = CentroidFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("CentroidFromWKB: %v allocs/op, want 0", allocs)
	}
	allocs = testing.AllocsPerRun(100, func() {
		_, _, _ = CentroidAndBoundsFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("CentroidAndBoundsFromWKB: %v allocs/op, want 0", allocs)
	}
}

// TestCentroidFromWKB_BigEndian — LE and BE encodings of the same
// geometry produce the same centroid.
func TestCentroidFromWKB_BigEndian(t *testing.T) {
	pts := []Point{{X: 1.5, Y: 2.5}, {X: -3.25, Y: 7.125}, {X: 0.5, Y: -1.5}}
	// Build both LE and BE LineString WKBs by hand.
	var le, be []byte
	le = append(le, wkbNDR)
	le = binary.LittleEndian.AppendUint32(le, wkbLineString)
	le = binary.LittleEndian.AppendUint32(le, uint32(len(pts)))
	for _, p := range pts {
		le = binary.LittleEndian.AppendUint64(le, math.Float64bits(p.X))
		le = binary.LittleEndian.AppendUint64(le, math.Float64bits(p.Y))
	}
	be = append(be, wkbXDR)
	be = binary.BigEndian.AppendUint32(be, wkbLineString)
	be = binary.BigEndian.AppendUint32(be, uint32(len(pts)))
	for _, p := range pts {
		be = binary.BigEndian.AppendUint64(be, math.Float64bits(p.X))
		be = binary.BigEndian.AppendUint64(be, math.Float64bits(p.Y))
	}
	leC, err := CentroidFromWKB(le)
	if err != nil {
		t.Fatal(err)
	}
	beC, err := CentroidFromWKB(be)
	if err != nil {
		t.Fatal(err)
	}
	if !centroidsAlmostEqual(leC, beC, 1e-12) {
		t.Errorf("byte-order mismatch: LE=%+v BE=%+v", leC, beC)
	}
}

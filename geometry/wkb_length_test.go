package geometry

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// TestPlanarLengthFromWKB_MatchesAoS — the SoA scanner must match
// LineString.Length / MultiLineString.Length on a projected CRS
// (planar) across the full geometry-type matrix. Non-linear types
// return 0 matching geometry.Length's dispatch.
func TestPlanarLengthFromWKB_MatchesAoS(t *testing.T) {
	projected := CRS{EPSG: 3857, Projected: true}
	cases := []struct {
		name string
		g    Geometry
		want float64
	}{
		{"Point", Point{X: 5, Y: -3}, 0},
		{"LineString-2pts", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 3, Y: 4},
		}, CRSValue: projected}, 5},
		{"LineString-many", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 3, Y: 0}, {X: 3, Y: 4}, {X: 0, Y: 4},
		}, CRSValue: projected}, 3 + 4 + 3},
		{"LineString-degenerate-1pt", LineString{Points: []Point{
			{X: 5, Y: 5},
		}, CRSValue: projected}, 0},
		{"Polygon-nonlinear", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
		}}, CRSValue: projected}, 0},
		{"MultiPoint-nonlinear", MultiPoint{Points: []Point{
			{X: 1, Y: 2}, {X: 3, Y: 4},
		}, CRSValue: projected}, 0},
		{"MultiLineString", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}}},
			{Points: []Point{{X: 0, Y: 0}, {X: 5, Y: 12}}},
		}, CRSValue: projected}, 5 + 13},
		{"MultiPolygon-nonlinear", MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0}}}},
		}, CRSValue: projected}, 0},
		{"GeometryCollection-mixed", GeometryCollection{Geometries: []Geometry{
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}}, CRSValue: projected},
			Point{X: 100, Y: 100, CRSValue: projected},
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 5, Y: 12}}, CRSValue: projected},
		}, CRSValue: projected}, 5 + 13},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := WKB(c.g)
			got, err := PlanarLengthFromWKB(data)
			if err != nil {
				t.Fatalf("PlanarLengthFromWKB: %v", err)
			}
			if !almostEqualF(got, c.want, 1e-9) {
				t.Errorf("PlanarLengthFromWKB = %v, want %v", got, c.want)
			}
			// Cross-check against AoS Length on a projected CRS.
			wantAoS, err := Length(c.g, UnitMeters)
			if err != nil {
				t.Fatalf("Length: %v", err)
			}
			if !almostEqualF(got, wantAoS, 1e-9) {
				t.Errorf("SoA=%v differs from AoS Length=%v", got, wantAoS)
			}
		})
	}
}

// TestPlanarLengthFromWKB_RandomLineStrings — fuzz against
// LineString.Length on projected CRS across many random shapes.
func TestPlanarLengthFromWKB_RandomLineStrings(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	projected := CRS{EPSG: 3857, Projected: true}
	for iter := range 100 {
		n := 2 + rng.Intn(20)
		pts := make([]Point, n)
		for i := range pts {
			pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
		}
		ls := LineString{Points: pts, CRSValue: projected}
		data := WKB(ls)
		got, err := PlanarLengthFromWKB(data)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		want, err := ls.Length(UnitMeters)
		if err != nil {
			t.Fatalf("iter %d Length: %v", iter, err)
		}
		if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Errorf("iter %d: SoA=%v AoS=%v", iter, got, want)
		}
	}
}

// TestPlanarLengthFromWKB_ShortInput — malformed input returns
// ErrShortWKB / ErrInvalidByteOrder rather than panicking.
func TestPlanarLengthFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x01}, // just byte order
		{0x01, 0x02, 0x00, 0x00, 0x00, 0x03, 0, 0}, // LineString header, truncated count
	}
	for i, data := range cases {
		_, err := PlanarLengthFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB/ErrInvalidByteOrder", i, err)
		}
	}
}

// TestPlanarLengthFromWKB_RejectsNestedCollection — matches
// ParseWKB's rule (no nested GeometryCollections).
func TestPlanarLengthFromWKB_RejectsNestedCollection(t *testing.T) {
	var buf []byte
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 0)

	_, err := PlanarLengthFromWKB(buf)
	if !errors.Is(err, ErrUnsupportedWKB) {
		t.Errorf("got %v, want ErrUnsupportedWKB", err)
	}
}

// TestPlanarLengthFromWKB_ZeroAllocations — hot path must be
// alloc-free on well-formed input.
func TestPlanarLengthFromWKB_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	pts := make([]Point, 100)
	for i := range pts {
		pts[i] = Point{X: rng.Float64(), Y: rng.Float64()}
	}
	data := WKB(LineString{Points: pts})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = PlanarLengthFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("%v allocs/op, want 0", allocs)
	}
}

func almostEqualF(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

package geometry

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// TestPlanarAreaFromWKB_MatchesAoS — the SoA scanner must match
// Polygon.Area / MultiPolygon.Area on a projected CRS (planar).
// Non-areal types return 0 matching geometry.Area's dispatch.
func TestPlanarAreaFromWKB_MatchesAoS(t *testing.T) {
	projected := CRS{EPSG: 3857, Projected: true}
	cases := []struct {
		name string
		g    Geometry
		want float64
	}{
		{"Point", Point{X: 5, Y: -3}, 0},
		{"LineString-nonareal", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10},
		}}, 0},
		{"MultiPoint-nonareal", MultiPoint{Points: []Point{
			{X: 1, Y: 2}, {X: 3, Y: 4},
		}}, 0},
		{"MultiLineString-nonareal", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		}}, 0},
		{"Polygon-square", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}, CRSValue: projected}, 100},
		{"Polygon-with-hole", Polygon{Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 2, Y: 2}, {X: 4, Y: 2}, {X: 4, Y: 4}, {X: 2, Y: 4}, {X: 2, Y: 2}},
		}, CRSValue: projected}, 100 - 4},
		{"Polygon-unclosed", Polygon{Rings: [][]Point{{
			// Not closed — scanner should virtual-close.
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
		}}, CRSValue: projected}, 100},
		{"MultiPolygon", MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
			}}},
			{Rings: [][]Point{{
				{X: 20, Y: 0}, {X: 25, Y: 0}, {X: 25, Y: 5}, {X: 20, Y: 5}, {X: 20, Y: 0},
			}}},
		}, CRSValue: projected}, 100 + 25},
		{"GeometryCollection-mixed", GeometryCollection{Geometries: []Geometry{
			Polygon{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
			}}, CRSValue: projected},
			Point{X: 100, Y: 100, CRSValue: projected},
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}}, CRSValue: projected},
		}, CRSValue: projected}, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := WKB(c.g)
			got, err := PlanarAreaFromWKB(data)
			if err != nil {
				t.Fatalf("PlanarAreaFromWKB: %v", err)
			}
			if !almostEqualF(got, c.want, 1e-9) {
				t.Errorf("PlanarAreaFromWKB = %v, want %v", got, c.want)
			}
			wantAoS, err := Area(c.g, UnitMeters)
			if err != nil {
				t.Fatalf("Area: %v", err)
			}
			if !almostEqualF(got, wantAoS, 1e-9) {
				t.Errorf("SoA=%v differs from AoS Area=%v", got, wantAoS)
			}
		})
	}
}

// TestPlanarAreaFromWKB_RandomPolygons — fuzz against Polygon.Area
// on projected CRS. Uses convex random polygons to sidestep
// self-intersection edge cases.
func TestPlanarAreaFromWKB_RandomPolygons(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	projected := CRS{EPSG: 3857, Projected: true}
	for iter := range 50 {
		// n points on a circle, guaranteed convex + non-self-intersecting.
		n := 3 + rng.Intn(12)
		cx := rng.Float64() * 100
		cy := rng.Float64() * 100
		r := 1 + rng.Float64()*50
		pts := make([]Point, n+1)
		for i := range n {
			θ := float64(i) * 2 * math.Pi / float64(n)
			pts[i] = Point{X: cx + r*math.Cos(θ), Y: cy + r*math.Sin(θ)}
		}
		pts[n] = pts[0] // close
		p := Polygon{Rings: [][]Point{pts}, CRSValue: projected}
		data := WKB(p)
		got, err := PlanarAreaFromWKB(data)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		want, err := p.Area(UnitMeters)
		if err != nil {
			t.Fatalf("iter %d Area: %v", iter, err)
		}
		if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Errorf("iter %d: SoA=%v AoS=%v", iter, got, want)
		}
	}
}

// TestPlanarAreaFromWKB_ShortInput — malformed input returns
// ErrShortWKB / ErrInvalidByteOrder rather than panicking.
func TestPlanarAreaFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x01}, // just byte order
		{0x01, 0x03, 0x00, 0x00, 0x00, 0x01, 0, 0}, // Polygon header, truncated ring count
	}
	for i, data := range cases {
		_, err := PlanarAreaFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB/ErrInvalidByteOrder", i, err)
		}
	}
}

// TestPlanarAreaFromWKB_RejectsNestedCollection — matches
// ParseWKB's rule.
func TestPlanarAreaFromWKB_RejectsNestedCollection(t *testing.T) {
	var buf []byte
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 0)

	_, err := PlanarAreaFromWKB(buf)
	if !errors.Is(err, ErrUnsupportedWKB) {
		t.Errorf("got %v, want ErrUnsupportedWKB", err)
	}
}

// TestPlanarAreaFromWKB_ZeroAllocations — hot path must be
// alloc-free on well-formed input.
func TestPlanarAreaFromWKB_ZeroAllocations(t *testing.T) {
	n := 100
	pts := make([]Point, n+1)
	for i := range n {
		θ := float64(i) * 2 * math.Pi / float64(n)
		pts[i] = Point{X: 10 * math.Cos(θ), Y: 10 * math.Sin(θ)}
	}
	pts[n] = pts[0]
	data := WKB(Polygon{Rings: [][]Point{pts}})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = PlanarAreaFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("%v allocs/op, want 0", allocs)
	}
}

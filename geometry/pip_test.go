package geometry

import (
	"errors"
	"math/rand"
	"testing"
)

// TestPIPRingFromXY_SquareAndConcave — the SoA ring kernel
// produces the same in/out answer as the AoS pointInRing on a
// battery of query points against a square and a concave ring.
func TestPIPRingFromXY_SquareAndConcave(t *testing.T) {
	// Closed unit square.
	square := []Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}
	// L-shape (concave). Outside "notch" at (7, 7).
	//   (0,10)          (10,10)
	//     +--------------+
	//     |              |
	//     |    +---+ (10,7)
	//     |    |   |
	//     |    +---+ (10,3)
	//     |              |
	//     +--------------+
	//   (0,0)            (10,0)
	// Rendered as a single ring by walking counterclockwise:
	lshape := []Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
		{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}
	cases := []struct {
		name    string
		ring    []Point
		queries []struct {
			tx, ty float64
			want   bool
		}
	}{
		{"square", square, []struct {
			tx, ty float64
			want   bool
		}{
			{5, 5, true},   // interior
			{-1, 5, false}, // left of left edge
			{11, 5, false}, // right of right edge
			{5, -1, false}, // below
			{5, 11, false}, // above
			{0.5, 0.5, true},
			{9.5, 9.5, true},
		}},
		{"L-shape", lshape, []struct {
			tx, ty float64
			want   bool
		}{
			{2, 5, true},   // inside the left column
			{2, 2, true},   // bottom-left corner region
			{2, 8, true},   // top-left corner region
			{7, 5, false},  // in the notch — outside
			{7, 2, true},   // below the notch — inside
			{7, 8, true},   // above the notch — inside
			{-1, 5, false}, // far outside
			{15, 5, false}, // far outside
		}},
	}
	for _, c := range cases {
		xs, ys := ringToXY(c.ring)
		for _, q := range c.queries {
			aos := pointInRing(Point{X: q.tx, Y: q.ty}, c.ring)
			soa := PIPRingFromXY(xs, ys, q.tx, q.ty)
			if aos != q.want {
				t.Errorf("%s @ (%v, %v): AoS gave %v, expected %v (test bug)",
					c.name, q.tx, q.ty, aos, q.want)
			}
			if soa != q.want {
				t.Errorf("%s @ (%v, %v): SoA gave %v, want %v", c.name, q.tx, q.ty, soa, q.want)
			}
		}
	}
}

// TestPIPRingFromXY_UnclosedRing — the SoA kernel must handle
// rings that don't have their first point repeated at the end.
// Matches pointInRing's closedRing-based handling.
func TestPIPRingFromXY_UnclosedRing(t *testing.T) {
	unclosed := []Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
	}
	xs, ys := ringToXY(unclosed)
	if !PIPRingFromXY(xs, ys, 5, 5) {
		t.Error("interior point should be inside unclosed square")
	}
	if PIPRingFromXY(xs, ys, 15, 5) {
		t.Error("exterior point should be outside unclosed square")
	}
}

// TestPIPPolygonFromRings_WithHole — polygon with an interior
// hole. Points inside the hole must NOT be contained.
func TestPIPPolygonFromRings_WithHole(t *testing.T) {
	p := Polygon{
		Rings: [][]Point{
			// Exterior: 10x10 square.
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			// Hole: 2x2 square at (4,4)-(6,6).
			{{X: 4, Y: 4}, {X: 6, Y: 4}, {X: 6, Y: 6}, {X: 4, Y: 6}, {X: 4, Y: 4}},
		},
	}
	rings := p.RingViews()
	cases := []struct {
		tx, ty float64
		want   bool
	}{
		{1, 1, true},      // inside exterior, outside hole
		{5, 5, false},     // inside hole
		{4.5, 4.5, false}, // just inside hole
		{15, 5, false},    // outside exterior entirely
	}
	for _, c := range cases {
		got := PIPPolygonFromRings(rings, c.tx, c.ty)
		aos := p.Contains(Point{X: c.tx, Y: c.ty})
		if got != c.want {
			t.Errorf("SoA @ (%v, %v) = %v, want %v", c.tx, c.ty, got, c.want)
		}
		if aos != c.want {
			t.Errorf("AoS @ (%v, %v) = %v, want %v (fixture check)", c.tx, c.ty, aos, c.want)
		}
	}
}

// TestPIPFromWKB_MatchesPolygonContains — the WKB scanner produces
// the same in/out answer as Polygon.Contains for the same input
// across a fuzz of query points.
func TestPIPFromWKB_MatchesPolygonContains(t *testing.T) {
	// Concave polygon shaped like an L.
	p := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
		{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}}}
	data := WKB(p)

	rng := rand.New(rand.NewSource(42))
	for range 200 {
		tx := rng.Float64()*14 - 2 // -2..12
		ty := rng.Float64()*14 - 2
		want := p.Contains(Point{X: tx, Y: ty})
		got, err := PIPFromWKB(data, tx, ty)
		if err != nil {
			t.Fatalf("PIPFromWKB: %v", err)
		}
		if got != want {
			t.Errorf("@ (%v, %v): WKB scan = %v, AoS = %v", tx, ty, got, want)
		}
	}
}

// TestPIPFromWKB_MultiPolygon — matches "any polygon contains"
// semantics across a MultiPolygon of two disjoint squares.
func TestPIPFromWKB_MultiPolygon(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{{
			{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10},
		}}},
	}}
	data := WKB(m)
	cases := []struct {
		tx, ty float64
		want   bool
	}{
		{2, 2, true},   // inside first
		{12, 12, true}, // inside second
		{7, 7, false},  // between the two
		{-1, 2, false}, // outside first, on the left
	}
	for _, c := range cases {
		got, err := PIPFromWKB(data, c.tx, c.ty)
		if err != nil {
			t.Fatalf("@ (%v, %v): %v", c.tx, c.ty, err)
		}
		if got != c.want {
			t.Errorf("@ (%v, %v): got %v, want %v", c.tx, c.ty, got, c.want)
		}
	}
}

// TestPIPFromWKB_NonPolygonReturnsFalse — Point / LineString / etc.
// return (false, nil). Matches the AoS shape where Polygon.Contains
// isn't defined on those types.
func TestPIPFromWKB_NonPolygonReturnsFalse(t *testing.T) {
	cases := []Geometry{
		Point{X: 5, Y: 5},
		LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 10}}},
		MultiPoint{Points: []Point{{X: 1, Y: 1}, {X: 2, Y: 2}}},
	}
	for _, g := range cases {
		got, err := PIPFromWKB(WKB(g), 5, 5)
		if err != nil {
			t.Fatalf("%T: %v", g, err)
		}
		if got {
			t.Errorf("%T should return false", g)
		}
	}
}

// TestPIPFromWKB_ShortInput — malformed inputs error cleanly.
func TestPIPFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x01},
		{0x01, 0x03, 0x00, 0x00, 0x00}, // Polygon header, no ring count
		{0x01, 0x03, 0x00, 0x00, 0x00, 0x01, 0, 0, 0}, // Polygon header + 1 ring, no ring size
	}
	for i, data := range cases {
		_, err := PIPFromWKB(data, 5, 5)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB or ErrInvalidByteOrder", i, err)
		}
	}
}

// TestPIPFromWKB_ZeroAllocations — Slice 4's fast path must be
// zero-alloc, matching Slice 2's BoundsFromWKB and Slice 3's
// CentroidFromWKB.
func TestPIPFromWKB_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pts := make([]Point, 100)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}
	// Close the ring.
	pts = append(pts, pts[0])
	data := WKB(Polygon{Rings: [][]Point{pts}})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = PIPFromWKB(data, 500, 500)
	})
	if allocs != 0 {
		t.Errorf("PIPFromWKB: %v allocs/op, want 0", allocs)
	}
}

// ringToXY copies a ring's coord fields into parallel Xs / Ys
// slices for the SoA kernel tests.
func ringToXY(r []Point) (xs, ys []float64) {
	xs = make([]float64, len(r))
	ys = make([]float64, len(r))
	for i, p := range r {
		xs[i] = p.X
		ys[i] = p.Y
	}
	return xs, ys
}

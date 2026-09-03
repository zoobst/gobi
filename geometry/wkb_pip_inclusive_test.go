package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// TestPIPInclusiveFromWKB_MatchesAoS — the boundary-inclusive
// scanner must match AoS pointInPolygon exactly for interior,
// exterior, and on-boundary query points.
func TestPIPInclusiveFromWKB_MatchesAoS(t *testing.T) {
	// L-shape polygon with a hole.
	poly := Polygon{Rings: [][]Point{
		{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
			{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
		{{X: 1, Y: 1}, {X: 2, Y: 1}, {X: 2, Y: 2}, {X: 1, Y: 2}, {X: 1, Y: 1}},
	}}
	data := WKB(poly)

	// Named query points covering interior / exterior / on-boundary /
	// on-hole-boundary / inside-hole cases.
	cases := []struct {
		name string
		x, y float64
		want bool
	}{
		{"interior-1", 2, 5, true},
		{"interior-2", 8, 8, true},
		{"exterior", -1, -1, false},
		{"on-exterior-edge", 5, 0, true},
		{"on-exterior-vertex", 10, 3, true},
		{"in-hole-interior", 1.5, 1.5, false},
		{"on-hole-edge", 1.5, 1, true},
		{"on-hole-vertex", 2, 2, true},
		{"in-concave-notch", 7, 5, false}, // inside L's notch
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PIPInclusiveFromWKB(data, c.x, c.y)
			if err != nil {
				t.Fatal(err)
			}
			want := pointInPolygon(Point{X: c.x, Y: c.y}, poly)
			if got != want || got != c.want {
				t.Errorf("(%v,%v): SoA=%v AoS=%v named-want=%v",
					c.x, c.y, got, want, c.want)
			}
		})
	}
}

// TestPIPInclusiveFromWKB_MultiPolygon — inclusive check with
// MultiPolygon input.
func TestPIPInclusiveFromWKB_MultiPolygon(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{{
			{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10},
		}}},
	}}
	data := WKB(m)

	cases := []struct{ x, y float64 }{
		{2, 2}, {12, 12}, {7, 7}, // interior, interior, exterior
		{0, 0}, {5, 5}, {15, 10}, // boundary points
		{20, 20}, {-1, -1}, // far exterior
	}
	for _, c := range cases {
		got, err := PIPInclusiveFromWKB(data, c.x, c.y)
		if err != nil {
			t.Fatal(err)
		}
		want := false
		for _, p := range m.Polygons {
			if pointInPolygon(Point{X: c.x, Y: c.y}, p) {
				want = true
				break
			}
		}
		if got != want {
			t.Errorf("(%v,%v): SoA=%v AoS=%v", c.x, c.y, got, want)
		}
	}
}

// TestPIPInclusiveFromWKB_RandomFuzz — random query points against
// the L-shape polygon; SoA must match AoS bit-for-bit.
func TestPIPInclusiveFromWKB_RandomFuzz(t *testing.T) {
	poly := Polygon{Rings: [][]Point{
		{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
			{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
	}}
	data := WKB(poly)
	rng := rand.New(rand.NewSource(9))
	for iter := range 500 {
		x := rng.Float64()*14 - 2
		y := rng.Float64()*14 - 2
		got, err := PIPInclusiveFromWKB(data, x, y)
		if err != nil {
			t.Fatal(err)
		}
		want := pointInPolygon(Point{X: x, Y: y}, poly)
		if got != want {
			t.Errorf("iter %d @ (%v,%v): SoA=%v AoS=%v", iter, x, y, got, want)
		}
	}
}

// TestPIPInclusiveFromWKB_NonPolygonReturnsFalse — non-polygon
// types return false (matches PIPFromWKB shape).
func TestPIPInclusiveFromWKB_NonPolygonReturnsFalse(t *testing.T) {
	ls := LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 10}}}
	data := WKB(ls)
	got, err := PIPInclusiveFromWKB(data, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Errorf("LineString: got true, want false")
	}
}

// TestPIPInclusiveFromWKB_ZeroAllocations — hot path must be
// alloc-free.
func TestPIPInclusiveFromWKB_ZeroAllocations(t *testing.T) {
	poly := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}}}
	data := WKB(poly)
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = PIPInclusiveFromWKB(data, 5, 5)
	})
	if allocs != 0 {
		t.Errorf("%v allocs/op, want 0", allocs)
	}
}

var _ = math.Pi // keep import stable

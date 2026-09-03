package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// TestPlanarMinDistanceFromWKB_MatchesAoS — the WKB-direct fast
// path must match planarMinDistance across every non-intersecting
// pair shape.
func TestPlanarMinDistanceFromWKB_MatchesAoS(t *testing.T) {
	cases := []struct {
		name string
		a, b Geometry
	}{
		{
			"pt-pt", Point{X: 0, Y: 0}, Point{X: 3, Y: 4},
		},
		{
			"pt-line",
			Point{X: 0, Y: 5},
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 0}}},
		},
		{
			"line-line",
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 0}}},
			LineString{Points: []Point{{X: 0, Y: 5}, {X: 10, Y: 5}}},
		},
		{
			"poly-poly",
			Polygon{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
			}}},
			Polygon{Rings: [][]Point{{
				{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10},
			}}},
		},
		{
			"multi-line-pt",
			MultiLineString{Lines: []LineString{
				{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 0}}},
				{Points: []Point{{X: 5, Y: 5}, {X: 6, Y: 6}}},
			}},
			Point{X: 100, Y: 100},
		},
		{
			"multi-poly-poly",
			MultiPolygon{Polygons: []Polygon{
				{Rings: [][]Point{{
					{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
				}}},
				{Rings: [][]Point{{
					{X: 5, Y: 5}, {X: 6, Y: 5}, {X: 6, Y: 6}, {X: 5, Y: 6}, {X: 5, Y: 5},
				}}},
			}},
			Polygon{Rings: [][]Point{{
				{X: 10, Y: 10}, {X: 11, Y: 10}, {X: 11, Y: 11}, {X: 10, Y: 11}, {X: 10, Y: 10},
			}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PlanarMinDistanceFromWKB(WKB(c.a), WKB(c.b))
			if err != nil {
				t.Fatalf("PlanarMinDistanceFromWKB: %v", err)
			}
			want := planarMinDistance(c.a, c.b)
			if math.Abs(got-want) > 1e-10 {
				t.Errorf("WKB=%v AoS=%v", got, want)
			}
		})
	}
}

// TestPlanarMinDistanceFromWKB_RandomPolyPairs — fuzz.
func TestPlanarMinDistanceFromWKB_RandomPolyPairs(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for iter := range 30 {
		a := buildRandomTriangle(rng, 0, 0)
		b := buildRandomTriangle(rng, 100, 100)
		got, err := PlanarMinDistanceFromWKB(WKB(a), WKB(b))
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		want := planarMinDistance(a, b)
		if math.Abs(got-want) > 1e-8 {
			t.Errorf("iter %d: WKB=%v AoS=%v", iter, got, want)
		}
	}
}

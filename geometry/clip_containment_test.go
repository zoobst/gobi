package geometry

import (
	"math"
	"testing"
)

// TestClip_ContainmentFastPath_MatchesSweep — the Slice-18
// containment fast paths must produce the same result as the
// full sweep would across every shape configuration:
//
//   - convex clipper wraps concave subject with holes
//   - convex subject wraps concave clipper
//   - convex-vs-convex (still hits SH after containment check)
//   - not-fully-contained (falls through to SH / sweep as before)
func TestClip_ContainmentFastPath_MatchesSweep(t *testing.T) {
	// Big convex disc.
	disc := regularPolygon(0, 0, 100, 32)
	// Concave L-shape polygon fully inside the disc, with a hole.
	lShape := Polygon{Rings: [][]Point{
		{{X: -20, Y: -20}, {X: 20, Y: -20}, {X: 20, Y: -10},
			{X: 0, Y: -10}, {X: 0, Y: 20}, {X: -20, Y: 20}, {X: -20, Y: -20}},
		{{X: -10, Y: -15}, {X: -5, Y: -15}, {X: -5, Y: -10},
			{X: -10, Y: -10}, {X: -10, Y: -15}},
	}}

	cases := []struct {
		name string
		a, b Geometry
	}{
		{"concave-L-inside-disc", lShape, disc},
		{"disc-wraps-concave-L", disc, lShape},
		{"convex-cell-inside-disc", unitSquare(-5, -5, 10), disc},
		{"disc-wraps-cell", disc, unitSquare(-5, -5, 10)},
		// Not fully contained — should fall through.
		{"partial-overlap", regularPolygon(50, 0, 30, 16), disc},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Clip(c.a, c.b)
			if err != nil {
				t.Fatal(err)
			}
			gotArea := polyOrMultiArea(got)
			// Oracle: sweep-only path. Wrap one side in a MultiPolygon
			// to bypass every Polygon×Polygon fast path in Boolean() —
			// forces the sweep.
			wantSweep, err := Clip(c.a, wrapMulti(c.b))
			if err != nil {
				t.Fatal(err)
			}
			wantArea := polyOrMultiArea(wantSweep)
			if math.Abs(gotArea-wantArea) > 1e-6*math.Max(1, math.Abs(wantArea)) {
				t.Errorf("area mismatch: fast=%v sweep=%v", gotArea, wantArea)
			}
		})
	}
}

func polyOrMultiArea(g Geometry) float64 {
	switch v := g.(type) {
	case Polygon:
		if len(v.Rings) == 0 {
			return 0
		}
		total := math.Abs(planarRingArea(v.Rings[0]))
		for _, h := range v.Rings[1:] {
			total -= math.Abs(planarRingArea(h))
		}
		return total
	case MultiPolygon:
		var total float64
		for _, p := range v.Polygons {
			total += polyOrMultiArea(p)
		}
		return total
	}
	return 0
}

func wrapMulti(g Geometry) Geometry {
	switch v := g.(type) {
	case Polygon:
		return MultiPolygon{Polygons: []Polygon{v}, CRSValue: v.CRSValue}
	}
	return g
}

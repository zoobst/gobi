package geometry

import (
	"math"
	"testing"
)

// TestUnionContainment_MatchesSweep — Slice 20a union containment
// fast path. When one operand is a convex polygon containing the
// other, union = the convex one.
func TestUnionContainment_MatchesSweep(t *testing.T) {
	convexDisc := regularPolygon(0, 0, 100, 32)
	innerL := Polygon{Rings: [][]Point{
		{{X: -20, Y: -20}, {X: 20, Y: -20}, {X: 20, Y: -10},
			{X: 0, Y: -10}, {X: 0, Y: 20}, {X: -20, Y: 20}, {X: -20, Y: -20}},
	}}
	innerCell := unitSquare(-5, -5, 10)

	cases := []struct {
		name string
		a, b Geometry
	}{
		{"disc-covers-concave-L", convexDisc, innerL},
		{"concave-L-covered-by-disc", innerL, convexDisc},
		{"disc-covers-cell", convexDisc, innerCell},
		{"cell-covered-by-disc", innerCell, convexDisc},
		// Not fully contained → fall through to sweep.
		{"partial-overlap", regularPolygon(50, 0, 30, 16), convexDisc},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Union(c.a, c.b)
			if err != nil {
				t.Fatal(err)
			}
			// Sweep-only oracle.
			want, err := Union(c.a, wrapMulti(c.b))
			if err != nil {
				t.Fatal(err)
			}
			gotArea := polyOrMultiArea(got)
			wantArea := polyOrMultiArea(want)
			if math.Abs(gotArea-wantArea) > 1e-6*math.Max(1, math.Abs(wantArea)) {
				t.Errorf("area mismatch: fast=%v sweep=%v", gotArea, wantArea)
			}
		})
	}
}

// TestDifferenceContainment_MatchesSweep — Slice 20b difference
// containment: a ⊆ convex b → a − b = empty.
func TestDifferenceContainment_MatchesSweep(t *testing.T) {
	convexDisc := regularPolygon(0, 0, 100, 32)
	innerL := Polygon{Rings: [][]Point{
		{{X: -20, Y: -20}, {X: 20, Y: -20}, {X: 20, Y: -10},
			{X: 0, Y: -10}, {X: 0, Y: 20}, {X: -20, Y: 20}, {X: -20, Y: -20}},
	}}
	innerCell := unitSquare(-5, -5, 10)

	cases := []struct {
		name string
		a, b Geometry
	}{
		{"concave-L-minus-covering-disc", innerL, convexDisc}, // → empty
		{"cell-minus-covering-disc", innerCell, convexDisc},   // → empty
		// Not fully contained → fall through to sweep.
		{"disc-minus-partial-overlap", convexDisc, regularPolygon(50, 0, 30, 16)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Difference(c.a, c.b)
			if err != nil {
				t.Fatal(err)
			}
			want, err := Difference(c.a, wrapMulti(c.b))
			if err != nil {
				t.Fatal(err)
			}
			gotArea := polyOrMultiArea(got)
			wantArea := polyOrMultiArea(want)
			if math.Abs(gotArea-wantArea) > 1e-6*math.Max(1, math.Abs(wantArea)) {
				t.Errorf("area mismatch: fast=%v sweep=%v", gotArea, wantArea)
			}
		})
	}
}

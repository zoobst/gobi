package geometry

import (
	"math"
	"math/rand"
	"testing"
)

// TestRelaxedSH_MatchesSweep_LShape — the canonical Slice-19
// shape: convex clipper AOI cutting through a concave L-shape
// subject. Verifies both directions (AOI clips L, L clips AOI)
// and edge cases (clipper misses concavity vs clipper straddles
// concavity).
func TestRelaxedSH_MatchesSweep_LShape(t *testing.T) {
	// L-shape (concave, single ring, no holes).
	lShape := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 4},
		{X: 4, Y: 4}, {X: 4, Y: 10}, {X: 0, Y: 10},
		{X: 0, Y: 0},
	}}}

	cases := []struct {
		name string
		aoi  Polygon
	}{
		// Convex AOI clipping the horizontal arm of the L. Enters
		// through the left edge, exits through the top of the arm.
		// Intersection = single trapezoid piece.
		{"AOI-in-horiz-arm", unitSquare(1, 1, 5)},
		// Convex AOI on the vertical arm. Similar shape.
		{"AOI-in-vert-arm", unitSquare(1, 5, 3)},
		// Convex AOI crossing the concavity (spans both arms —
		// intersection has 2 components, must fall back to sweep).
		{"AOI-crosses-concavity", unitSquare(2, 3, 5)},
		// Convex AOI fully inside L → containment fast path.
		{"AOI-fully-inside-L", unitSquare(1, 1, 2)},
		// Convex AOI fully covering L → containment fast path.
		{"AOI-covers-L", unitSquare(-5, -5, 20)},
		// Convex AOI disjoint from L.
		{"AOI-disjoint", unitSquare(20, 20, 5)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Fast path result.
			got, err := Clip(lShape, c.aoi)
			if err != nil {
				t.Fatal(err)
			}
			// Sweep-only oracle: wrap AOI in MultiPolygon to bypass
			// every Polygon×Polygon fast path.
			want, err := Clip(lShape, wrapMulti(c.aoi))
			if err != nil {
				t.Fatal(err)
			}
			gotArea := polyOrMultiArea(got)
			wantArea := polyOrMultiArea(want)
			if math.Abs(gotArea-wantArea) > 1e-6*math.Max(1, math.Abs(wantArea)) {
				t.Errorf("area mismatch: fast=%v sweep=%v (got: %+v)",
					gotArea, wantArea, got)
			}
		})
	}
}

// TestRelaxedSH_MatchesSweep_RandomConcave — fuzz random concave
// subjects clipped by convex AOIs. Verifies the transition-count
// gate correctly identifies safe vs unsafe cases across many
// shapes.
func TestRelaxedSH_MatchesSweep_RandomConcave(t *testing.T) {
	rng := rand.New(rand.NewSource(19))
	for iter := range 100 {
		// Random star-shape subject (guaranteed concave when
		// inner/outer radii differ).
		nSpokes := 4 + rng.Intn(6)
		subject := starPolygon(0, 0, 10, 3, nSpokes)
		// Random convex AOI within the subject's bbox.
		cx := rng.Float64()*14 - 7
		cy := rng.Float64()*14 - 7
		size := 2 + rng.Float64()*8
		aoi := unitSquare(cx, cy, size)

		got, err := Clip(subject, aoi)
		if err != nil {
			t.Fatalf("iter %d: fast Clip: %v", iter, err)
		}
		want, err := Clip(subject, wrapMulti(aoi))
		if err != nil {
			t.Fatalf("iter %d: sweep Clip: %v", iter, err)
		}
		gotArea := polyOrMultiArea(got)
		wantArea := polyOrMultiArea(want)
		if math.Abs(gotArea-wantArea) > 1e-4*math.Max(1, math.Abs(wantArea)) {
			t.Errorf("iter %d: area mismatch: fast=%v sweep=%v",
				iter, gotArea, wantArea)
		}
	}
}

// starPolygon returns a star-shape polygon with `spokes` alternating
// outer/inner vertices. Guaranteed concave when innerR < outerR.
func starPolygon(cx, cy, outerR, innerR float64, spokes int) Polygon {
	n := spokes * 2
	pts := make([]Point, n+1)
	for i := range n {
		theta := 2 * math.Pi * float64(i) / float64(n)
		r := outerR
		if i%2 == 1 {
			r = innerR
		}
		pts[i] = Point{X: cx + r*math.Cos(theta), Y: cy + r*math.Sin(theta)}
	}
	pts[n] = pts[0]
	return Polygon{Rings: [][]Point{pts}}
}

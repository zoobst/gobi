package geometry

import (
	"math"
	"math/rand"
	"testing"
)

func TestIsConvex(t *testing.T) {
	cases := []struct {
		name string
		poly Polygon
		want bool
	}{
		{
			name: "square",
			poly: SimplePolygon([]Point{
				pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0),
			}, CRS{}),
			want: true,
		},
		{
			name: "hexagon",
			poly: regularPolygon(0, 0, 10, 6),
			want: true,
		},
		{
			name: "L-shape (concave)",
			poly: SimplePolygon([]Point{
				pt(0, 0), pt(20, 0), pt(20, 10), pt(10, 10),
				pt(10, 20), pt(0, 20), pt(0, 0),
			}, CRS{}),
			want: false,
		},
		{
			name: "polygon with hole",
			poly: Polygon{Rings: [][]Point{
				{pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0)},
				{pt(3, 3), pt(3, 7), pt(7, 7), pt(7, 3), pt(3, 3)},
			}},
			want: false,
		},
		{
			name: "triangle",
			poly: SimplePolygon([]Point{
				pt(0, 0), pt(10, 0), pt(5, 10), pt(0, 0),
			}, CRS{}),
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.poly.IsConvex(); got != c.want {
				t.Errorf("IsConvex = %v, want %v", got, c.want)
			}
		})
	}
}

// TestClip_ConvexFastPath_KnownAreas verifies the Sutherland-Hodgman
// fast path against analytically-known intersection areas. This also
// implicitly verifies that dispatch picks the fast path — if it
// fell through to the sweep, the identical-polygon case would return
// half-area (see the "identical MultiPolygon-wrapped inputs mis-
// classify half the shared edges as differentTransition" limitation
// tracked as a follow-up).
func TestClip_ConvexFastPath_KnownAreas(t *testing.T) {
	octagon := regularPolygon(0, 0, 10, 8)
	octagonArea := polyPlanarArea(octagon) // analytical: 200√2 ≈ 282.84

	cases := []struct {
		name string
		a, b Polygon
		want float64
	}{
		{
			name: "identical octagons",
			a:    octagon,
			b:    regularPolygon(0, 0, 10, 8),
			want: octagonArea,
		},
		{
			name: "disjoint",
			a:    SimplePolygon([]Point{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0)}, CRS{}),
			b:    SimplePolygon([]Point{pt(10, 10), pt(11, 10), pt(11, 11), pt(10, 11), pt(10, 10)}, CRS{}),
			want: 0,
		},
		{
			name: "small contained in large",
			a:    regularPolygon(0, 0, 100, 6),
			b:    regularPolygon(0, 0, 10, 6),
			want: polyPlanarArea(regularPolygon(0, 0, 10, 6)),
		},
		{
			name: "axis-aligned squares half-overlap",
			a:    unitSquare(0, 0, 10),
			b:    unitSquare(5, 0, 10),
			want: 50, // 5×10 overlap
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Clip(c.a, c.b)
			if err != nil {
				t.Fatalf("Clip: %v", err)
			}
			area := clipTotalArea(got)
			if c.want == 0 {
				if area != 0 {
					t.Errorf("area = %v, want 0", area)
				}
				return
			}
			if rel := math.Abs(area-c.want) / c.want; rel > 1e-9 {
				t.Errorf("area = %v, want %v (rel err %g)", area, c.want, rel)
			}
		})
	}
}

// TestClip_ConvexFastPath_Disjoint verifies the fast path handles the
// disjoint-bboxes case correctly (returns an empty polygon, not junk).
func TestClip_ConvexFastPath_Disjoint(t *testing.T) {
	a := SimplePolygon([]Point{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0)}, CRS{})
	b := SimplePolygon([]Point{pt(10, 10), pt(11, 10), pt(11, 11), pt(10, 11), pt(10, 10)}, CRS{})
	got, err := Clip(a, b)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	if area := clipTotalArea(got); area != 0 {
		t.Errorf("disjoint area = %v, want 0", area)
	}
}

// TestClip_ConvexFastPath_Contained: one convex fully inside another.
func TestClip_ConvexFastPath_Contained(t *testing.T) {
	outer := regularPolygon(0, 0, 100, 6)
	inner := regularPolygon(0, 0, 10, 6)
	got, err := Clip(inner, outer)
	if err != nil {
		t.Fatalf("Clip: %v", err)
	}
	innerArea := polyPlanarArea(inner)
	gotArea := clipTotalArea(got)
	if rel := math.Abs(gotArea-innerArea) / innerArea; rel > 1e-9 {
		t.Errorf("contained clip area = %v, want %v (rel err %g)", gotArea, innerArea, rel)
	}
}

// regularPolygonAt is regularPolygon offset from the origin — convenient
// for building sliding-overlap test cases.
func regularPolygonAt(cx, cy, r float64, n int) Polygon {
	pts := make([]Point, n+1)
	for i := 0; i < n; i++ {
		theta := 2 * math.Pi * float64(i) / float64(n)
		pts[i] = pt(cx+r*math.Cos(theta), cy+r*math.Sin(theta))
	}
	pts[n] = pts[0]
	return SimplePolygon(pts, CRS{})
}

func clipTotalArea(g Geometry) float64 {
	switch v := g.(type) {
	case Polygon:
		return polyPlanarArea(v)
	case MultiPolygon:
		var t float64
		for _, p := range v.Polygons {
			t += polyPlanarArea(p)
		}
		return t
	}
	return 0
}

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
	// Iteration count raised from 100 → 1000. Rare degeneracies in
	// the transition-count gate (e.g. subject vertex exactly on
	// clip edge) fire on ~1/500 random configurations; 100 iters
	// missed them, 1000 gives a comfortable margin.
	for iter := range 1000 {
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

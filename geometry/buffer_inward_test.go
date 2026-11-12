package geometry

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func inwardRing(xy ...float64) []Point {
	r := make([]Point, 0, len(xy)/2+1)
	for i := 0; i < len(xy); i += 2 {
		r = append(r, pt(xy[i], xy[i+1]))
	}
	return append(r, r[0])
}

func bufferInwardArea(t *testing.T, g Geometry, d float64) (Geometry, float64) {
	t.Helper()
	out, err := Buffer(g, d, BufferOptions{})
	if err != nil {
		t.Fatalf("Buffer(%v): %v", d, err)
	}
	return out, clipTotalArea(out)
}

// A square has no reflex corners, so shrinking it is exact: every
// side moves in by the distance and the corners stay sharp.
func TestBufferInward_Square(t *testing.T) {
	ccw := inwardRing(0, 0, 10, 0, 10, 10, 0, 10)
	cw := slices.Clone(ccw)
	slices.Reverse(cw)
	for name, ring := range map[string][]Point{"ccw": ccw, "cw": cw} {
		for _, style := range []BufferStyle{BufferRound, BufferSquare} {
			out, err := Buffer(Polygon{Rings: [][]Point{ring}}, -1, BufferOptions{Style: style})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			poly, ok := out.(Polygon)
			if !ok {
				t.Fatalf("%s: got %T, want Polygon", name, out)
			}
			if a := clipTotalArea(poly); math.Abs(a-64) > 1e-9 {
				t.Errorf("%s style %d: area = %v, want 64", name, style, a)
			}
			if b := poly.Bounds(); b.MinX != 1 || b.MinY != 1 || b.MaxX != 9 || b.MaxY != 9 {
				t.Errorf("%s style %d: bounds = %+v, want 1..9", name, style, b)
			}
		}
	}
}

// A hole grows by the distance, with rounded corners: the hole's
// vertices are the polygon's reflex vertices.
func TestBufferInward_HoleGrows(t *testing.T) {
	shell := inwardRing(0, 0, 20, 0, 20, 20, 0, 20)
	for name, hole := range map[string][]Point{
		"cw hole":  inwardRing(8, 8, 8, 12, 12, 12, 12, 8),
		"ccw hole": inwardRing(8, 8, 12, 8, 12, 12, 8, 12),
	} {
		_, got := bufferInwardArea(t, Polygon{Rings: [][]Point{shell, hole}}, -1)
		// 18×18 shell, minus the 4×4 hole grown by 1: the hole, four
		// 4×1 side strips and four quarter disks of radius 1.
		want := 324 - (16 + 16 + math.Pi)
		// The disks are 32-gons inscribed in the circle: ~0.02 short.
		if math.Abs(got-want) > 0.03 || got < want {
			t.Errorf("%s: area = %v, want just over %v", name, got, want)
		}
	}
}

// The inner corner of an L is reflex and comes out rounded; the five
// outer corners stay sharp.
func TestBufferInward_ConcaveCorner(t *testing.T) {
	l := Polygon{Rings: [][]Point{inwardRing(0, 0, 10, 0, 10, 5, 5, 5, 5, 10, 0, 10)}}
	_, got := bufferInwardArea(t, l, -1)
	// area − perimeter×d + d² per outer corner − a quarter disk at the
	// inner corner.
	want := 75 - 40 + 5 - math.Pi/4
	if math.Abs(got-want) > 0.01 || got < want {
		t.Errorf("area = %v, want just over %v", got, want)
	}
}

// Two rooms joined by a corridor narrower than twice the distance
// come apart.
func TestBufferInward_Splits(t *testing.T) {
	dumbbell := Polygon{Rings: [][]Point{inwardRing(
		0, 0, 10, 0, 10, 4, 20, 4, 20, 0, 30, 0, 30, 10, 20, 10, 20, 6, 10, 6, 10, 10, 0, 10,
	)}}
	out, got := bufferInwardArea(t, dumbbell, -1.5)
	mp, ok := out.(MultiPolygon)
	if !ok || len(mp.Polygons) != 2 {
		t.Fatalf("got %T %+v, want a MultiPolygon of 2 rooms", out, out)
	}
	// Each room keeps its 7×7 core plus a little in front of the
	// corridor mouth (at most the 2×1.5 mouth strip).
	if got < 98 || got > 98+6 {
		t.Errorf("area = %v, want between 98 and 104", got)
	}
	a0, a1 := polyPlanarArea(mp.Polygons[0]), polyPlanarArea(mp.Polygons[1])
	if math.Abs(a0-a1) > 1e-6 {
		t.Errorf("rooms differ: %v vs %v", a0, a1)
	}
}

// Nothing is left of a polygon narrower than twice the distance, or
// of a geometry with no interior.
func TestBufferInward_Empty(t *testing.T) {
	square := Polygon{Rings: [][]Point{inwardRing(0, 0, 10, 0, 10, 10, 0, 10)}, CRSValue: PseudoMercator}
	for name, g := range map[string]Geometry{
		"collapsed square": square,
		"thin sliver":      Polygon{Rings: [][]Point{inwardRing(0, 0, 100, 0, 100, 1, 0, 1)}, CRSValue: PseudoMercator},
		"point":            Point{X: 1, Y: 2, CRSValue: PseudoMercator},
		"line":             LineString{Points: []Point{pt(0, 0), pt(5, 5)}, CRSValue: PseudoMercator},
		"multipolygon":     MultiPolygon{Polygons: []Polygon{square}, CRSValue: PseudoMercator},
	} {
		out, err := Buffer(g, -6, BufferOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		poly, ok := out.(Polygon)
		if !ok || len(poly.Rings) != 0 {
			t.Errorf("%s: got %T %+v, want a Polygon with no rings", name, out, out)
		}
		if !out.CRS().Equal(PseudoMercator) {
			t.Errorf("%s: CRS = %v, want it kept", name, out.CRS())
		}
	}
}

// Each member of a MultiPolygon shrinks; members that vanish are
// dropped. A geographic CRS works (the distance is in degrees, as for
// outward buffers) and is kept.
func TestBufferInward_MultiPolygonAndCRS(t *testing.T) {
	mp := MultiPolygon{
		Polygons: []Polygon{
			{Rings: [][]Point{inwardRing(0, 0, 10, 0, 10, 10, 0, 10)}},
			{Rings: [][]Point{inwardRing(20, 0, 21, 0, 21, 1, 20, 1)}}, // vanishes
			{Rings: [][]Point{inwardRing(30, 0, 36, 0, 36, 6, 30, 6)}},
		},
		CRSValue: WGS84,
	}
	out, got := bufferInwardArea(t, mp, -1)
	res, ok := out.(MultiPolygon)
	if !ok || len(res.Polygons) != 2 {
		t.Fatalf("got %T %+v, want a MultiPolygon of 2", out, out)
	}
	if math.Abs(got-(64+16)) > 1e-9 {
		t.Errorf("area = %v, want 80", got)
	}
	if !out.CRS().Equal(WGS84) {
		t.Errorf("CRS = %v, want WGS84", out.CRS())
	}
}

// Growing then shrinking a convex polygon by the same distance gives
// back (nearly) the original: a check that the two directions agree.
func TestBufferInward_UndoesOutward(t *testing.T) {
	square := Polygon{Rings: [][]Point{inwardRing(0, 0, 10, 0, 10, 10, 0, 10)}}
	grown, err := Buffer(square, 2, BufferOptions{Segments: 64})
	if err != nil {
		t.Fatal(err)
	}
	_, got := bufferInwardArea(t, grown, -2)
	if math.Abs(got-100) > 0.05 {
		t.Errorf("area after +2 then −2 = %v, want ~100", got)
	}
}

// TestBufferInward_MatchesDistanceToBoundary checks the definition
// directly on irregular polygons at arbitrary angles: a point is in
// the result exactly when it is inside the polygon and more than the
// distance from its boundary.
func TestBufferInward_MatchesDistanceToBoundary(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 9))
	distToRing := func(p Point, ring []Point) float64 {
		best := math.Inf(1)
		for i := range len(ring) - 1 {
			a, b := ring[i], ring[i+1]
			dx, dy := b.X-a.X, b.Y-a.Y
			u := ((p.X-a.X)*dx + (p.Y-a.Y)*dy) / (dx*dx + dy*dy)
			u = math.Max(0, math.Min(1, u))
			best = math.Min(best, math.Hypot(p.X-(a.X+u*dx), p.Y-(a.Y+u*dy)))
		}
		return best
	}
	for trial := range 40 {
		// A star-shaped ring: mixed convex and reflex corners, no
		// self-intersections. Every third trial gets a hole.
		n := 5 + r.IntN(12)
		cx, cy := r.Float64()*1000-500, r.Float64()*1000-500
		shell := make([]Point, 0, n+1)
		for i := range n {
			a := 2 * math.Pi * (float64(i) + 0.8*r.Float64()) / float64(n)
			rad := 20 + 30*r.Float64()
			shell = append(shell, pt(cx+rad*math.Cos(a), cy+rad*math.Sin(a)))
		}
		shell = append(shell, shell[0])
		rings := [][]Point{shell}
		if trial%3 == 0 {
			rings = append(rings, inwardRing(cx-4, cy-3, cx+5, cy-2, cx+1, cy+6))
		}
		poly := Polygon{Rings: rings}
		d := 1 + 5*r.Float64()

		out, err := Buffer(poly, -d, BufferOptions{})
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		parts := appendPolygons(nil, out)
		for _, p := range parts {
			for _, ring := range p.Rings {
				if len(ring) < 4 || math.Abs(ringSignedArea(ring)) < 1e-9 {
					t.Fatalf("trial %d: degenerate ring in result: %v", trial, ring)
				}
			}
		}
		// Rounded corners are inscribed in the true arc, so points in a
		// thin shell around the exact answer can go either way.
		slack := d*(1-math.Cos(math.Pi/DefaultBufferSegments)) + 1e-6
		for range 400 {
			a, rad := 2*math.Pi*r.Float64(), 55*math.Sqrt(r.Float64())
			q := pt(cx+rad*math.Cos(a), cy+rad*math.Sin(a))
			dist := math.Inf(1)
			for _, ring := range rings {
				dist = math.Min(dist, distToRing(q, ring))
			}
			if math.Abs(dist-d) <= slack {
				continue
			}
			want := pointInPolygon(q, poly) && dist > d
			got := false
			for _, p := range parts {
				got = got || pointInPolygon(q, p)
			}
			if got != want {
				t.Fatalf("trial %d (d=%.3f): point (%.4f, %.4f), %.4f from the boundary: in result = %v, want %v\npolygon: %v",
					trial, d, q.X, q.Y, dist, got, want, rings)
			}
		}
	}
}

// A buffer is 2D whichever way it goes: the output's vertices are
// offsets of the input's, so a 3D input's Z values have nowhere to go.
func TestBuffer_ResultIs2D(t *testing.T) {
	z := func(x, y float64) Point { return Point{X: x, Y: y, Z: 5, HasZ: true} }
	poly := Polygon{Rings: [][]Point{{z(0, 0), z(10, 0), z(10, 10), z(0, 10), z(0, 0)}}, HasZ: true}
	for _, d := range []float64{1, -1} {
		for name, g := range map[string]Geometry{"Polygon": poly, "MultiPolygon": MultiPolygon{Polygons: []Polygon{poly}, HasZ: true}} {
			out, err := Buffer(g, d, BufferOptions{})
			if err != nil {
				t.Fatalf("%s, distance %v: %v", name, d, err)
			}
			if out.Is3D() {
				t.Errorf("%s, distance %v: result is 3D, want 2D", name, d)
			}
			for _, p := range appendPolygons(nil, out) {
				for _, ring := range p.Rings {
					for _, v := range ring {
						if v.HasZ || v.Z != 0 {
							t.Fatalf("%s, distance %v: vertex %+v carries Z", name, d, v)
						}
					}
				}
			}
		}
	}
}

// A MultiPolygon shrinks as one region: an edge two members share is
// inside it, so nothing is taken off along that edge.
func TestBufferInward_AdjoiningMembers(t *testing.T) {
	mp := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{inwardRing(0, 0, 10, 0, 10, 10, 0, 10)}},
		{Rings: [][]Point{inwardRing(10, 0, 20, 0, 20, 10, 10, 10)}},
		{Rings: [][]Point{inwardRing(40, 0, 46, 0, 46, 6, 40, 6)}}, // on its own
	}, CRSValue: PseudoMercator}
	out, got := bufferInwardArea(t, mp, -1)
	res, ok := out.(MultiPolygon)
	if !ok || len(res.Polygons) != 2 {
		t.Fatalf("got %T with %d parts, want a MultiPolygon of 2: the merged pair and the lone square", out, len(appendPolygons(nil, out)))
	}
	// [1,19]×[1,9] and [41,45]×[1,5].
	if math.Abs(got-(18*8+16)) > 1e-9 {
		t.Errorf("area = %v, want 160", got)
	}
	var pair Polygon
	for _, p := range res.Polygons {
		if p.Bounds().MinX < 30 {
			pair = p
		}
	}
	if b := pair.Bounds(); b.MinX != 1 || b.MaxX != 19 || b.MinY != 1 || b.MaxY != 9 {
		t.Errorf("merged pair bounds = %+v, want 1..19 × 1..9 with no gap at x=10", b)
	}
	if !out.CRS().Equal(PseudoMercator) {
		t.Errorf("CRS = %v, want it kept", out.CRS())
	}
}

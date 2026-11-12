package geometry

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

// windSquare returns the closed ring of an axis-aligned rectangle,
// counter-clockwise or clockwise.
func windSquare(x0, y0, x1, y1 float64, ccw bool) []Point {
	r := []Point{pt(x0, y0), pt(x1, y0), pt(x1, y1), pt(x0, y1), pt(x0, y0)}
	if !ccw {
		slices.Reverse(r)
	}
	return r
}

// TestBoolean_WindingIndependent checks that boolean ops and Dissolve
// give the same answer however the inputs are wound. GeoJSON (RFC 7946)
// and OGC shells are counter-clockwise; shapefile shells are clockwise;
// holes in real data come both ways. Shared (coincident) edges used to
// be classified by ring-walking direction, so clipping a
// counter-clockwise polygon by a clockwise copy of itself returned an
// empty result.
//
// Every case shares at least part of an edge between the operands,
// because that is the path that depended on winding. Operands are
// wrapped in single-member MultiPolygons as well, which skips the
// Polygon × Polygon fast paths and exercises the sweep.
func TestBoolean_WindingIndependent(t *testing.T) {
	type rect struct{ x0, y0, x1, y1 float64 }
	type shape struct {
		shell rect
		holes []rect
	}
	cases := []struct {
		name                       string
		a, b                       shape
		inter, union, diff, symdif float64
	}{
		{"identical", shape{shell: rect{0, 0, 10, 10}}, shape{shell: rect{0, 0, 10, 10}}, 100, 100, 0, 0},
		{"adjacent full edge", shape{shell: rect{0, 0, 10, 10}}, shape{shell: rect{10, 0, 20, 10}}, 0, 200, 100, 200},
		{"adjacent partial edge", shape{shell: rect{0, 0, 10, 10}}, shape{shell: rect{10, 5, 20, 15}}, 0, 200, 100, 200},
		{"inside sharing a corner", shape{shell: rect{0, 0, 10, 10}}, shape{shell: rect{0, 0, 5, 5}}, 25, 100, 75, 75},
		{"overlap sharing an edge line", shape{shell: rect{0, 0, 10, 10}}, shape{shell: rect{5, 0, 15, 10}}, 50, 150, 50, 100},
		{
			"identical with hole",
			shape{rect{0, 0, 10, 10}, []rect{{3, 3, 6, 6}}}, shape{rect{0, 0, 10, 10}, []rect{{3, 3, 6, 6}}},
			91, 91, 0, 0,
		},
		{
			"plug fills the hole",
			shape{rect{0, 0, 10, 10}, []rect{{3, 3, 6, 6}}}, shape{shell: rect{3, 3, 6, 6}},
			0, 100, 91, 100,
		},
	}
	build := func(s shape, shellCCW, holeCCW bool) Polygon {
		rings := [][]Point{windSquare(s.shell.x0, s.shell.y0, s.shell.x1, s.shell.y1, shellCCW)}
		for _, h := range s.holes {
			rings = append(rings, windSquare(h.x0, h.y0, h.x1, h.y1, holeCCW))
		}
		return Polygon{Rings: rings}
	}
	check := func(t *testing.T, what string, g Geometry, err error, want float64) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
			return
		}
		if got := clipTotalArea(g); math.Abs(got-want) > 1e-9 {
			t.Errorf("%s area = %v, want %v", what, got, want)
		}
	}
	// Five independent switches, so 32 combinations per case: whether
	// the operands are wrapped in MultiPolygons, and whether each of the
	// four rings (a's shell and hole, b's shell and hole) is wound
	// counter-clockwise. Each bit of combo is one switch.
	const switches = 5
	for _, c := range cases {
		for combo := range 1 << switches {
			on := func(bit int) bool { return combo&(1<<bit) != 0 }
			multi, aShell, aHole, bShell, bHole := on(0), on(1), on(2), on(3), on(4)
			name := fmt.Sprintf("%s/multi=%v/a=%v,%v/b=%v,%v", c.name, multi, aShell, aHole, bShell, bHole)
			t.Run(name, func(t *testing.T) {
				var a, b Geometry = build(c.a, aShell, aHole), build(c.b, bShell, bHole)
				if multi {
					a = MultiPolygon{Polygons: []Polygon{a.(Polygon)}}
					b = MultiPolygon{Polygons: []Polygon{b.(Polygon)}}
				}
				g, err := Clip(a, b)
				check(t, "Clip", g, err, c.inter)
				g, err = Union(a, b)
				check(t, "Union", g, err, c.union)
				g, err = Difference(a, b)
				check(t, "Difference", g, err, c.diff)
				g, err = SymDifference(a, b)
				check(t, "SymDifference", g, err, c.symdif)
				g, err = Dissolve([]Geometry{a, b})
				check(t, "Dissolve", g, err, c.union)
			})
		}
	}
}

// TestBoolean_SlantedSharedEdge covers shared edges that are not
// axis-aligned. The sweep's collinearity test (signedArea) was fused
// into one multiply-subtract instruction on arm64 and so was not
// exactly zero for two coincident slanted edges; it happened to be
// exact for horizontal and vertical ones, which is all the other
// tests used. The union then dropped one operand.
func TestBoolean_SlantedSharedEdge(t *testing.T) {
	// The pair this was first seen on: a thin triangle fan and a
	// hexagon sharing one edge, from an inward buffer's boundary band.
	fan := Polygon{Rings: [][]Point{{
		pt(11.847759065022574, 10.76536686473018), pt(13.809329625829035, 11.155547508762435),
		pt(13.695518130045148, 11.530733729460357), pt(13.510698289627666, 11.876507330769382),
		pt(11.847759065022574, 10.76536686473018),
	}}}
	hex := Polygon{Rings: [][]Point{{
		pt(12, 10), pt(13.96157056080646, 10.390180644032256), pt(13.809329625829035, 11.155547508762435),
		pt(11.847759065022574, 10.76536686473018), pt(9.886188504216113, 10.375186220697923),
		pt(10.03842943919354, 9.609819355967744), pt(12, 10),
	}}}
	want := clipTotalArea(fan) + clipTotalArea(hex)
	for name, pair := range map[string][2]Geometry{"fan, hex": {fan, hex}, "hex, fan": {hex, fan}} {
		got, err := Union(pair[0], pair[1])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a := clipTotalArea(got); math.Abs(a-want) > 1e-9 {
			t.Errorf("%s: union area = %v, want %v", name, a, want)
		}
		if p, ok := got.(Polygon); !ok || len(p.Rings) != 1 {
			t.Errorf("%s: got %T with %d polygons, want one ring", name, got, len(appendPolygons(nil, got)))
		}
	}

	// The same unit squares, side by side and identical, turned to
	// many angles. Rotating each corner the same way keeps the shared
	// corners bit-identical, so the shared edge stays exactly shared.
	for deg := 1.0; deg < 90; deg += 3.7 {
		sin, cos := math.Sincos(deg * math.Pi / 180)
		turn := func(x, y float64) Point { return pt(100+7*(x*cos-y*sin), 50+7*(x*sin+y*cos)) }
		square := func(x float64) Polygon {
			return Polygon{Rings: [][]Point{{turn(x, 0), turn(x+1, 0), turn(x+1, 1), turn(x, 1), turn(x, 0)}}}
		}
		a, b := square(0), square(1)
		check := func(what string, g Geometry, err error, want float64) {
			t.Helper()
			if err != nil {
				t.Errorf("%.1f° %s: %v", deg, what, err)
				return
			}
			if got := clipTotalArea(g); math.Abs(got-want) > 1e-9 {
				t.Errorf("%.1f° %s: area = %v, want %v", deg, what, got, want)
			}
		}
		g, err := Union(a, b)
		check("side by side: Union", g, err, 98)
		g, err = Clip(a, b)
		check("side by side: Clip", g, err, 0)
		g, err = Difference(a, b)
		check("side by side: Difference", g, err, 49)
		g, err = SymDifference(a, b)
		check("side by side: SymDifference", g, err, 98)
		g, err = Dissolve([]Geometry{a, b, square(2), square(3)})
		check("row of four: Dissolve", g, err, 196)

		// MultiPolygon wrappers skip the Polygon × Polygon fast paths.
		ma, mb := MultiPolygon{Polygons: []Polygon{a}}, MultiPolygon{Polygons: []Polygon{square(0)}}
		g, err = Union(ma, mb)
		check("identical: Union", g, err, 49)
		g, err = Clip(ma, mb)
		check("identical: Clip", g, err, 49)
		g, err = Difference(ma, mb)
		check("identical: Difference", g, err, 0)
	}
}

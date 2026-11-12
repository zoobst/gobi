package geometry

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/zoobst/gobi/internal/detmath"
)

// The geometry package gives the same bits on every platform: a
// buffer, clip or reprojection computed in a browser (wasm) and on a
// server (arm64 or amd64) are byte-identical WKB. Two things make
// that so. Its own arithmetic never lets the compiler fuse a multiply
// into an add (TestNoFusedMultiplyAdd), and its trigonometry comes
// from internal/detmath, not from math, whose results differ between
// platforms in the last bit.
//
// TestSameResultsOnEveryPlatform is the end-to-end check: it runs a
// spread of operations on fixed inputs and compares a digest of every
// output coordinate with one recorded once. A platform that computes
// anything differently fails it.

// digest accumulates results.
type digest struct {
	t *testing.T
	h interface {
		Write([]byte) (int, error)
		Sum64() uint64
	}
}

func (d *digest) floats(vs ...float64) {
	var b [8]byte
	for _, v := range vs {
		bits := math.Float64bits(v)
		if math.IsNaN(v) {
			bits = 0x7FF8000000000001 // one NaN
		}
		for k := range 8 {
			b[k] = byte(bits >> (8 * k))
		}
		d.h.Write(b[:])
	}
}

// geom adds every coordinate of g, and its structure, as WKB.
func (d *digest) geom(g Geometry, err error) {
	d.t.Helper()
	if err != nil {
		d.t.Fatal(err)
	}
	d.h.Write(WKB(g))
}

func (d *digest) value(v float64, err error) {
	d.t.Helper()
	if err != nil {
		d.t.Fatal(err)
	}
	d.floats(v)
}

func (d *digest) points(pts []Point) {
	for _, p := range pts {
		d.floats(p.X, p.Y)
	}
}

// fixture builds the test inputs. Its own arithmetic follows the
// package's rule (every product rounded before it is added), because
// a fused multiply here would feed different inputs to different
// platforms and the test would fail for the wrong reason.
type fixture struct{ r *rand.Rand }

func (f fixture) unit() float64 { return float64(f.r.Uint64()>>11) / (1 << 53) }

func (f fixture) span(lo, hi float64) float64 { return lo + float64(f.unit()*(hi-lo)) }

// star is a closed star-shaped ring of n vertices around (cx, cy):
// convex and reflex corners, no self-intersections, slanted edges.
func (f fixture) star(cx, cy float64, n int, rMin, rMax float64) []Point {
	ring := make([]Point, 0, n+1)
	for k := range n {
		a := float64(2*math.Pi*(float64(k)+float64(0.8*f.unit()))) / float64(n)
		rad := f.span(rMin, rMax)
		ring = append(ring, Point{X: cx + float64(rad*detmath.Cos(a)), Y: cy + float64(rad*detmath.Sin(a))})
	}
	return append(ring, ring[0])
}

func (f fixture) walk(x, y float64, n int, step float64) LineString {
	l := LineString{Points: []Point{{X: x, Y: y}}}
	for range n {
		last := l.Points[len(l.Points)-1]
		l.Points = append(l.Points, Point{X: last.X + f.span(-step, step), Y: last.Y + f.span(-step, step)})
	}
	return l
}

var determinismCases = []struct {
	name string
	run  func(d *digest, f fixture)
}{
	{"boolean ops", func(d *digest, f fixture) {
		for range 25 {
			// At UTM-sized coordinates, overlapping.
			cx, cy := f.span(4e5, 6e5), f.span(4e6, 5e6)
			a := Polygon{Rings: [][]Point{f.star(cx, cy, 5+f.r.IntN(20), 20, 50)}}
			b := Polygon{Rings: [][]Point{f.star(cx+f.span(-30, 30), cy+f.span(-30, 30), 5+f.r.IntN(20), 20, 50)}}
			d.geom(Clip(a, b))
			d.geom(Union(a, b))
			d.geom(Difference(a, b))
			d.geom(SymDifference(a, b))
		}
	}},
	{"dissolve", func(d *digest, f fixture) {
		var parts []Geometry
		for i := range 40 {
			parts = append(parts, Polygon{Rings: [][]Point{f.star(float64(i%8)*45, float64(i/8)*45, 12, 15, 35)}})
		}
		// Tiles sharing slanted edges.
		for i := range 6 {
			x := float64(i) * 10
			parts = append(parts, Polygon{Rings: [][]Point{{
				{X: x, Y: -100 + x/3}, {X: x + 10, Y: -100 + (x+10)/3}, {X: x + 10, Y: -80 + (x+10)/3}, {X: x, Y: -80 + x/3}, {X: x, Y: -100 + x/3},
			}}})
		}
		d.geom(Dissolve(parts))
	}},
	{"line clip", func(d *digest, f fixture) {
		var polys []Polygon
		for i := range 12 {
			polys = append(polys, Polygon{Rings: [][]Point{f.star(float64(i%4)*90, float64(i/4)*90, 30, 20, 45)}})
		}
		m, err := NewClipMask(MultiPolygon{Polygons: polys})
		if err != nil {
			d.t.Fatal(err)
		}
		for range 60 {
			d.geom(m.Clip(f.walk(f.span(-50, 320), f.span(-50, 230), 2+f.r.IntN(40), 60)))
			d.geom(m.Clip(Point{X: f.span(-50, 320), Y: f.span(-50, 230)}))
		}
	}},
	{"round buffer", func(d *digest, f fixture) {
		for _, segments := range []int{0, 8, 64} {
			opts := BufferOptions{Segments: segments}
			d.geom(Buffer(Point{X: f.span(4e5, 6e5), Y: f.span(4e6, 5e6)}, f.span(1, 500), opts))
			d.geom(Buffer(f.walk(5e5, 4.5e6, 30, 40), f.span(1, 10), opts))
			d.geom(Buffer(Polygon{Rings: [][]Point{f.star(5e5, 4.5e6, 9, 60, 80)}}, f.span(1, 10), opts))
		}
	}},
	{"square buffer", func(d *digest, f fixture) {
		opts := BufferOptions{Style: BufferSquare}
		d.geom(Buffer(Point{X: f.span(4e5, 6e5), Y: f.span(4e6, 5e6)}, f.span(1, 500), opts))
		d.geom(Buffer(f.walk(5e5, 4.5e6, 30, 40), f.span(1, 10), opts))
		d.geom(Buffer(Polygon{Rings: [][]Point{f.star(5e5, 4.5e6, 9, 60, 80)}}, f.span(1, 10), opts))
	}},
	{"inward buffer", func(d *digest, f fixture) {
		for range 12 {
			cx, cy := f.span(4e5, 6e5), f.span(4e6, 5e6)
			d.geom(Buffer(Polygon{Rings: [][]Point{f.star(cx, cy, 5+f.r.IntN(12), 20, 50)}}, -f.span(1, 6), BufferOptions{}))
		}
	}},
	{"area, centroid, length, distance", func(d *digest, f fixture) {
		for range 40 {
			cx, cy := f.span(4e5, 6e5), f.span(4e6, 5e6)
			poly := Polygon{Rings: [][]Point{f.star(cx, cy, 5+f.r.IntN(30), 0.01, 50)}, CRSValue: PseudoMercator}
			line := f.walk(cx, cy, 20, 30)
			line.CRSValue = PseudoMercator
			d.value(poly.Area(UnitMeters))
			c := poly.Centroid()
			d.floats(c.X, c.Y)
			c = line.Centroid()
			d.floats(c.X, c.Y)
			d.value(line.Length(UnitMeters))
			d.value(GeomDistance(poly, Point{X: cx + 200, Y: cy + 100, CRSValue: PseudoMercator}, UnitMeters))
			d.value(GeomDistance(line, poly, UnitMeters))
		}
	}},
	{"geographic measures", func(d *digest, f fixture) {
		for range 200 {
			a := Point{X: f.span(-180, 180), Y: f.span(-85, 85), CRSValue: WGS84}
			b := Point{X: f.span(-180, 180), Y: f.span(-85, 85), CRSValue: WGS84}
			d.value(Haversine(a, b, UnitMeters))
			pts, err := SampleGeodesic(a, b, 7)
			if err != nil {
				d.t.Fatal(err)
			}
			d.points(pts)
		}
		ring := f.star(10, 45, 16, 0.5, 1)
		poly := Polygon{Rings: [][]Point{ring}, CRSValue: WGS84}
		d.value(poly.Area(UnitMeters))
		d.value(LineString{Points: ring, CRSValue: WGS84}.Length(UnitMeters))
		d.geom(DensifyGeodesic(LineString{Points: ring[:6], CRSValue: WGS84}, 5000))
	}},
	{"reprojection", func(d *digest, f fixture) {
		for range 60 {
			lon, lat := f.span(-179, 179), f.span(-80, 84)
			l := LineString{CRSValue: WGS84}
			for range 8 {
				l.Points = append(l.Points, Point{X: lon + f.span(-1, 1), Y: lat + f.span(-1, 1)})
			}
			utm, err := LookupCRS(UTMEpsgFor(lon, lat))
			if err != nil {
				d.t.Fatal(err)
			}
			inUTM, err := Project(l, utm)
			d.geom(inUTM, err)
			d.geom(Project(inUTM, WGS84))
			inMercator, err := Project(l, PseudoMercator)
			d.geom(inMercator, err)
			d.geom(Project(inMercator, WGS84))
			d.geom(Project(inUTM, PseudoMercator))
		}
	}},
	{"circle and ellipse", func(d *digest, f fixture) {
		for range 20 {
			center := Point{X: f.span(4e5, 6e5), Y: f.span(4e6, 5e6)}
			d.geom(Circle{Center: center, Radius: f.span(1, 300)}.Boundary(48), nil)
			e := NewEllipse(center, f.span(50, 300), f.span(10, 50), f.span(0, 3))
			d.geom(e.Boundary(64), nil)
			d.floats(e.Area(), e.Circumference())
		}
	}},
	{"simplify and hull", func(d *digest, f fixture) {
		for range 20 {
			line := f.walk(f.span(4e5, 6e5), f.span(4e6, 5e6), 200, 15)
			d.geom(Simplify(line, f.span(1, 20)))
			poly := Polygon{Rings: [][]Point{f.star(5e5, 4.5e6, 60, 30, 90)}}
			d.geom(Simplify(poly, f.span(1, 10)))
			d.geom(ConvexHull(poly), nil)
			d.geom(ConvexHull(line), nil)
		}
	}},
}

// determinismDigests are the digests of determinismCases, recorded
// once and reproduced by every platform. When a change is meant to
// alter results, run the test, check the new digests are the same on
// arm64, amd64 and wasm (see the failure message), and update them.
var determinismDigests = map[string]uint64{
	"boolean ops":                      0x679ea3427434e459,
	"dissolve":                         0x2ffc12c26cd771ca,
	"line clip":                        0xe6670069130cec5b,
	"round buffer":                     0x1d582ac3a4a5ed4a,
	"square buffer":                    0x4659abfe07a0d215,
	"inward buffer":                    0xb4bb302f21e3d8bc,
	"area, centroid, length, distance": 0xf701b6888691b8eb,
	"geographic measures":              0x7294b2f2713006eb,
	"reprojection":                     0x92dc848e81ba33ee,
	"circle and ellipse":               0x85342213ffa7c58e,
	"simplify and hull":                0x041a180c1b981e5e,
}

func TestSameResultsOnEveryPlatform(t *testing.T) {
	for i, c := range determinismCases {
		d := &digest{t: t, h: fnv.New64a()}
		c.run(d, fixture{r: rand.New(rand.NewPCG(uint64(i)+1, 77))})
		got := d.h.Sum64()
		if want, ok := determinismDigests[c.name]; !ok || got != want {
			t.Errorf("%s: digest %#016x on %s/%s, recorded %#016x.\n"+
				"If nothing was meant to change, this platform computes something differently from the others.\n"+
				"If the change is intended, record the new digest once all of arm64, amd64 and wasm agree on it:\n"+
				"  go test ./geometry -run TestSameResultsOnEveryPlatform\n"+
				"  GOARCH=amd64 go test ./geometry -run TestSameResultsOnEveryPlatform\n"+
				"  GOOS=js GOARCH=wasm go test ./geometry -run TestSameResultsOnEveryPlatform",
				c.name, got, runtime.GOOS, runtime.GOARCH, want)
		}
	}
}

// Dissolve returns the same geometry, parts in the same order, every
// time. Its clusters used to be taken in map-iteration order, which
// Go randomizes, so with two separate clusters the result's polygons
// swapped places about every other run.
func TestDissolve_SameOrderEveryRun(t *testing.T) {
	var parts []Geometry
	for i := range 9 {
		// Nine squares far apart: nine clusters.
		parts = append(parts, unitSquare(float64(i)*100, float64(i%3)*100, 10))
	}
	first, err := Dissolve(parts)
	if err != nil {
		t.Fatal(err)
	}
	want := string(WKB(first))
	for run := range 30 {
		got, err := Dissolve(parts)
		if err != nil {
			t.Fatal(err)
		}
		if string(WKB(got)) != want {
			t.Fatalf("run %d returned a different geometry from the first", run+1)
		}
	}
	// And in the order the inputs came in.
	mp, ok := first.(MultiPolygon)
	if !ok || len(mp.Polygons) != 9 {
		t.Fatalf("got %T, want a MultiPolygon of 9", first)
	}
	for i, p := range mp.Polygons {
		if b := p.Bounds(); b.MinX != float64(i)*100 {
			t.Errorf("part %d starts at x=%v, want %v", i, b.MinX, float64(i)*100)
		}
	}
}

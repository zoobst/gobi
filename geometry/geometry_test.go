package geometry

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// approx compares floats with an absolute tolerance.
func approx(t *testing.T, got, want, tol float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %v, want %v (±%v)", msg, got, want, tol)
	}
}

// pt is a shorthand for a CRS-less Point used in tests.
func pt(x, y float64) Point { return Point{X: x, Y: y} }

func TestPointWKB_RoundTrip(t *testing.T) {
	p := Point{X: -73.9857, Y: 40.7484}
	buf := WKB(p)
	g, err := ParseWKB(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := g.(Point)
	if !ok {
		t.Fatalf("expected Point, got %T", g)
	}
	approx(t, got.X, p.X, 0, "X")
	approx(t, got.Y, p.Y, 0, "Y")
}

func TestLineStringWKB_RoundTrip(t *testing.T) {
	l := LineString{Points: []Point{
		pt(0, 0), pt(1, 1), pt(2, -1),
	}}
	buf := WKB(l)
	g, err := ParseWKB(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := g.(LineString)
	if !ok {
		t.Fatalf("expected LineString, got %T", g)
	}
	if len(got.Points) != 3 {
		t.Fatalf("wrong point count: %d", len(got.Points))
	}
}

func TestPolygonWKB_RoundTripWithHole(t *testing.T) {
	p := Polygon{Rings: [][]Point{
		{pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0)},
		{pt(3, 3), pt(7, 3), pt(7, 7), pt(3, 7), pt(3, 3)},
	}}
	buf := WKB(p)
	g, err := ParseWKB(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := g.(Polygon)
	if !ok {
		t.Fatalf("expected Polygon, got %T", g)
	}
	if len(got.Rings) != 2 {
		t.Fatalf("expected 2 rings, got %d", len(got.Rings))
	}
	if len(got.Rings[1]) != 5 {
		t.Fatalf("hole ring wrong length: %d", len(got.Rings[1]))
	}
}

func TestPolygonWKB_ClosesUnclosedRings(t *testing.T) {
	unclosed := Polygon{Rings: [][]Point{
		{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1)},
	}}
	buf := WKB(unclosed)
	g, _ := ParseWKB(buf)
	got := g.(Polygon)
	first := got.Rings[0][0]
	last := got.Rings[0][len(got.Rings[0])-1]
	if first.X != last.X || first.Y != last.Y {
		t.Fatalf("WKB output ring not closed")
	}
}

func TestWKB_UnsupportedType(t *testing.T) {
	// byte order + type=99
	buf := []byte{wkbNDR, 99, 0, 0, 0}
	_, err := ParseWKB(buf)
	if !errors.Is(err, ErrUnsupportedWKB) {
		t.Fatalf("want ErrUnsupportedWKB, got %v", err)
	}
}

func TestWKB_ShortBuffer(t *testing.T) {
	_, err := ParseWKB([]byte{wkbNDR})
	if !errors.Is(err, ErrShortWKB) {
		t.Fatalf("want ErrShortWKB, got %v", err)
	}
}

func TestParseWKT_PointAndPolygon(t *testing.T) {
	g, err := ParseWKT("POINT (1 2)")
	if err != nil {
		t.Fatal(err)
	}
	p := g.(Point)
	if p.X != 1 || p.Y != 2 {
		t.Fatalf("point: %+v", p)
	}

	g, err = ParseWKT("POLYGON ((0 0, 10 0, 10 10, 0 10, 0 0), (3 3, 7 3, 7 7, 3 7, 3 3))")
	if err != nil {
		t.Fatal(err)
	}
	poly := g.(Polygon)
	if len(poly.Rings) != 2 {
		t.Fatalf("wanted 2 rings, got %d", len(poly.Rings))
	}
}

func TestParseWKT_InvalidKeyword(t *testing.T) {
	_, err := ParseWKT("BANANA (1 2)")
	if !errors.Is(err, ErrInvalidWKT) {
		t.Fatalf("want ErrInvalidWKT, got %v", err)
	}
}

func TestWKT_WKB_CrossParse(t *testing.T) {
	orig, err := ParseWKT("POINT (-73.9857 40.7484)")
	if err != nil {
		t.Fatal(err)
	}
	buf := WKB(orig)
	back, err := ParseWKB(buf)
	if err != nil {
		t.Fatal(err)
	}
	if orig.WKT() != back.WKT() {
		t.Fatalf("WKT round trip mismatch: %s vs %s", orig.WKT(), back.WKT())
	}
}

func TestHaversine_NYCToLondon(t *testing.T) {
	// Known reference: ~5570 km between NYC and London
	d, err := Haversine(
		Point{X: -73.9857, Y: 40.7484},
		Point{X: -0.1276, Y: 51.5074},
		UnitKilometers,
	)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, d, 5570, 20, "NYC→London")
}

func TestHaversine_UnitConsistency(t *testing.T) {
	km, _ := Haversine(Point{X: 0, Y: 0}, Point{X: 1, Y: 0}, UnitKilometers)
	mi, _ := Haversine(Point{X: 0, Y: 0}, Point{X: 1, Y: 0}, UnitMiles)
	approx(t, km/mi, 1.609344, 1e-6, "km/mi ratio")
}

func TestPointDistance_CRSMismatch(t *testing.T) {
	a := Point{X: 0, Y: 0, CRSValue: WGS84}
	b := Point{X: 0, Y: 0, CRSValue: PseudoMercator}
	_, err := a.Distance(b, UnitMeters)
	if !errors.Is(err, ErrCRSMismatch) {
		t.Fatalf("want ErrCRSMismatch, got %v", err)
	}
}

func TestPolygonArea_UnitSquare_Projected(t *testing.T) {
	// 1×1 square in a projected (metric) CRS: area = 1 m² = 0.000001 km²
	p := SimplePolygon([]Point{
		pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0),
	}, PseudoMercator)
	a, err := p.Area(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, a, 1, 1e-9, "m²")
	akm, _ := p.Area(UnitKilometers)
	approx(t, akm, 1e-6, 1e-12, "km²")
}

func TestPolygonArea_WithHole(t *testing.T) {
	// 10×10 square with a 4×4 hole = 100 - 16 = 84 m²
	p := Polygon{
		Rings: [][]Point{
			{pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0)},
			{pt(3, 3), pt(7, 3), pt(7, 7), pt(3, 7), pt(3, 3)},
		},
		CRSValue: PseudoMercator,
	}
	a, err := p.Area(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, a, 84, 1e-9, "with hole")
}

func TestPolygonPerimeter_Projected(t *testing.T) {
	p := SimplePolygon([]Point{
		pt(0, 0), pt(3, 0), pt(3, 4), pt(0, 4),
	}, PseudoMercator)
	l, err := p.Perimeter(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, l, 14, 1e-9, "perimeter") // 3+4+3+4
}

func TestPolygonCentroid_Square(t *testing.T) {
	p := SimplePolygon([]Point{
		pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0),
	}, PseudoMercator)
	c := p.Centroid()
	approx(t, c.X, 5, 1e-9, "cx")
	approx(t, c.Y, 5, 1e-9, "cy")
}

func TestPolygonContains(t *testing.T) {
	p := SimplePolygon([]Point{
		pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0),
	}, PseudoMercator)
	if !p.Contains(Point{X: 5, Y: 5}) {
		t.Fatal("center should be inside")
	}
	if p.Contains(Point{X: -1, Y: 5}) {
		t.Fatal("outside x should be outside")
	}
}

func TestPolygonContains_HoleExcluded(t *testing.T) {
	p := Polygon{Rings: [][]Point{
		{pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0)},
		{pt(3, 3), pt(7, 3), pt(7, 7), pt(3, 7), pt(3, 3)},
	}, CRSValue: PseudoMercator}
	if p.Contains(Point{X: 5, Y: 5}) {
		t.Fatal("hole center should be excluded")
	}
	if !p.Contains(Point{X: 1, Y: 1}) {
		t.Fatal("outside hole but inside outer should be included")
	}
}

func TestConvexHull_Square(t *testing.T) {
	// Points inside a 10x10 square should reduce to just the 4 corners.
	p := SimplePolygon([]Point{
		pt(0, 0), pt(5, 1), pt(10, 0), pt(9, 5), pt(10, 10), pt(5, 9), pt(0, 10), pt(1, 5),
	}, PseudoMercator)
	h := p.ConvexHull()
	// Exterior of hull is closed: 5 points (4 corners + repeated first).
	if len(h.Exterior()) != 5 {
		t.Fatalf("hull ring len = %d, want 5", len(h.Exterior()))
	}
}

func TestBounds_ExtendAndUnion(t *testing.T) {
	b := EmptyBounds()
	if !b.Empty() {
		t.Fatal("EmptyBounds should be empty")
	}
	b = b.Extend(1, 2)
	b = b.Extend(5, -1)
	if b.MinX != 1 || b.MinY != -1 || b.MaxX != 5 || b.MaxY != 2 {
		t.Fatalf("bounds after extend: %+v", b)
	}
	other := Bounds{MinX: -10, MinY: -10, MaxX: 0, MaxY: 0}
	u := b.Union(other)
	if u.MinX != -10 || u.MinY != -10 || u.MaxX != 5 || u.MaxY != 2 {
		t.Fatalf("union: %+v", u)
	}
}

func TestCRSRegistry(t *testing.T) {
	c, err := LookupCRS(4326)
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 4326 {
		t.Fatalf("expected 4326, got %d", c.EPSG)
	}
	_, err = LookupCRS(99999)
	if !errors.Is(err, ErrUnknownCRS) {
		t.Fatalf("want ErrUnknownCRS, got %v", err)
	}
}

func TestLineStringCentroid_StraightSegment(t *testing.T) {
	l := LineString{Points: []Point{pt(0, 0), pt(10, 0)}}
	c := l.Centroid()
	if math.Abs(c.X-5) > 1e-9 || math.Abs(c.Y) > 1e-9 {
		t.Fatalf("centroid = %+v, want (5, 0)", c)
	}
}

func TestLineStringCentroid_LengthWeighted(t *testing.T) {
	// Two segments: (0,0)→(10,0) length 10, (10,0)→(11,0) length 1.
	// Midpoints: 5 and 10.5, weights 10 and 1.
	// x = (5*10 + 10.5*1) / 11 = 60.5 / 11 ≈ 5.5
	l := LineString{Points: []Point{pt(0, 0), pt(10, 0), pt(11, 0)}}
	c := l.Centroid()
	if math.Abs(c.X-5.5) > 1e-9 {
		t.Fatalf("length-weighted centroid X = %v, want 5.5", c.X)
	}
}

func TestMultiPointCentroid(t *testing.T) {
	m := MultiPoint{Points: []Point{pt(0, 0), pt(2, 0), pt(0, 4)}}
	c := m.Centroid()
	if math.Abs(c.X-2.0/3) > 1e-9 || math.Abs(c.Y-4.0/3) > 1e-9 {
		t.Fatalf("centroid = %+v", c)
	}
}

func TestMultiPolygonCentroid_AreaWeighted(t *testing.T) {
	// Two squares: 1x1 at (0,0) with centroid (0.5, 0.5), and 3x3 at (10,0)
	// with centroid (11.5, 1.5). Area weights: 1 and 9. Expected X:
	// (0.5*1 + 11.5*9) / 10 = 104/10 = 10.4. Expected Y: (0.5*1 + 1.5*9)/10 = 1.4.
	m := MultiPolygon{
		Polygons: []Polygon{
			SimplePolygon([]Point{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0)}, PseudoMercator),
			SimplePolygon([]Point{pt(10, 0), pt(13, 0), pt(13, 3), pt(10, 3), pt(10, 0)}, PseudoMercator),
		},
		CRSValue: PseudoMercator,
	}
	c := m.Centroid()
	if math.Abs(c.X-10.4) > 1e-6 || math.Abs(c.Y-1.4) > 1e-6 {
		t.Fatalf("area-weighted centroid = %+v, want (10.4, 1.4)", c)
	}
}

func TestCentroidDispatch(t *testing.T) {
	c := Centroid(Point{X: 3, Y: 4})
	if c.X != 3 || c.Y != 4 {
		t.Fatalf("point centroid: %+v", c)
	}
	c = Centroid(MultiPoint{Points: []Point{pt(0, 0), pt(2, 2)}})
	if c.X != 1 || c.Y != 1 {
		t.Fatalf("multipoint centroid: %+v", c)
	}
}

func TestArea_DispatchAndZeroForNonPolygonal(t *testing.T) {
	a, err := Area(LineString{Points: []Point{pt(0, 0), pt(1, 0)}, CRSValue: PseudoMercator}, UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	if a != 0 {
		t.Errorf("LineString area = %v, want 0", a)
	}

	poly := SimplePolygon([]Point{pt(0, 0), pt(2, 0), pt(2, 2), pt(0, 2), pt(0, 0)}, PseudoMercator)
	a, _ = Area(poly, UnitMeters)
	if math.Abs(a-4) > 1e-9 {
		t.Errorf("2x2 square area = %v, want 4", a)
	}
}

func TestLength_DispatchAndZeroForNonLinear(t *testing.T) {
	l, _ := Length(Point{X: 1, Y: 2, CRSValue: PseudoMercator}, UnitMeters)
	if l != 0 {
		t.Errorf("Point length = %v, want 0", l)
	}
	ls := LineString{Points: []Point{pt(0, 0), pt(3, 4)}, CRSValue: PseudoMercator}
	l, _ = Length(ls, UnitMeters)
	if math.Abs(l-5) > 1e-9 {
		t.Errorf("3-4-5 line length = %v, want 5", l)
	}
}

func TestMultiLineString_WKB_RoundTrip(t *testing.T) {
	m := MultiLineString{Lines: []LineString{
		{Points: []Point{pt(0, 0), pt(1, 1)}},
		{Points: []Point{pt(2, 2), pt(3, 3), pt(4, 5)}},
	}}
	back, err := ParseWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(MultiLineString)
	if len(got.Lines) != 2 || len(got.Lines[1].Points) != 3 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestMultiPolygon_WKB_RoundTrip(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0)}}},
		{Rings: [][]Point{
			{pt(10, 10), pt(20, 10), pt(20, 20), pt(10, 20), pt(10, 10)},
			{pt(13, 13), pt(17, 13), pt(17, 17), pt(13, 17), pt(13, 13)},
		}},
	}}
	back, err := ParseWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(MultiPolygon)
	if len(got.Polygons) != 2 {
		t.Fatalf("polygons: %d want 2", len(got.Polygons))
	}
	if len(got.Polygons[1].Rings) != 2 {
		t.Fatalf("second polygon rings: %d want 2", len(got.Polygons[1].Rings))
	}
}

func TestGeometryCollection_WKB_RoundTrip(t *testing.T) {
	gc := GeometryCollection{Geometries: []Geometry{
		Point{X: 1, Y: 2},
		LineString{Points: []Point{pt(0, 0), pt(1, 1)}},
		Polygon{Rings: [][]Point{{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0)}}},
	}}
	back, err := ParseWKB(WKB(gc))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(GeometryCollection)
	if len(got.Geometries) != 3 {
		t.Fatalf("collection: %d, want 3", len(got.Geometries))
	}
	if _, ok := got.Geometries[0].(Point); !ok {
		t.Fatalf("first element type: %T", got.Geometries[0])
	}
	if _, ok := got.Geometries[1].(LineString); !ok {
		t.Fatalf("second element type: %T", got.Geometries[1])
	}
	if _, ok := got.Geometries[2].(Polygon); !ok {
		t.Fatalf("third element type: %T", got.Geometries[2])
	}
}

func TestMultiLineString_WKT_Parse(t *testing.T) {
	g, err := ParseWKT("MULTILINESTRING ((0 0, 1 1), (2 2, 3 3, 4 5))")
	if err != nil {
		t.Fatal(err)
	}
	m := g.(MultiLineString)
	if len(m.Lines) != 2 || len(m.Lines[1].Points) != 3 {
		t.Fatalf("bad parse: %+v", m)
	}
}

func TestMultiPolygon_WKT_Parse(t *testing.T) {
	src := "MULTIPOLYGON (((0 0, 1 0, 1 1, 0 1, 0 0)), ((10 10, 20 10, 20 20, 10 20, 10 10), (13 13, 17 13, 17 17, 13 17, 13 13)))"
	g, err := ParseWKT(src)
	if err != nil {
		t.Fatal(err)
	}
	m := g.(MultiPolygon)
	if len(m.Polygons) != 2 {
		t.Fatalf("polygons: %d", len(m.Polygons))
	}
	if len(m.Polygons[1].Rings) != 2 {
		t.Fatalf("second polygon rings: %d", len(m.Polygons[1].Rings))
	}
}

func TestGeometryCollection_WKT_Parse(t *testing.T) {
	src := "GEOMETRYCOLLECTION (POINT (1 2), LINESTRING (0 0, 1 1))"
	g, err := ParseWKT(src)
	if err != nil {
		t.Fatal(err)
	}
	gc := g.(GeometryCollection)
	if len(gc.Geometries) != 2 {
		t.Fatalf("elements: %d", len(gc.Geometries))
	}
}

func TestMultiPolygon_Area_WithHole(t *testing.T) {
	// Two projected 10x10 squares, second one with a 4x4 hole = 100 + 84 = 184
	m := MultiPolygon{
		Polygons: []Polygon{
			SimplePolygon([]Point{pt(0, 0), pt(10, 0), pt(10, 10), pt(0, 10), pt(0, 0)}, PseudoMercator),
			{Rings: [][]Point{
				{pt(20, 20), pt(30, 20), pt(30, 30), pt(20, 30), pt(20, 20)},
				{pt(23, 23), pt(27, 23), pt(27, 27), pt(23, 27), pt(23, 23)},
			}, CRSValue: PseudoMercator},
		},
		CRSValue: PseudoMercator,
	}
	a, err := m.Area(UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	if a < 183.999 || a > 184.001 {
		t.Fatalf("multipolygon area = %v want ~184", a)
	}
}

func TestGeometryCollection_WKB_RejectsNested(t *testing.T) {
	inner := GeometryCollection{Geometries: []Geometry{Point{X: 1, Y: 2}}}
	outer := GeometryCollection{Geometries: []Geometry{inner}}
	buf := WKB(outer)
	if _, err := ParseWKB(buf); err == nil {
		t.Fatal("expected error decoding nested GeometryCollection")
	}
}

// ptZ is a shorthand for a 3D point.
func ptZ(x, y, z float64) Point { return Point{X: x, Y: y, Z: z, HasZ: true} }

func TestPointZ_WKB_RoundTrip(t *testing.T) {
	p := ptZ(1, 2, 3)
	back, err := ParseWKB(WKB(p))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(Point)
	if !got.HasZ {
		t.Fatal("HasZ not set on decoded Point")
	}
	if got.X != 1 || got.Y != 2 || got.Z != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestPointZ_WKB_LengthIs21(t *testing.T) {
	buf := WKB(ptZ(1, 2, 3))
	// 1 byte order + 4 type + 24 xyz = 29
	if len(buf) != 29 {
		t.Fatalf("WKB point Z length = %d, want 29", len(buf))
	}
	// Byte order little-endian
	if buf[0] != wkbNDR {
		t.Fatalf("byte order = %d", buf[0])
	}
	// Type = 1001 little-endian
	if buf[1] != 0xE9 || buf[2] != 0x03 || buf[3] != 0 || buf[4] != 0 {
		t.Fatalf("type bytes = %x %x %x %x", buf[1], buf[2], buf[3], buf[4])
	}
}

func TestLineStringZ_WKB_RoundTrip(t *testing.T) {
	l := LineString{Points: []Point{ptZ(0, 0, 10), ptZ(1, 1, 20)}, HasZ: true}
	back, err := ParseWKB(WKB(l))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(LineString)
	if !got.HasZ {
		t.Fatal("HasZ not set on decoded LineString")
	}
	if got.Points[0].Z != 10 || got.Points[1].Z != 20 {
		t.Fatalf("Z: %+v", got.Points)
	}
}

func TestPolygonZ_WKB_RoundTripWithHole(t *testing.T) {
	p := Polygon{
		Rings: [][]Point{
			{ptZ(0, 0, 5), ptZ(10, 0, 5), ptZ(10, 10, 5), ptZ(0, 10, 5), ptZ(0, 0, 5)},
			{ptZ(3, 3, 5), ptZ(7, 3, 5), ptZ(7, 7, 5), ptZ(3, 7, 5), ptZ(3, 3, 5)},
		},
		HasZ: true,
	}
	back, err := ParseWKB(WKB(p))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(Polygon)
	if !got.HasZ || len(got.Rings) != 2 {
		t.Fatalf("polygon Z round trip: %+v", got)
	}
	for _, ring := range got.Rings {
		for _, pt := range ring {
			if pt.Z != 5 {
				t.Fatalf("Z lost: %+v", pt)
			}
		}
	}
}

func TestMultiPointZ_WKB_RoundTrip(t *testing.T) {
	m := MultiPoint{Points: []Point{ptZ(1, 2, 3), ptZ(4, 5, 6)}, HasZ: true}
	back, err := ParseWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(MultiPoint)
	if !got.HasZ || len(got.Points) != 2 {
		t.Fatalf("multipoint Z: %+v", got)
	}
	if got.Points[1].Z != 6 {
		t.Fatalf("Z: %v", got.Points[1].Z)
	}
}

func TestMultiLineStringZ_WKB_RoundTrip(t *testing.T) {
	m := MultiLineString{
		Lines: []LineString{
			{Points: []Point{ptZ(0, 0, 1), ptZ(1, 1, 2)}, HasZ: true},
			{Points: []Point{ptZ(2, 2, 3), ptZ(3, 3, 4), ptZ(4, 4, 5)}, HasZ: true},
		},
		HasZ: true,
	}
	back, err := ParseWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(MultiLineString)
	if !got.HasZ || len(got.Lines) != 2 {
		t.Fatalf("multilinestring Z: %+v", got)
	}
	if got.Lines[1].Points[2].Z != 5 {
		t.Fatalf("Z: %v", got.Lines[1].Points[2].Z)
	}
}

func TestMultiPolygonZ_WKB_RoundTrip(t *testing.T) {
	m := MultiPolygon{
		Polygons: []Polygon{
			{Rings: [][]Point{{ptZ(0, 0, 1), ptZ(1, 0, 1), ptZ(1, 1, 1), ptZ(0, 1, 1), ptZ(0, 0, 1)}}, HasZ: true},
			{Rings: [][]Point{{ptZ(10, 10, 2), ptZ(20, 10, 2), ptZ(20, 20, 2), ptZ(10, 20, 2), ptZ(10, 10, 2)}}, HasZ: true},
		},
		HasZ: true,
	}
	back, err := ParseWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(MultiPolygon)
	if !got.HasZ || len(got.Polygons) != 2 {
		t.Fatalf("multipolygon Z: %+v", got)
	}
	if got.Polygons[1].Rings[0][0].Z != 2 {
		t.Fatalf("Z: %v", got.Polygons[1].Rings[0][0].Z)
	}
}

func TestGeometryCollectionZ_WKB_RoundTrip(t *testing.T) {
	gc := GeometryCollection{
		Geometries: []Geometry{
			ptZ(1, 2, 3),
			LineString{Points: []Point{ptZ(0, 0, 1), ptZ(1, 1, 2)}, HasZ: true},
		},
		HasZ: true,
	}
	back, err := ParseWKB(WKB(gc))
	if err != nil {
		t.Fatal(err)
	}
	got := back.(GeometryCollection)
	if !got.HasZ || len(got.Geometries) != 2 {
		t.Fatalf("gc Z: %+v", got)
	}
	if !got.Geometries[0].Is3D() || !got.Geometries[1].Is3D() {
		t.Fatalf("inner Is3D flags dropped")
	}
	if p, ok := got.Geometries[0].(Point); !ok || p.Z != 3 {
		t.Fatalf("inner point: %+v", got.Geometries[0])
	}
}

func TestParseWKT_PointZ(t *testing.T) {
	g, err := ParseWKT("POINT Z (1 2 3)")
	if err != nil {
		t.Fatal(err)
	}
	p := g.(Point)
	if !p.HasZ || p.X != 1 || p.Y != 2 || p.Z != 3 {
		t.Fatalf("parsed: %+v", p)
	}
}

func TestParseWKT_PolygonZWithHole(t *testing.T) {
	src := "POLYGON Z ((0 0 5, 10 0 5, 10 10 5, 0 10 5, 0 0 5), (3 3 5, 7 3 5, 7 7 5, 3 7 5, 3 3 5))"
	g, err := ParseWKT(src)
	if err != nil {
		t.Fatal(err)
	}
	p := g.(Polygon)
	if !p.HasZ || len(p.Rings) != 2 {
		t.Fatalf("poly Z: %+v", p)
	}
	for _, ring := range p.Rings {
		for _, pt := range ring {
			if pt.Z != 5 {
				t.Fatalf("Z lost: %+v", pt)
			}
		}
	}
}

func TestParseWKT_MultiPointZ(t *testing.T) {
	g, err := ParseWKT("MULTIPOINT Z ((1 2 3), (4 5 6))")
	if err != nil {
		t.Fatal(err)
	}
	m := g.(MultiPoint)
	if !m.HasZ || m.Points[1].Z != 6 {
		t.Fatalf("multipoint Z: %+v", m)
	}
}

func TestParseWKT_ZFlaggedButMissingZValueErrs(t *testing.T) {
	_, err := ParseWKT("POINT Z (1 2)")
	if !errors.Is(err, ErrInvalidWKT) {
		t.Fatalf("expected ErrInvalidWKT, got %v", err)
	}
}

func TestWKT_EmitsZQualifier(t *testing.T) {
	got := ptZ(1, 2, 3).WKT()
	if !strings.HasPrefix(got, "POINT Z ") {
		t.Fatalf("wkt = %q", got)
	}
	if !strings.Contains(got, " 3") {
		t.Fatalf("Z coord missing: %q", got)
	}
}

func Test2DAnd3DWKB_AreDifferent(t *testing.T) {
	a := WKB(Point{X: 1, Y: 2})
	b := WKB(ptZ(1, 2, 3))
	if len(a) == len(b) {
		t.Fatalf("2D and 3D WKB should differ in length; got %d and %d", len(a), len(b))
	}
}

func TestPoint_Distance3D(t *testing.T) {
	p := Point{X: 0, Y: 0, Z: 0, CRSValue: PseudoMercator, HasZ: true}
	q := Point{X: 3, Y: 4, Z: 12, CRSValue: PseudoMercator, HasZ: true}
	d, err := p.Distance3D(q, UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	// sqrt(9 + 16 + 144) = sqrt(169) = 13
	if math.Abs(d-13) > 1e-9 {
		t.Fatalf("Distance3D = %v, want 13", d)
	}
}

func TestPoint_Distance3D_RequiresBoth3D(t *testing.T) {
	p := Point{X: 0, Y: 0, CRSValue: PseudoMercator, HasZ: true}
	q := Point{X: 3, Y: 4, CRSValue: PseudoMercator}
	if _, err := p.Distance3D(q, UnitMeters); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
}

func TestPoint_Distance3D_GeographicDispatchesToECEF(t *testing.T) {
	// Pre-v0.4.7 behavior: geographic input errored with
	// ErrCRSMismatch. New behavior: dispatches to ECEF-Euclidean.
	// See CHANGELOG v0.4.7 for the migration note.
	p := Point{X: 0, Y: 0, Z: 0, CRSValue: WGS84, HasZ: true}
	q := Point{X: 0, Y: 0, Z: 100, CRSValue: WGS84, HasZ: true}
	got, err := p.Distance3D(q, UnitMeters)
	if err != nil {
		t.Fatalf("expected geographic dispatch to succeed, got %v", err)
	}
	// 100m altitude difference at the same lat/lon → ~100m ECEF
	// distance (sub-millimeter agreement).
	if math.Abs(got-100) > 0.001 {
		t.Errorf("100m altitude delta: got %v, want ~100", got)
	}
}

func TestProjectCarriesZ(t *testing.T) {
	p := ptZ(-73.9857, 40.7484, 100)
	p.CRSValue = WGS84
	out, err := Project(p, CRS{EPSG: 32618})
	if err != nil {
		t.Fatal(err)
	}
	pp := out.(Point)
	if !pp.HasZ || pp.Z != 100 {
		t.Fatalf("Z not carried through projection: %+v", pp)
	}
	// Round trip returns original Z.
	back, err := Project(pp, WGS84)
	if err != nil {
		t.Fatal(err)
	}
	pb := back.(Point)
	if !pb.HasZ || pb.Z != 100 {
		t.Fatalf("Z lost on round trip: %+v", pb)
	}
}

func TestIs3D_ReflectsHasZ(t *testing.T) {
	cases := []struct {
		name string
		g    Geometry
		want bool
	}{
		{"Point 2D", Point{X: 1, Y: 2}, false},
		{"Point 3D", ptZ(1, 2, 3), true},
		{"LineString 3D", LineString{Points: []Point{ptZ(0, 0, 0), ptZ(1, 1, 1)}, HasZ: true}, true},
		{"Polygon 2D", Polygon{Rings: [][]Point{{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 0, Y: 1}, {X: 0, Y: 0}}}}, false},
		{"MultiLineString 3D", MultiLineString{HasZ: true}, true},
		{"GeometryCollection 2D", GeometryCollection{}, false},
	}
	for _, c := range cases {
		if got := c.g.Is3D(); got != c.want {
			t.Errorf("%s Is3D = %v, want %v", c.name, got, c.want)
		}
	}
}

func Test2DGeometry_UnchangedBehaviour(t *testing.T) {
	// Ensures 2D constructions still WKT/WKB the same shape (no stray Z bytes).
	p := Point{X: 1, Y: 2}
	if p.WKT() != "POINT (1 2)" {
		t.Fatalf("2D WKT changed: %q", p.WKT())
	}
	if len(WKB(p)) != 21 {
		t.Fatalf("2D Point WKB should be 21 bytes, got %d", len(WKB(p)))
	}
}

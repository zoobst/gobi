package geometry

import (
	"errors"
	"math"
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
	d, err := Haversine(-73.9857, 40.7484, -0.1276, 51.5074, UnitKilometers)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, d, 5570, 20, "NYC→London")
}

func TestHaversine_UnitConsistency(t *testing.T) {
	km, _ := Haversine(0, 0, 1, 0, UnitKilometers)
	mi, _ := Haversine(0, 0, 1, 0, UnitMiles)
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

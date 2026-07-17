package geometry

import (
	"errors"
	"math"
	"strings"
	"testing"
)

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

func TestPoint_Distance3D_RequiresProjected(t *testing.T) {
	// Both geographic — should error.
	p := Point{X: 0, Y: 0, Z: 0, CRSValue: WGS84, HasZ: true}
	q := Point{X: 1, Y: 1, Z: 1, CRSValue: WGS84, HasZ: true}
	if _, err := p.Distance3D(q, UnitMeters); !errors.Is(err, ErrCRSMismatch) {
		t.Fatalf("expected ErrCRSMismatch, got %v", err)
	}
}

func TestProjectCarriesZ(t *testing.T) {
	p := ptZ(-73.9857, 40.7484, 100)
	p.CRSValue = WGS84
	out, err := Project(p, CRS{EPSG: 32618, Projected: true})
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

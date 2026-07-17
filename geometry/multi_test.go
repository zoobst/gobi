package geometry

import (
	"testing"
)

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

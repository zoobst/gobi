package kmlio_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/kmlio"
)

const citiesKML = `<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2">
  <Document>
    <Placemark>
      <name>New York</name>
      <description>Big Apple</description>
      <ExtendedData>
        <Data name="population"><value>8804190</value></Data>
      </ExtendedData>
      <Point><coordinates>-74.006,40.7128</coordinates></Point>
    </Placemark>
    <Placemark>
      <name>Central Park</name>
      <Polygon>
        <outerBoundaryIs>
          <LinearRing>
            <coordinates>-73.9819,40.7681 -73.9497,40.7681 -73.9497,40.8006 -73.9819,40.8006 -73.9819,40.7681</coordinates>
          </LinearRing>
        </outerBoundaryIs>
      </Polygon>
    </Placemark>
    <Placemark>
      <name>Broadway</name>
      <LineString>
        <coordinates>-73.98,40.75 -73.99,40.76 -74.00,40.77</coordinates>
      </LineString>
    </Placemark>
  </Document>
</kml>`

func TestRead_Cities(t *testing.T) {
	df, err := kmlio.Read(strings.NewReader(citiesKML), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := df.Shape()
	if rows != 3 {
		t.Fatalf("rows = %d, want 3", rows)
	}

	names, _ := df.Column("name")
	nameArr := names.Column().Data().Chunks()[0].(*array.String)
	want := []string{"New York", "Central Park", "Broadway"}
	for i, w := range want {
		if got := nameArr.Value(i); got != w {
			t.Errorf("row %d name = %q, want %q", i, got, w)
		}
	}

	// Row 0 → Point
	g, err := df.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("row 0 not a Point: %T", g)
	}
	if p.X > -74.005 || p.X < -74.007 {
		t.Fatalf("NYC lon = %v", p.X)
	}

	// Row 1 → Polygon with 5 vertices (closed ring).
	g, _ = df.Geometry("geometry", 1)
	poly, ok := g.(geometry.Polygon)
	if !ok {
		t.Fatalf("row 1 not a Polygon: %T", g)
	}
	if len(poly.Rings) != 1 || len(poly.Rings[0]) != 5 {
		t.Fatalf("row 1 rings: %+v", poly.Rings)
	}

	// Row 2 → LineString with 3 vertices.
	g, _ = df.Geometry("geometry", 2)
	line, ok := g.(geometry.LineString)
	if !ok {
		t.Fatalf("row 2 not a LineString: %T", g)
	}
	if len(line.Points) != 3 {
		t.Fatalf("row 2 points: %d", len(line.Points))
	}

	// ExtendedData column "population" should exist and hold "8804190" for row 0.
	pop, err := df.Column("population")
	if err != nil {
		t.Fatalf("population column missing: %v", err)
	}
	popArr := pop.Column().Data().Chunks()[0].(*array.String)
	if popArr.Value(0) != "8804190" {
		t.Fatalf("population[0] = %q, want 8804190", popArr.Value(0))
	}
	if !popArr.IsNull(1) || !popArr.IsNull(2) {
		t.Fatalf("population should be null for rows without ExtendedData")
	}
}

func TestRead_MultiGeometry(t *testing.T) {
	src := `<?xml version="1.0"?>
<kml xmlns="http://www.opengis.net/kml/2.2">
  <Placemark>
    <MultiGeometry>
      <Point><coordinates>1,2</coordinates></Point>
      <Point><coordinates>3,4</coordinates></Point>
      <Point><coordinates>5,6</coordinates></Point>
    </MultiGeometry>
  </Placemark>
</kml>`
	df, err := kmlio.Read(strings.NewReader(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := df.Geometry("geometry", 0)
	mp, ok := g.(geometry.MultiPoint)
	if !ok {
		t.Fatalf("expected MultiPoint, got %T", g)
	}
	if len(mp.Points) != 3 {
		t.Fatalf("points: %d", len(mp.Points))
	}
}

func TestWrite_RoundTripPreservesGeometry(t *testing.T) {
	// Build a frame with two rows: a Point and a Polygon.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"pt", "poly"}, nil)
	descB := array.NewStringBuilder(pool)
	defer descB.Release()
	descB.AppendValues([]string{"a point", "a polygon"}, nil)
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	pt := geometry.Point{X: 1.5, Y: 2.5, CRSValue: geometry.WGS84}
	poly := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}, geometry.WGS84)
	geomB.Append(geometry.WKB(pt))
	geomB.Append(geometry.WKB(poly))

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "description", Type: arrow.BinaryTypes.String, Nullable: true},
		gobi.GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), descB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 3)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := kmlio.Write(df, &buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "<Placemark>") {
		t.Fatalf("KML output missing <Placemark>:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "<Point>") {
		t.Fatal("KML output missing <Point>")
	}
	if !strings.Contains(buf.String(), "<Polygon>") {
		t.Fatal("KML output missing <Polygon>")
	}

	// Round-trip: read what we wrote and confirm geometries survive.
	back, err := kmlio.Read(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := back.Shape(); r != 2 {
		t.Fatalf("round-trip rows = %d, want 2", r)
	}
	g0, _ := back.Geometry("geometry", 0)
	if p, ok := g0.(geometry.Point); !ok || p.X != 1.5 || p.Y != 2.5 {
		t.Fatalf("Point round-trip: %+v", g0)
	}
	g1, _ := back.Geometry("geometry", 1)
	if p, ok := g1.(geometry.Polygon); !ok || len(p.Rings[0]) != 5 {
		t.Fatalf("Polygon round-trip: %+v", g1)
	}
}

func TestRead_MalformedErrors(t *testing.T) {
	// Non-<kml> root should return a clear error, not silently succeed.
	if _, err := kmlio.Read(strings.NewReader(`<not-kml/>`), nil); err == nil {
		t.Fatal("expected error for non-KML root, got nil")
	}
	// Broken XML should also error.
	if _, err := kmlio.Read(strings.NewReader(`<kml><Placemark>`), nil); err == nil {
		t.Fatal("expected error for unterminated XML, got nil")
	}
}

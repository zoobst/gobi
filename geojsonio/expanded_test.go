package geojsonio_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geojsonio"
	"github.com/zoobst/gobi/geometry"
)

// -----------------------------------------------------------------------------
// Geometry-type coverage — every RFC 7946 §3.1 shape round-trips.
// -----------------------------------------------------------------------------

func TestMultiLineStringRoundTrip(t *testing.T) {
	m := geometry.MultiLineString{
		Lines: []geometry.LineString{
			{Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 0}}},
			{Points: []geometry.Point{{X: 5, Y: 5}, {X: 6, Y: 6}, {X: 7, Y: 5}}},
		},
		CRSValue: geometry.WGS84,
	}
	buf, err := geojsonio.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.(geometry.MultiLineString)
	if !ok {
		t.Fatalf("expected MultiLineString, got %T", back)
	}
	if len(got.Lines) != 2 || len(got.Lines[0].Points) != 2 || len(got.Lines[1].Points) != 3 {
		t.Fatalf("shape lost in round-trip: %+v", got)
	}
}

func TestMultiPolygonRoundTrip(t *testing.T) {
	square := [][]geometry.Point{{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}}
	m := geometry.MultiPolygon{
		Polygons: []geometry.Polygon{
			{Rings: square},
			{Rings: square},
		},
		CRSValue: geometry.WGS84,
	}
	buf, err := geojsonio.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.(geometry.MultiPolygon)
	if !ok {
		t.Fatalf("expected MultiPolygon, got %T", back)
	}
	if len(got.Polygons) != 2 || len(got.Polygons[0].Rings) != 1 || len(got.Polygons[0].Rings[0]) != 5 {
		t.Fatalf("shape lost in round-trip: %+v", got)
	}
}

func TestGeometryCollectionRoundTrip(t *testing.T) {
	c := geometry.GeometryCollection{
		Geometries: []geometry.Geometry{
			geometry.Point{X: 1, Y: 2},
			geometry.LineString{Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		},
		CRSValue: geometry.WGS84,
	}
	buf, err := geojsonio.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.(geometry.GeometryCollection)
	if !ok {
		t.Fatalf("expected GeometryCollection, got %T", back)
	}
	if len(got.Geometries) != 2 {
		t.Fatalf("geometry count = %d, want 2", len(got.Geometries))
	}
	if _, ok := got.Geometries[0].(geometry.Point); !ok {
		t.Errorf("member 0: expected Point, got %T", got.Geometries[0])
	}
	if _, ok := got.Geometries[1].(geometry.LineString); !ok {
		t.Errorf("member 1: expected LineString, got %T", got.Geometries[1])
	}
}

// -----------------------------------------------------------------------------
// XYZ coordinate support — 3-element position arrays round-trip.
// -----------------------------------------------------------------------------

func TestPointXYZRoundTrip(t *testing.T) {
	p := geometry.NewPointZ(1, 2, 3, geometry.WGS84)
	buf, err := geojsonio.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// Marshaled coordinates should be a 3-element array.
	if !strings.Contains(string(buf), `[1,2,3]`) {
		t.Fatalf("expected 3-elem coords in output: %s", buf)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(geometry.Point)
	if got.X != 1 || got.Y != 2 || got.Z != 3 || !got.HasZ {
		t.Fatalf("XYZ point lost: %+v", got)
	}
}

func TestLineStringXYZRoundTrip(t *testing.T) {
	l := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, Z: 10, HasZ: true},
			{X: 1, Y: 1, Z: 20, HasZ: true},
		},
		HasZ:     true,
		CRSValue: geometry.WGS84,
	}
	buf, err := geojsonio.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(geometry.LineString)
	if !got.HasZ {
		t.Errorf("HasZ lost")
	}
	if got.Points[0].Z != 10 || got.Points[1].Z != 20 {
		t.Errorf("Z values lost: %+v", got.Points)
	}
}

// -----------------------------------------------------------------------------
// Frame-level I/O — ReadFile / WriteFile round-trip.
// -----------------------------------------------------------------------------

// buildFeatureFrame — 3-row Frame with a geometry column + name +
// population properties. Shared across the Frame-I/O tests.
func buildFeatureFrame(t *testing.T) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"a", "b", "c"}, nil)

	popB := array.NewInt64Builder(pool)
	defer popB.Release()
	popB.AppendValues([]int64{10, 20, 30}, nil)

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, pt := range []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 2}} {
		geomB.Append(geometry.WKB(pt))
	}

	fields := []arrow.Field{
		gobi.GeometryField("geometry", 4326),
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "population", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{geomB.NewArray(), nameB.NewArray(), popB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFrame_WriteAndRead_FeatureCollection(t *testing.T) {
	df := buildFeatureFrame(t)
	path := filepath.Join(t.TempDir(), "out.geojson")

	if err := geojsonio.WriteFile(df, path, &geojsonio.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := geojsonio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got.NumRows() != 3 {
		t.Fatalf("rows = %d, want 3", got.NumRows())
	}
	// Property columns: name (String) + population (Int64).
	nameS, err := got.Column("name")
	if err != nil {
		t.Fatal(err)
	}
	nameArr := nameS.Column().Data().Chunks()[0].(*array.String)
	if nameArr.Value(0) != "a" || nameArr.Value(2) != "c" {
		t.Errorf("name values wrong: %v %v", nameArr.Value(0), nameArr.Value(2))
	}
	popS, err := got.Column("population")
	if err != nil {
		t.Fatal(err)
	}
	popArr := popS.Column().Data().Chunks()[0].(*array.Int64)
	if popArr.Value(0) != 10 || popArr.Value(2) != 30 {
		t.Errorf("population values wrong: %v %v", popArr.Value(0), popArr.Value(2))
	}
	// Geometry column preserved as WKB Binary + geometry-tagged field.
	geomS, err := got.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	if !geomS.IsGeometry() {
		t.Errorf("geometry column lost its tag")
	}
	ba := geomS.Column().Data().Chunks()[0].(*array.Binary)
	g, err := geometry.ParseWKB(ba.Value(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(geometry.Point); !ok {
		t.Errorf("row 0 geom: expected Point, got %T", g)
	}
}

func TestFrame_WriteAndRead_LineDelimited(t *testing.T) {
	df := buildFeatureFrame(t)
	// .geojsonl extension triggers FormatLineDelimited via
	// FormatAuto.
	path := filepath.Join(t.TempDir(), "out.geojsonl")
	if err := geojsonio.WriteFile(df, path, &geojsonio.WriteOptions{
		Format: geojsonio.FormatLineDelimited,
	}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := geojsonio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got.NumRows() != 3 {
		t.Fatalf("rows = %d, want 3", got.NumRows())
	}
}

// -----------------------------------------------------------------------------
// LazyFrame ScanFile
// -----------------------------------------------------------------------------

func TestScanFile_ProjectionAboveScan(t *testing.T) {
	df := buildFeatureFrame(t)
	path := filepath.Join(t.TempDir(), "scan.geojson")
	if err := geojsonio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	out, err := geojsonio.ScanFile(path, nil).
		Select(gobi.Col("name")).
		Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	names := out.ColumnNames()
	if len(names) != 1 || names[0] != "name" {
		t.Fatalf("cols = %v, want [name]", names)
	}
}

func TestScanFile_ExplainLabel(t *testing.T) {
	df := buildFeatureFrame(t)
	path := filepath.Join(t.TempDir(), "explain.geojson")
	if err := geojsonio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	lf := geojsonio.ScanFile(path, nil).
		Filter(gobi.Col("population").Gt(gobi.Lit(int64(15))))
	explain := lf.ExplainPhysical()
	if !strings.Contains(explain, "Scan[geojson]") {
		t.Fatalf("explain missing Scan[geojson] label:\n%s", explain)
	}
}

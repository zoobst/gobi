package shpio_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/shpio"
)

// buildFrame constructs a small frame with a name column, a population
// column, and a Point geometry column. Used as the input to write-then-read
// round trips.
func buildPointFrame(t *testing.T) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"NYC", "LA", "CHI"}, nil)
	popB := array.NewInt64Builder(pool)
	defer popB.Release()
	popB.AppendValues([]int64{8804190, 3898747, 2746388}, nil)
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	pts := []geometry.Point{
		{X: -74.006, Y: 40.7128, CRSValue: geometry.WGS84},
		{X: -118.2437, Y: 34.0522, CRSValue: geometry.WGS84},
		{X: -87.6298, Y: 41.8781, CRSValue: geometry.WGS84},
	}
	for _, p := range pts {
		geomB.Append(geometry.WKB(p))
	}
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "population", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		gobi.GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), popB.NewArray(), geomB.NewArray()}
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
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWriteRead_PointRoundTrip(t *testing.T) {
	df := buildPointFrame(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "cities")
	if err := shpio.WriteFile(df, base); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Confirm the four sidecars all landed.
	for _, ext := range []string{".shp", ".shx", ".dbf", ".prj"} {
		if _, err := os.Stat(base + ext); err != nil {
			t.Errorf("missing %s: %v", ext, err)
		}
	}

	back, err := shpio.ReadFile(base)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if r, _ := back.Shape(); r != 3 {
		t.Fatalf("round-trip rows = %d, want 3", r)
	}

	names, err := back.Column("NAME")
	if err != nil {
		t.Fatal(err)
	}
	nameArr := names.Column().Data().Chunks()[0].(*array.String)
	want := []string{"NYC", "LA", "CHI"}
	for i, w := range want {
		if got := nameArr.Value(i); got != w {
			t.Errorf("row %d name = %q, want %q", i, got, w)
		}
	}

	pops, err := back.Column("POPULATION")
	if err != nil {
		t.Fatal(err)
	}
	popArr := pops.Column().Data().Chunks()[0].(*array.Float64)
	if popArr.Value(0) != 8804190 {
		t.Fatalf("row 0 pop = %v, want 8804190", popArr.Value(0))
	}

	g, err := back.Geometry("geometry", 0)
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
}

func TestWriteRead_PolygonRoundTrip(t *testing.T) {
	// A polygon with one hole. Shapefile requires exterior=CW / interior=CCW;
	// the writer will re-orient rings as needed and the reader will
	// re-group them.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"square-with-hole"}, nil)
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	poly := geometry.Polygon{
		Rings: [][]geometry.Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 3, Y: 3}, {X: 7, Y: 3}, {X: 7, Y: 7}, {X: 3, Y: 7}, {X: 3, Y: 3}},
		},
		CRSValue: geometry.WGS84,
	}
	geomB.Append(geometry.WKB(poly))
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		gobi.GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	base := filepath.Join(dir, "poly")
	if err := shpio.WriteFile(df, base); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := shpio.ReadFile(base)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	g, err := back.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	pp, ok := g.(geometry.Polygon)
	if !ok {
		t.Fatalf("round-trip type: %T", g)
	}
	if len(pp.Rings) != 2 {
		t.Fatalf("round-trip rings = %d, want 2", len(pp.Rings))
	}
	if len(pp.Rings[0]) != 5 || len(pp.Rings[1]) != 5 {
		t.Fatalf("ring lengths: %v", []int{len(pp.Rings[0]), len(pp.Rings[1])})
	}
}

func TestWriteRead_LineStringRoundTrip(t *testing.T) {
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"line"}, nil)
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	line := geometry.LineString{
		Points:   []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 3}},
		CRSValue: geometry.WGS84,
	}
	geomB.Append(geometry.WKB(line))
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		gobi.GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, _ := gobi.NewFrame(schema, cols)

	dir := t.TempDir()
	base := filepath.Join(dir, "line")
	if err := shpio.WriteFile(df, base); err != nil {
		t.Fatal(err)
	}
	back, err := shpio.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := back.Geometry("geometry", 0)
	ls, ok := g.(geometry.LineString)
	if !ok {
		t.Fatalf("round-trip type: %T", g)
	}
	if len(ls.Points) != 3 {
		t.Fatalf("point count: %d", len(ls.Points))
	}
	if ls.Points[2].X != 2 || ls.Points[2].Y != 3 {
		t.Fatalf("last vertex: %+v", ls.Points[2])
	}
}

func TestReadFile_MissingSHPErrors(t *testing.T) {
	_, err := shpio.ReadFile(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing .shp")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

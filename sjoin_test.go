package gobi

import (
	"testing"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// pointsFrame builds a frame of (name string, geometry Binary/WKB Point).
func pointsFrame(t *testing.T, names []string, pts []geometry.Point) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues(names, nil)

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, p := range pts {
		geomB.Append(geometry.WKB(p))
	}

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// polygonsFrame builds a frame of (region string, geometry Binary/WKB Polygon).
func polygonsFrame(t *testing.T, regions []string, polys []geometry.Polygon) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues(regions, nil)

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, p := range polys {
		geomB.Append(geometry.WKB(p))
	}

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// unitSquare returns a projected polygon of extent 2r centered at (cx, cy).
func unitSquare(cx, cy, r float64) geometry.Polygon {
	return geometry.SimplePolygon([]geometry.Point{
		{X: cx - r, Y: cy - r}, {X: cx + r, Y: cy - r},
		{X: cx + r, Y: cy + r}, {X: cx - r, Y: cy + r},
		{X: cx - r, Y: cy - r},
	}, geometry.WGS84)
}

func TestSJoin_Intersects_PointsInPolygons(t *testing.T) {
	// 3 regions: NW quadrant, NE quadrant, disjoint. 4 points:
	// (0,0) inside NW+NE both (they share the edge; expect 2 matches)
	// (-5, 5) NW only
	// (5, 5)  NE only
	// (100, 100) no match
	regions := polygonsFrame(t,
		[]string{"NW", "NE", "faraway"},
		[]geometry.Polygon{
			unitSquare(-5, 5, 5),   // NW: covers x in [-10, 0], y in [0, 10]
			unitSquare(5, 5, 5),    // NE: covers x in [0, 10],  y in [0, 10]
			unitSquare(1000, 0, 1),
		},
	)
	points := pointsFrame(t,
		[]string{"origin", "left", "right", "far"},
		[]geometry.Point{{X: 0, Y: 5}, {X: -5, Y: 5}, {X: 5, Y: 5}, {X: 100, Y: 100}},
	)
	out, err := points.SJoin(regions, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatal(err)
	}

	// Expected matches: origin ↔ NW, origin ↔ NE, left ↔ NW, right ↔ NE (4 rows).
	if out.NumRows() != 4 {
		t.Fatalf("row count = %d, want 4", out.NumRows())
	}

	nameCol, _ := out.Column("name")
	regionCol, _ := out.Column("region")
	nameArr := nameCol.col.Data().Chunks()[0].(*array.String)
	regionArr := regionCol.col.Data().Chunks()[0].(*array.String)

	got := map[string]bool{}
	for i := 0; i < out.NumRows(); i++ {
		got[nameArr.Value(i)+"|"+regionArr.Value(i)] = true
	}
	for _, want := range []string{
		"origin|NW", "origin|NE", "left|NW", "right|NE",
	} {
		if !got[want] {
			t.Errorf("missing match %q; got %v", want, got)
		}
	}
	if got["far|faraway"] {
		t.Error("distant point should not match any region")
	}
}

func TestSJoin_Within_PointInPolygon(t *testing.T) {
	// Same layout as above; using SPWithin. Points are within polygons that
	// contain them — same match set as SPIntersects for these primitive
	// points (a point is either inside/on a polygon or not).
	regions := polygonsFrame(t,
		[]string{"NW", "NE"},
		[]geometry.Polygon{unitSquare(-5, 5, 5), unitSquare(5, 5, 5)},
	)
	points := pointsFrame(t,
		[]string{"left", "right"},
		[]geometry.Point{{X: -5, Y: 5}, {X: 5, Y: 5}},
	)
	out, err := points.SJoin(regions, "geometry", "geometry", SPWithin)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("row count = %d, want 2", out.NumRows())
	}
}

func TestSJoin_Contains_PolygonPolygon(t *testing.T) {
	// Big polygons contain nested small ones.
	outer := polygonsFrame(t,
		[]string{"big", "unrelated"},
		[]geometry.Polygon{
			unitSquare(0, 0, 100),
			unitSquare(1000, 1000, 5),
		},
	)
	inner := polygonsFrame(t,
		[]string{"a", "b", "outside"},
		[]geometry.Polygon{
			unitSquare(0, 0, 10),
			unitSquare(50, 50, 3),
			unitSquare(1500, 1500, 1),
		},
	)
	out, err := outer.SJoin(inner, "geometry", "geometry", SPContains)
	if err != nil {
		t.Fatal(err)
	}
	// big should contain a and b (2 matches); unrelated should contain none.
	if out.NumRows() != 2 {
		t.Fatalf("row count = %d, want 2", out.NumRows())
	}
	regionArr := getStringColumn(t, out, "region")
	// After join the LEFT geometry column stays; the RIGHT geometry column
	// is dropped. Left is "outer" — its label column is "region", right's is
	// "region_right" via collision rename.
	regionRightArr := getStringColumn(t, out, "region_right")
	pairs := map[string]bool{}
	for i := 0; i < out.NumRows(); i++ {
		pairs[regionArr.Value(i)+"→"+regionRightArr.Value(i)] = true
	}
	for _, want := range []string{"big→a", "big→b"} {
		if !pairs[want] {
			t.Errorf("missing match %q; got %v", want, pairs)
		}
	}
}

func TestSJoin_NoMatches(t *testing.T) {
	a := pointsFrame(t, []string{"x"}, []geometry.Point{{X: 0, Y: 0}})
	b := polygonsFrame(t, []string{"far"}, []geometry.Polygon{unitSquare(1000, 1000, 5)})
	out, err := a.SJoin(b, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("expected 0 rows, got %d", out.NumRows())
	}
	// Column shape: left keeps name + geometry; right keeps region.
	if out.NumCols() != 3 {
		t.Fatalf("cols = %d, want 3", out.NumCols())
	}
}

func TestSJoin_ColumnShape(t *testing.T) {
	left := pointsFrame(t, []string{"p"}, []geometry.Point{{X: 0, Y: 0}})
	right := polygonsFrame(t, []string{"r"}, []geometry.Polygon{unitSquare(0, 0, 5)})
	out, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatal(err)
	}
	// Columns should be: name, geometry (left kept), region (right kept, no
	// clash so no rename).
	names := out.ColumnNames()
	if len(names) != 3 || names[0] != "name" || names[1] != "geometry" || names[2] != "region" {
		t.Fatalf("column names = %v", names)
	}
}

func TestSJoin_ErrorsOnNonGeometryColumn(t *testing.T) {
	left := pointsFrame(t, []string{"p"}, []geometry.Point{{X: 0, Y: 0}})
	right := polygonsFrame(t, []string{"r"}, []geometry.Polygon{unitSquare(0, 0, 5)})
	if _, err := left.SJoin(right, "name", "geometry", SPIntersects); err == nil {
		t.Fatal("expected error when left column is not a geometry column")
	}
	if _, err := left.SJoin(right, "geometry", "region", SPIntersects); err == nil {
		t.Fatal("expected error when right column is not a geometry column")
	}
	if _, err := left.SJoin(right, "nope", "geometry", SPIntersects); err == nil {
		t.Fatal("expected error when left column is missing")
	}
}

// getStringColumn is a small helper to unwrap a string-typed column from a
// single-chunk frame for assertion.
func getStringColumn(t *testing.T, f *Frame, name string) *array.String {
	t.Helper()
	s, err := f.Column(name)
	if err != nil {
		t.Fatal(err)
	}
	return s.col.Data().Chunks()[0].(*array.String)
}

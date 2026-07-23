package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// mixedGeomFrame builds a small frame with a name column and a geometry
// column mixing single- and multi-part geometries plus a null row.
func mixedGeomFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	// Include a null in the geometry column at row index 2.
	nameB.AppendValues([]string{"solo", "multi", "null", "collection"}, nil)

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	geomB.Append(geometry.WKB(geometry.Point{X: 1, Y: 1}))
	geomB.Append(geometry.WKB(geometry.MultiPoint{
		Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 2, Y: 0}},
	}))
	geomB.AppendNull()
	geomB.Append(geometry.WKB(geometry.GeometryCollection{
		Geometries: []geometry.Geometry{
			geometry.Point{X: 5, Y: 5},
			geometry.LineString{Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		},
	}))

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
	cols := make([]arrow.Column, 2)
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

func TestExplode_ExpandsMultisAndDuplicatesAttrs(t *testing.T) {
	f := mixedGeomFrame(t)
	out, err := f.Explode("geometry")
	if err != nil {
		t.Fatal(err)
	}
	// 1 (solo) + 3 (multi-point components) + 1 (null passthrough) + 2
	// (collection components) = 7 rows.
	if got := out.NumRows(); got != 7 {
		t.Fatalf("exploded row count = %d, want 7", got)
	}

	nameCol, _ := out.Column("name")
	nameArr := nameCol.col.Data().Chunks()[0].(*array.String)
	want := []string{"solo", "multi", "multi", "multi", "null", "collection", "collection"}
	for i, w := range want {
		if got := nameArr.Value(i); got != w {
			t.Fatalf("row %d name = %q, want %q", i, got, w)
		}
	}
}

func TestExplode_NullRowRetained(t *testing.T) {
	f := mixedGeomFrame(t)
	out, _ := f.Explode("geometry")
	geomCol, _ := out.Column("geometry")
	bin := geomCol.col.Data().Chunks()[0].(*array.Binary)
	if !bin.IsNull(4) {
		t.Fatalf("null geometry row should have been kept as null; got %v", bin.Value(4))
	}
}

func TestExplode_ComponentTypesCorrect(t *testing.T) {
	f := mixedGeomFrame(t)
	out, _ := f.Explode("geometry")
	geomCol, _ := out.Column("geometry")

	// Row 0: original Point.
	g, err := geomCol.Geometry(0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Type() != geometry.TypePoint {
		t.Fatalf("row 0 type = %s, want Point", g.Type())
	}
	// Rows 1-3: exploded multipoint components (individual Points).
	for i := 1; i <= 3; i++ {
		g, _ := geomCol.Geometry(i)
		if g.Type() != geometry.TypePoint {
			t.Fatalf("row %d type = %s, want Point", i, g.Type())
		}
	}
	// Row 5: first collection component (Point).
	g, _ = geomCol.Geometry(5)
	if g.Type() != geometry.TypePoint {
		t.Fatalf("row 5 type = %s, want Point", g.Type())
	}
	// Row 6: second collection component (LineString).
	g, _ = geomCol.Geometry(6)
	if g.Type() != geometry.TypeLineString {
		t.Fatalf("row 6 type = %s, want LineString", g.Type())
	}
}

func TestExplode_NonGeometryColumnErrors(t *testing.T) {
	f := mixedGeomFrame(t)
	_, err := f.Explode("name")
	if !errors.Is(err, ErrNotGeometry) {
		t.Fatalf("expected ErrNotGeometry, got %v", err)
	}
}

// listExplodeFrame builds a small frame with an id column and a
// List<Int64> tags column mixing non-empty, empty, and null lists.
func listExplodeFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3, 4}, nil)

	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.Int64Builder)
	// Row 0: [10, 20, 30]
	lb.Append(true)
	vb.AppendValues([]int64{10, 20, 30}, nil)
	// Row 1: [40]
	lb.Append(true)
	vb.Append(40)
	// Row 2: null list
	lb.AppendNull()
	// Row 3: [] (empty non-null)
	lb.Append(true)

	idArr := idB.NewArray()
	defer idArr.Release()
	tagsArr := lb.NewArray()
	defer tagsArr.Release()

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "tags", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(tagsArr.DataType(), []arrow.Array{tagsArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExplode_ListColumn(t *testing.T) {
	f := listExplodeFrame(t)
	out, err := f.Explode("tags")
	if err != nil {
		t.Fatal(err)
	}
	// Row 0 → 3, row 1 → 1, row 2 (null) → 1 null, row 3 ([]) → 1 null.
	if got := out.NumRows(); got != 6 {
		t.Fatalf("row count = %d, want 6", got)
	}
	tagsS, _ := out.Column("tags")
	if tagsS.DataType().ID() != arrow.INT64 {
		t.Fatalf("exploded tags type = %s, want INT64", tagsS.DataType())
	}
	tagsArr := tagsS.col.Data().Chunks()[0].(*array.Int64)
	// Values: 10, 20, 30, 40, null, null
	wantVal := []int64{10, 20, 30, 40}
	for i, w := range wantVal {
		if tagsArr.IsNull(i) || tagsArr.Value(i) != w {
			t.Fatalf("tags row %d = %d (null=%v), want %d", i, tagsArr.Value(i), tagsArr.IsNull(i), w)
		}
	}
	if !tagsArr.IsNull(4) || !tagsArr.IsNull(5) {
		t.Fatalf("tags rows 4,5 should be null: %v %v", tagsArr.IsNull(4), tagsArr.IsNull(5))
	}
	// id column duplicated: 1, 1, 1, 2, 3, 4
	idArr := out.series[0].col.Data().Chunks()[0].(*array.Int64)
	wantID := []int64{1, 1, 1, 2, 3, 4}
	for i, w := range wantID {
		if idArr.Value(i) != w {
			t.Fatalf("id row %d = %d, want %d", i, idArr.Value(i), w)
		}
	}
}

func TestExplode_MissingColumnErrors(t *testing.T) {
	f := mixedGeomFrame(t)
	_, err := f.Explode("nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("expected ErrColumnNotFound, got %v", err)
	}
}

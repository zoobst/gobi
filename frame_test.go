package gobi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi/geometry"
)

// buildFrame builds a small frame with (name string, pop int64, geometry WKB-Point).
func buildFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.NewGoAllocator()

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "pop", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)

	names := array.NewStringBuilder(pool)
	defer names.Release()
	names.AppendValues([]string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}, nil)

	pops := array.NewInt64Builder(pool)
	defer pops.Release()
	pops.AppendValues([]int64{1, 2, 3, 4, 5}, nil)

	geoms := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geoms.Release()
	for i, x := range []float64{0, 1, 2, 3, 4} {
		wkb := geometry.WKB(geometry.Point{X: x, Y: float64(i * 10)})
		geoms.Append(wkb)
	}

	arrays := []arrow.Array{names.NewArray(), pops.NewArray(), geoms.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()

	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}

	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFrame_Shape(t *testing.T) {
	f := buildFrame(t)
	rows, cols := f.Shape()
	if rows != 5 || cols != 3 {
		t.Fatalf("shape got (%d, %d) want (5, 3)", rows, cols)
	}
}

func TestFrame_ColumnNames(t *testing.T) {
	f := buildFrame(t)
	names := f.ColumnNames()
	if len(names) != 3 || names[0] != "name" || names[2] != "geometry" {
		t.Fatalf("column names: %v", names)
	}
}

func TestFrame_HeadTail(t *testing.T) {
	f := buildFrame(t)
	head := f.Head(2)
	if head.NumRows() != 2 {
		t.Fatalf("head rows = %d want 2", head.NumRows())
	}
	tail := f.Tail(2)
	if tail.NumRows() != 2 {
		t.Fatalf("tail rows = %d want 2", tail.NumRows())
	}
	// Tail should include the last row's population = 5
	pops, _ := tail.Column("pop")
	pRow, _ := pops.Row(pops.Len() - 1)
	// grab the actual int64 through the chunk
	chunk := pRow.Column().Data().Chunks()[0].(*array.Int64)
	if chunk.Value(0) != 5 {
		t.Fatalf("tail last pop = %d want 5", chunk.Value(0))
	}
}

func TestFrame_HeadDefaultAndOverflow(t *testing.T) {
	f := buildFrame(t)
	if f.Head(0).NumRows() != 5 { // default is 5, table has 5 rows
		t.Fatal("Head(0) default should equal min(5, rows)")
	}
	if f.Head(100).NumRows() != 5 { // clamp to available rows
		t.Fatal("Head(100) should be clamped to available rows")
	}
}

func TestFrame_ColumnNotFound(t *testing.T) {
	f := buildFrame(t)
	_, err := f.Column("nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestFrame_RowOutOfRange(t *testing.T) {
	f := buildFrame(t)
	_, err := f.Row(99)
	if !errors.Is(err, ErrRowOutOfRange) {
		t.Fatalf("want ErrRowOutOfRange, got %v", err)
	}
}

func TestSeries_GeometryDecode(t *testing.T) {
	f := buildFrame(t)
	g, err := f.Geometry("geometry", 2)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("got %T, want Point", g)
	}
	if p.X != 2 || p.Y != 20 {
		t.Fatalf("point = %+v want (2, 20)", p)
	}
}

func TestSeries_NotGeometry(t *testing.T) {
	f := buildFrame(t)
	s, _ := f.Column("name")
	_, err := s.Geometry(0)
	if !errors.Is(err, ErrNotGeometry) {
		t.Fatalf("want ErrNotGeometry, got %v", err)
	}
}

// makeUint64Series is a helper: builds a single-chunk Uint64 Series with
// the given values. Useful for simulating an H3-cell UDF result.
func makeUint64Series(t *testing.T, name string, vals []uint64) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewUint64Builder(pool)
	defer b.Release()
	b.AppendValues(vals, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Uint64, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked))
}

func TestFrame_WithColumn_Append(t *testing.T) {
	f := buildFrame(t)
	// H3-style derived column: 5 uint64 cells, one per row.
	h3 := makeUint64Series(t, "h3", []uint64{100, 101, 102, 103, 104})

	out, err := f.WithColumn("h3", h3)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 4 {
		t.Fatalf("cols = %d, want 4", got)
	}
	if got := out.NumRows(); got != 5 {
		t.Fatalf("rows = %d, want 5", got)
	}
	// The original frame is unchanged.
	if f.NumCols() != 3 {
		t.Fatalf("source frame mutated: cols = %d, want 3", f.NumCols())
	}
	// New column is last.
	names := out.ColumnNames()
	if names[len(names)-1] != "h3" {
		t.Fatalf("last col = %q, want h3", names[len(names)-1])
	}
	// Values survived.
	col, err := out.Column("h3")
	if err != nil {
		t.Fatal(err)
	}
	arr := col.Column().Data().Chunks()[0].(*array.Uint64)
	if arr.Value(0) != 100 || arr.Value(4) != 104 {
		t.Fatalf("unexpected h3 values: %d..%d", arr.Value(0), arr.Value(4))
	}
}

func TestFrame_WithColumn_Replace(t *testing.T) {
	f := buildFrame(t)
	// Replace the existing "pop" column with a derived one.
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues([]int64{10, 20, 30, 40, 50}, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "pop", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	newPop := NewSeries(arrow.NewColumn(field, chunked))

	out, err := f.WithColumn("pop", newPop)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 3 {
		t.Fatalf("cols = %d, want 3 (replaced, not appended)", got)
	}
	// The order must be preserved.
	names := out.ColumnNames()
	if names[0] != "name" || names[1] != "pop" || names[2] != "geometry" {
		t.Fatalf("column order broken: %v", names)
	}
	// New values in the "pop" column.
	col, _ := out.Column("pop")
	chunk := col.Column().Data().Chunks()[0].(*array.Int64)
	if chunk.Value(2) != 30 {
		t.Fatalf("row 2 pop = %d, want 30", chunk.Value(2))
	}
}

func TestFrame_WithColumn_LenMismatch(t *testing.T) {
	f := buildFrame(t)
	// Only 3 rows — mismatch with the 5-row frame.
	short := makeUint64Series(t, "h3", []uint64{1, 2, 3})
	if _, err := f.WithColumn("h3", short); !errors.Is(err, ErrColumnLenMismatch) {
		t.Fatalf("want ErrColumnLenMismatch, got %v", err)
	}
}

func TestFrame_DropColumn(t *testing.T) {
	f := buildFrame(t)
	out, err := f.DropColumn("pop")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 2 {
		t.Fatalf("cols = %d, want 2", got)
	}
	names := out.ColumnNames()
	if names[0] != "name" || names[1] != "geometry" {
		t.Fatalf("cols = %v, want [name geometry]", names)
	}
	if _, err := out.Column("pop"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("dropped column still queryable: %v", err)
	}
	// Original untouched.
	if _, err := f.Column("pop"); err != nil {
		t.Fatalf("source frame lost pop: %v", err)
	}
}

func TestFrame_DropColumn_Missing(t *testing.T) {
	f := buildFrame(t)
	if _, err := f.DropColumn("does_not_exist"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestFrame_WithColumn_PreservesSchemaMetadata(t *testing.T) {
	// Build a frame whose schema carries file-level metadata (e.g. a
	// GeoParquet "geo" key). WithColumn must not drop it.
	f := buildFrame(t)
	md := arrow.NewMetadata([]string{"geo"}, []string{`{"primary_column":"geometry"}`})
	f.schema = arrow.NewSchema(f.schema.Fields(), &md)

	h3 := makeUint64Series(t, "h3", []uint64{1, 2, 3, 4, 5})
	out, err := f.WithColumn("h3", h3)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.Schema().Metadata().GetValue("geo")
	if !ok {
		t.Fatal("schema metadata dropped by WithColumn")
	}
	if got != `{"primary_column":"geometry"}` {
		t.Fatalf("metadata mutated: %s", got)
	}
}

// TestListColumn_Construction verifies that a Frame can be
// constructed with a List<String> column and that basic
// operations (Shape, Column access, ColumnAt, ColumnNames)
// work. Serves as the phase-1a smoke test — any explosion here
// tells us what else in the codebase assumes a scalar column.
func TestListColumn_Construction(t *testing.T) {
	pool := memory.DefaultAllocator

	// Build a List<String> column with 3 rows:
	//   row 0: ["a", "b"]
	//   row 1: []          (empty list — non-null)
	//   row 2: null        (list itself is null)
	lb := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer lb.Release()
	sb := lb.ValueBuilder().(*array.StringBuilder)
	// Row 0
	lb.Append(true)
	sb.Append("a")
	sb.Append("b")
	// Row 1 — empty (Append(true) with no values pushed to inner
	// builder = zero-length list at this row).
	lb.Append(true)
	// Row 2 — null.
	lb.AppendNull()
	arr := lb.NewArray()
	defer arr.Release()

	// Also a scalar id column so we can verify the Frame carries
	// both column shapes correctly.
	ib := array.NewInt64Builder(pool)
	defer ib.Release()
	ib.AppendValues([]int64{1, 2, 3}, nil)
	idArr := ib.NewArray()
	defer idArr.Release()

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "tags", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(arr.DataType(), []arrow.Array{arr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if r, c := f.Shape(); r != 3 || c != 2 {
		t.Fatalf("Shape = (%d, %d), want (3, 2)", r, c)
	}
	names := f.ColumnNames()
	if names[0] != "id" || names[1] != "tags" {
		t.Fatalf("column names = %v", names)
	}
	tagsCol, err := f.Column("tags")
	if err != nil {
		t.Fatal(err)
	}
	if tagsCol.DataType().ID() != arrow.LIST {
		t.Errorf("tags column type = %s, want LIST", tagsCol.DataType())
	}
	// The element type should be preserved.
	lt, ok := tagsCol.DataType().(*arrow.ListType)
	if !ok || lt.Elem().ID() != arrow.STRING {
		t.Errorf("tags element type wrong: %v", tagsCol.DataType())
	}
}

// TestListColumn_BuilderForType verifies builderForType (used by
// custom aggregator + FromStructs paths) can construct a List
// builder from a ListType. Regression guard for the phase-1a
// builder-switch update.
func TestListColumn_BuilderForType(t *testing.T) {
	pool := memory.DefaultAllocator
	lt := arrow.ListOf(arrow.PrimitiveTypes.Int64)
	b, err := builderForType(pool, lt)
	if err != nil {
		t.Fatalf("builderForType: %v", err)
	}
	defer b.Release()
	lb, ok := b.(*array.ListBuilder)
	if !ok {
		t.Fatalf("got %T, want *array.ListBuilder", b)
	}
	// Element builder type is what NewListBuilder configured.
	if _, ok := lb.ValueBuilder().(*array.Int64Builder); !ok {
		t.Fatalf("inner builder = %T, want Int64Builder", lb.ValueBuilder())
	}
}

// roadSnapUDF simulates a UDF whose natural output is a struct:
// (path []uint64, offRoute bool). Serves as the reference pattern for
// UDFs that need to return multiple values per row without splitting
// into two exprs sharing captured state.
type roadSnapUDF struct {
	inner ExprNode // Int64 column driving fake path length
}

func (u *roadSnapUDF) outType() arrow.DataType {
	return arrow.StructOf(
		arrow.Field{Name: "path", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint64), Nullable: true},
		arrow.Field{Name: "offRoute", Type: arrow.FixedWidthTypes.Boolean, Nullable: false},
	)
}

func (u *roadSnapUDF) Eval(input *Frame) (Series, error) {
	s, err := u.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	chunk := s.col.Data().Chunks()[0].(*array.Int64)
	pool := memory.DefaultAllocator
	outType := u.outType().(*arrow.StructType)
	sb, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, err
	}
	defer sb.Release()
	structB := sb.(*array.StructBuilder)
	pathB := structB.FieldBuilder(0).(*array.ListBuilder)
	pathValB := pathB.ValueBuilder().(*array.Uint64Builder)
	offB := structB.FieldBuilder(1).(*array.BooleanBuilder)

	for i := 0; i < chunk.Len(); i++ {
		structB.Append(true)
		n := chunk.Value(i)
		pathB.Append(true)
		for j := int64(0); j < n; j++ {
			pathValB.Append(uint64(j * 100))
		}
		offB.Append(n == 0) // OffRoute when the path is empty.
	}

	arr := structB.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "snap", Type: outType, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked)), nil
}

func (u *roadSnapUDF) Type(schema *arrow.Schema) (arrow.DataType, error) {
	return u.outType(), nil
}

func (u *roadSnapUDF) Children() []Expr { return []Expr{{node: u.inner}} }
func (u *roadSnapUDF) String() string   { return fmt.Sprintf("road_snap(%s)", u.inner) }

// TestStructColumn_UDFOutput confirms a Custom ExprNode can produce a
// Struct<List<Uint64>, Bool> column and the Frame carries it end-to-end
// with the schema intact.
func TestStructColumn_UDFOutput(t *testing.T) {
	pool := memory.DefaultAllocator
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	lenB := array.NewInt64Builder(pool)
	defer lenB.Release()
	idB.AppendValues([]int64{1, 2, 3}, nil)
	lenB.AppendValues([]int64{2, 0, 3}, nil)

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), lenB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(arrs[0].DataType(), []arrow.Array{arrs[0]})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(arrs[1].DataType(), []arrow.Array{arrs[1]})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.WithColumnExpr("snap", Custom(&roadSnapUDF{inner: Col("n").Node()}))
	if err != nil {
		t.Fatalf("WithColumnExpr producing Struct column: %v", err)
	}
	snapS, err := out.Column("snap")
	if err != nil {
		t.Fatal(err)
	}
	if snapS.DataType().ID() != arrow.STRUCT {
		t.Fatalf("snap type = %s, want STRUCT", snapS.DataType())
	}
	st := snapS.DataType().(*arrow.StructType)
	if st.NumFields() != 2 {
		t.Fatalf("struct fields = %d, want 2", st.NumFields())
	}
	if st.Field(0).Name != "path" || st.Field(0).Type.ID() != arrow.LIST {
		t.Errorf("field 0: %+v, want path List", st.Field(0))
	}
	if st.Field(1).Name != "offRoute" || st.Field(1).Type.ID() != arrow.BOOL {
		t.Errorf("field 1: %+v, want offRoute Boolean", st.Field(1))
	}

	// Read back struct field data via arrow's Struct array API.
	sa := snapS.col.Data().Chunks()[0].(*array.Struct)
	pathArr := sa.Field(0).(*array.List)
	offArr := sa.Field(1).(*array.Boolean)
	// Row 1 (n=0): path = [], offRoute = true.
	s1, e1 := pathArr.ValueOffsets(1)
	if e1-s1 != 0 || !offArr.Value(1) {
		t.Errorf("row 1 struct wrong: pathLen=%d off=%v", e1-s1, offArr.Value(1))
	}
	// Row 2 (n=3): path = [0, 100, 200], offRoute = false.
	s2, e2 := pathArr.ValueOffsets(2)
	if e2-s2 != 3 || offArr.Value(2) {
		t.Errorf("row 2 struct wrong: pathLen=%d off=%v", e2-s2, offArr.Value(2))
	}
	pathInner := pathArr.ListValues().(*array.Uint64)
	if pathInner.Value(int(s2)+2) != 200 {
		t.Errorf("row 2 path[2] = %d, want 200", pathInner.Value(int(s2)+2))
	}
}

// TestStructColumn_ListOfStruct confirms the List<Struct<...>> shape
// used by aggregators emitting per-row intervals (Start/End timestamps)
// carries through Frame construction. This is the second motivating
// case the user raised.
func TestStructColumn_ListOfStruct(t *testing.T) {
	pool := memory.DefaultAllocator

	// Struct<Start: Timestamp[ns], End: Timestamp[ns]>
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond}
	structType := arrow.StructOf(
		arrow.Field{Name: "Start", Type: tsType, Nullable: false},
		arrow.Field{Name: "End", Type: tsType, Nullable: false},
	)
	// List<Struct<...>>
	listType := arrow.ListOf(structType)

	// Build via builderForType (which now understands LIST + STRUCT).
	b, err := builderForType(pool, listType)
	if err != nil {
		t.Fatalf("builderForType(List<Struct>): %v", err)
	}
	defer b.Release()
	lb := b.(*array.ListBuilder)
	sb := lb.ValueBuilder().(*array.StructBuilder)
	startB := sb.FieldBuilder(0).(*array.TimestampBuilder)
	endB := sb.FieldBuilder(1).(*array.TimestampBuilder)

	// Row 0: two intervals; Row 1: one interval; Row 2: empty list.
	lb.Append(true)
	sb.Append(true)
	startB.Append(arrow.Timestamp(1000))
	endB.Append(arrow.Timestamp(2000))
	sb.Append(true)
	startB.Append(arrow.Timestamp(3000))
	endB.Append(arrow.Timestamp(4000))
	lb.Append(true)
	sb.Append(true)
	startB.Append(arrow.Timestamp(5000))
	endB.Append(arrow.Timestamp(6000))
	lb.Append(true) // empty list

	arr := lb.NewArray()
	defer arr.Release()

	fields := []arrow.Field{
		{Name: "intervals", Type: listType, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(listType, []arrow.Array{arr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.Shape(); r != 3 {
		t.Fatalf("shape rows = %d, want 3", r)
	}
	ivS, err := f.Column("intervals")
	if err != nil {
		t.Fatal(err)
	}
	// Verify the nested type is preserved through Frame construction.
	lt, ok := ivS.DataType().(*arrow.ListType)
	if !ok {
		t.Fatalf("intervals column not ListType: %s", ivS.DataType())
	}
	if lt.Elem().ID() != arrow.STRUCT {
		t.Fatalf("list element type = %s, want STRUCT", lt.Elem())
	}
	elemSt := lt.Elem().(*arrow.StructType)
	if elemSt.Field(0).Name != "Start" || elemSt.Field(1).Name != "End" {
		t.Errorf("struct field names dropped: %v, %v",
			elemSt.Field(0).Name, elemSt.Field(1).Name)
	}

	// Verify ListLen still works on List<Struct> (list-op independence
	// from element type).
	out, err := f.WithColumnExpr("n", Col("intervals").ListLen())
	if err != nil {
		t.Fatalf("ListLen over List<Struct>: %v", err)
	}
	nArr := out.series[1].col.Data().Chunks()[0].(*array.Int64)
	want := []int64{2, 1, 0}
	for i, w := range want {
		if nArr.Value(i) != w {
			t.Errorf("row %d len = %d, want %d", i, nArr.Value(i), w)
		}
	}
}

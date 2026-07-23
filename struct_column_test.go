package gobi

import (
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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

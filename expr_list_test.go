package gobi

import (
	"fmt"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// listExprFrame builds a fixture with a single List<Int64> column
// "vals" whose rows exercise the interesting cases: multi-element,
// single-element, null-list, and empty-non-null-list.
func listExprFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.Int64Builder)
	// Row 0: [10, 20, 30]
	lb.Append(true)
	vb.AppendValues([]int64{10, 20, 30}, nil)
	// Row 1: [40]
	lb.Append(true)
	vb.Append(40)
	// Row 2: null
	lb.AppendNull()
	// Row 3: []
	lb.Append(true)

	arr := lb.NewArray()
	defer arr.Release()
	fields := []arrow.Field{
		{Name: "vals", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(arr.DataType(), []arrow.Array{arr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExprList_Len(t *testing.T) {
	f := listExprFrame(t)
	out, err := f.WithColumnExpr("len", Col("vals").ListLen())
	if err != nil {
		t.Fatal(err)
	}
	lenS, _ := out.Column("len")
	arr := lenS.col.Data().Chunks()[0].(*array.Int64)
	// Expect [3, 1, null, 0].
	if arr.Value(0) != 3 || arr.Value(1) != 1 {
		t.Fatalf("lens = [%d, %d, ...], want [3, 1]", arr.Value(0), arr.Value(1))
	}
	if !arr.IsNull(2) {
		t.Fatalf("row 2 (null list) len should be null, got %d", arr.Value(2))
	}
	if arr.IsNull(3) || arr.Value(3) != 0 {
		t.Fatalf("row 3 (empty list) len should be 0, got %d (null=%v)", arr.Value(3), arr.IsNull(3))
	}
}

func TestExprList_Get(t *testing.T) {
	f := listExprFrame(t)
	// Positive index.
	out, err := f.WithColumnExpr("first", Col("vals").ListGet(0))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := out.Column("first")
	if first.DataType().ID() != arrow.INT64 {
		t.Fatalf("first type = %s, want INT64", first.DataType())
	}
	arr := first.col.Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != 10 || arr.Value(1) != 40 {
		t.Fatalf("firsts = [%d, %d, ...], want [10, 40]", arr.Value(0), arr.Value(1))
	}
	if !arr.IsNull(2) || !arr.IsNull(3) {
		t.Fatalf("rows 2 (null) + 3 (empty) should give null firsts")
	}

	// Negative index (last element).
	out2, err := f.WithColumnExpr("last", Col("vals").ListGet(-1))
	if err != nil {
		t.Fatal(err)
	}
	lastArr := out2.series[1].col.Data().Chunks()[0].(*array.Int64)
	if lastArr.Value(0) != 30 || lastArr.Value(1) != 40 {
		t.Fatalf("lasts wrong: [%d, %d]", lastArr.Value(0), lastArr.Value(1))
	}

	// Out-of-range → null.
	out3, err := f.WithColumnExpr("far", Col("vals").ListGet(99))
	if err != nil {
		t.Fatal(err)
	}
	farArr := out3.series[1].col.Data().Chunks()[0].(*array.Int64)
	for i := 0; i < 4; i++ {
		if !farArr.IsNull(i) {
			t.Errorf("far[%d] should be null (out-of-range)", i)
		}
	}
}

func TestExprList_Slice(t *testing.T) {
	f := listExprFrame(t)
	out, err := f.WithColumnExpr("tail", Col("vals").ListSlice(1, 3))
	if err != nil {
		t.Fatal(err)
	}
	tailS, _ := out.Column("tail")
	if tailS.DataType().ID() != arrow.LIST {
		t.Fatalf("tail type = %s, want LIST", tailS.DataType())
	}
	la := tailS.col.Data().Chunks()[0].(*array.List)
	s0, e0 := la.ValueOffsets(0)
	if e0-s0 != 2 {
		t.Fatalf("row 0 tail len = %d, want 2", e0-s0)
	}
	inner := la.ListValues().(*array.Int64)
	if inner.Value(int(s0)) != 20 || inner.Value(int(s0)+1) != 30 {
		t.Fatalf("row 0 tail values wrong")
	}
	// Row 1: [40][1:3] = [] (clamped).
	s1, e1 := la.ValueOffsets(1)
	if e1-s1 != 0 {
		t.Fatalf("row 1 tail should clamp to empty, got len %d", e1-s1)
	}
	// Row 2: null list stays null.
	if !la.IsNull(2) {
		t.Fatalf("row 2 (null list) should stay null after slice")
	}
	// Row 3: [][1:3] = [].
	s3, e3 := la.ValueOffsets(3)
	if la.IsNull(3) || e3-s3 != 0 {
		t.Fatalf("row 3 empty-slice should stay empty, null=%v len=%d", la.IsNull(3), e3-s3)
	}
}

func TestExprList_Contains(t *testing.T) {
	f := listExprFrame(t)
	// Contains 20 (row 0 only).
	out, err := f.WithColumnExpr("has20", Col("vals").ListContains(int64(20)))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.series[1].col.Data().Chunks()[0].(*array.Boolean)
	if !arr.Value(0) {
		t.Errorf("row 0 should contain 20")
	}
	if arr.Value(1) {
		t.Errorf("row 1 should not contain 20")
	}
	if !arr.IsNull(2) {
		t.Errorf("row 2 (null list) should give null, got %v", arr.Value(2))
	}
	if arr.Value(3) {
		t.Errorf("row 3 (empty) should give false, got true")
	}
}

func TestExprList_Aggregations(t *testing.T) {
	f := listExprFrame(t)
	// Sum: [10+20+30, 40, null, null-empty] = [60, 40, null, null].
	out, err := f.WithColumnExpr("sum", Col("vals").ListSum())
	if err != nil {
		t.Fatal(err)
	}
	sumS, _ := out.Column("sum")
	if sumS.DataType().ID() != arrow.INT64 {
		t.Fatalf("sum type = %s, want INT64", sumS.DataType())
	}
	sumArr := sumS.col.Data().Chunks()[0].(*array.Int64)
	if sumArr.Value(0) != 60 || sumArr.Value(1) != 40 {
		t.Errorf("sums = [%d, %d, ...], want [60, 40]", sumArr.Value(0), sumArr.Value(1))
	}
	if !sumArr.IsNull(2) || !sumArr.IsNull(3) {
		t.Errorf("null/empty list sum should be null: [null=%v, null=%v]", sumArr.IsNull(2), sumArr.IsNull(3))
	}

	// Mean: [20, 40, null, null].
	out, err = f.WithColumnExpr("mean", Col("vals").ListMean())
	if err != nil {
		t.Fatal(err)
	}
	meanS, _ := out.Column("mean")
	if meanS.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("mean type = %s, want FLOAT64", meanS.DataType())
	}
	meanArr := meanS.col.Data().Chunks()[0].(*array.Float64)
	if meanArr.Value(0) != 20 || meanArr.Value(1) != 40 {
		t.Errorf("means wrong: [%v, %v]", meanArr.Value(0), meanArr.Value(1))
	}

	// Min: [10, 40, null, null]; Max: [30, 40, null, null].
	out, err = f.WithColumnExpr("min", Col("vals").ListMin())
	if err != nil {
		t.Fatal(err)
	}
	minArr := out.series[1].col.Data().Chunks()[0].(*array.Int64)
	if minArr.Value(0) != 10 || minArr.Value(1) != 40 {
		t.Errorf("mins wrong: [%d, %d]", minArr.Value(0), minArr.Value(1))
	}

	out, err = f.WithColumnExpr("max", Col("vals").ListMax())
	if err != nil {
		t.Fatal(err)
	}
	maxArr := out.series[1].col.Data().Chunks()[0].(*array.Int64)
	if maxArr.Value(0) != 30 || maxArr.Value(1) != 40 {
		t.Errorf("maxes wrong: [%d, %d]", maxArr.Value(0), maxArr.Value(1))
	}
}

func TestExprList_FirstLast(t *testing.T) {
	f := listExprFrame(t)
	out, err := f.WithColumnExpr("first", Col("vals").ListFirst())
	if err != nil {
		t.Fatal(err)
	}
	firstArr := out.series[1].col.Data().Chunks()[0].(*array.Int64)
	if firstArr.Value(0) != 10 || firstArr.Value(1) != 40 {
		t.Errorf("first wrong: [%d, %d]", firstArr.Value(0), firstArr.Value(1))
	}
	out, err = f.WithColumnExpr("last", Col("vals").ListLast())
	if err != nil {
		t.Fatal(err)
	}
	lastArr := out.series[1].col.Data().Chunks()[0].(*array.Int64)
	if lastArr.Value(0) != 30 || lastArr.Value(1) != 40 {
		t.Errorf("last wrong: [%d, %d]", lastArr.Value(0), lastArr.Value(1))
	}
}

// listFloatFrame builds a List<Float64> column to exercise the float
// reduction path.
func listFloatFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Float64)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.Float64Builder)
	// Row 0: [1.5, 2.5, 3.5]; Row 1: null-in-list [1.0, null, 3.0].
	lb.Append(true)
	vb.AppendValues([]float64{1.5, 2.5, 3.5}, nil)
	lb.Append(true)
	vb.Append(1.0)
	vb.AppendNull()
	vb.Append(3.0)

	arr := lb.NewArray()
	defer arr.Release()
	fields := []arrow.Field{
		{Name: "vals", Type: arrow.ListOf(arrow.PrimitiveTypes.Float64), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(arr.DataType(), []arrow.Array{arr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExprList_FloatAggWithNullElements(t *testing.T) {
	f := listFloatFrame(t)
	out, err := f.WithColumnExpr("mean", Col("vals").ListMean())
	if err != nil {
		t.Fatal(err)
	}
	arr := out.series[1].col.Data().Chunks()[0].(*array.Float64)
	// Row 0: (1.5+2.5+3.5)/3 = 2.5; Row 1: (1.0+3.0)/2 = 2.0 (null skipped).
	if arr.Value(0) != 2.5 {
		t.Errorf("row 0 mean = %v, want 2.5", arr.Value(0))
	}
	if arr.Value(1) != 2.0 {
		t.Errorf("row 1 mean = %v, want 2.0 (null elem should be skipped)", arr.Value(1))
	}
}

// gridPathUDF simulates a variable-length list-returning UDF (think
// H3 GridPath). For each input row's `n` value it emits a list of
// [0, 1, ..., n-1] as Int64. Serves as the reference implementation
// pattern for custom UDFs whose ExprNode.Eval must produce a
// List<T> column rather than a scalar.
type gridPathUDF struct {
	inner ExprNode // must evaluate to an Int64 column
	name  string
}

func (u *gridPathUDF) Eval(input *Frame) (Series, error) {
	s, err := u.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	// Read n from the inner column at each row.
	chunk := s.col.Data().Chunks()[0].(*array.Int64)
	pool := memory.DefaultAllocator
	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer lb.Release()
	inner := lb.ValueBuilder().(*array.Int64Builder)
	for i := 0; i < chunk.Len(); i++ {
		if chunk.IsNull(i) {
			lb.AppendNull()
			continue
		}
		n := chunk.Value(i)
		lb.Append(true)
		for j := int64(0); j < n; j++ {
			inner.Append(j)
		}
	}
	arr := lb.NewArray()
	defer arr.Release()
	field := arrow.Field{
		Name:     u.name,
		Type:     arrow.ListOf(arrow.PrimitiveTypes.Int64),
		Nullable: true,
	}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked)), nil
}

func (u *gridPathUDF) Type(schema *arrow.Schema) (arrow.DataType, error) {
	return arrow.ListOf(arrow.PrimitiveTypes.Int64), nil
}

func (u *gridPathUDF) Children() []Expr { return []Expr{{node: u.inner}} }
func (u *gridPathUDF) String() string   { return fmt.Sprintf("grid_path(%s)", u.inner) }

// TestExprList_CustomUDFReturnsListColumn confirms the Expr framework
// contract accommodates a Custom ExprNode whose Eval produces a
// variable-length list column. This is the shape of every H3-style
// UDF (GridPath, KRing, PolyfillCells) that returns []Cell per row.
func TestExprList_CustomUDFReturnsListColumn(t *testing.T) {
	// Input frame: id + n. UDF's output list-per-row = [0..n-1].
	pool := memory.DefaultAllocator
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	nB := array.NewInt64Builder(pool)
	defer nB.Release()
	idB.AppendValues([]int64{100, 200, 300, 400}, nil)
	nB.AppendValues([]int64{3, 1, 0, 2}, nil)

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), nB.NewArray()}
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

	out, err := f.WithColumnExpr("path",
		Custom(&gridPathUDF{inner: Col("n").Node(), name: "path"}),
	)
	if err != nil {
		t.Fatalf("WithColumnExpr with list-returning UDF: %v", err)
	}
	if r, c := out.Shape(); r != 4 || c != 3 {
		t.Fatalf("shape = (%d, %d), want (4, 3)", r, c)
	}
	pathS, err := out.Column("path")
	if err != nil {
		t.Fatal(err)
	}
	if pathS.DataType().ID() != arrow.LIST {
		t.Fatalf("path column type = %s, want LIST", pathS.DataType())
	}
	// Row 0: [0, 1, 2]; Row 1: [0]; Row 2: []; Row 3: [0, 1].
	la := pathS.col.Data().Chunks()[0].(*array.List)
	inner := la.ListValues().(*array.Int64)
	type check struct {
		want []int64
	}
	checks := []check{{[]int64{0, 1, 2}}, {[]int64{0}}, {[]int64{}}, {[]int64{0, 1}}}
	for row, ck := range checks {
		start, end := la.ValueOffsets(row)
		if int(end-start) != len(ck.want) {
			t.Fatalf("row %d len = %d, want %d", row, end-start, len(ck.want))
		}
		for i, w := range ck.want {
			if inner.Value(int(start)+i) != w {
				t.Errorf("row %d elem %d = %d, want %d", row, i, inner.Value(int(start)+i), w)
			}
		}
	}
	// Confirm downstream ops (ListLen, ListGet) work on the UDF output.
	out2, err := out.WithColumnExpr("len", Col("path").ListLen())
	if err != nil {
		t.Fatalf("chaining list op onto UDF-produced list column: %v", err)
	}
	lenArr := out2.series[3].col.Data().Chunks()[0].(*array.Int64)
	wantLens := []int64{3, 1, 0, 2}
	for i, w := range wantLens {
		if lenArr.Value(i) != w {
			t.Errorf("len row %d = %d, want %d", i, lenArr.Value(i), w)
		}
	}
}

func TestExprList_TypeCheckRejectsNonList(t *testing.T) {
	// A frame with a plain Int64 column — ListLen against it should
	// error at eval time. (Type-inference errors surface identically.)
	pool := memory.DefaultAllocator
	ib := array.NewInt64Builder(pool)
	defer ib.Release()
	ib.AppendValues([]int64{1, 2, 3}, nil)
	arr := ib.NewArray()
	defer arr.Release()
	fields := []arrow.Field{
		{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(arr.DataType(), []arrow.Array{arr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WithColumnExpr("bad", Col("x").ListLen())
	if err == nil {
		t.Fatal("expected error when ListLen applied to non-list column")
	}
}

// twoListFrame builds a small frame with two List<Int64> columns for
// per-row union testing.
//
//	row  a              b
//	0    [1, 2, 3]      [3, 4, 5]
//	1    [10, 20]       [20, 30]
//	2    []             [100]
//	3    [7]            []
//	4    [1, 1, 2]      [2, 2, 3]   // duplicates within a single list
//	5    null           [999]       // null propagates
//	6    [42]           null        // null propagates
//	7    []             []          // both empty → empty (non-null)
func twoListFrame(t *testing.T, elemType arrow.DataType) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	buildList := func(rows [][]int64, nullRows map[int]bool) arrow.Array {
		lb := array.NewListBuilder(pool, elemType)
		defer lb.Release()
		vb := lb.ValueBuilder()
		switch b := vb.(type) {
		case *array.Int64Builder:
			for i, xs := range rows {
				if nullRows[i] {
					lb.AppendNull()
					continue
				}
				lb.Append(true)
				for _, x := range xs {
					b.Append(x)
				}
			}
		case *array.StringBuilder:
			for i, xs := range rows {
				if nullRows[i] {
					lb.AppendNull()
					continue
				}
				lb.Append(true)
				for _, x := range xs {
					// convert numeric fixture to string
					b.Append(itoaHelper(x))
				}
			}
		}
		return lb.NewArray()
	}

	rowsA := [][]int64{
		{1, 2, 3},
		{10, 20},
		{},
		{7},
		{1, 1, 2},
		{}, // null via nullRows
		{42},
		{},
	}
	rowsB := [][]int64{
		{3, 4, 5},
		{20, 30},
		{100},
		{},
		{2, 2, 3},
		{999},
		{}, // null via nullRows
		{},
	}
	aArr := buildList(rowsA, map[int]bool{5: true})
	bArr := buildList(rowsB, map[int]bool{6: true})
	defer aArr.Release()
	defer bArr.Release()

	fields := []arrow.Field{
		{Name: "a", Type: arrow.ListOf(elemType), Nullable: true},
		{Name: "b", Type: arrow.ListOf(elemType), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(aArr.DataType(), []arrow.Array{aArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(bArr.DataType(), []arrow.Array{bArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// itoaHelper is a tiny local int64→string converter to avoid importing
// strconv for a fixture-only need.
func itoaHelper(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// listRow extracts row i from a List column as []int64.
func listRowInt64(t *testing.T, s Series, i int) (vals []int64, isNull bool) {
	la := s.col.Data().Chunks()[0].(*array.List)
	if la.IsNull(i) {
		return nil, true
	}
	values := la.ListValues().(*array.Int64)
	start, end := la.ValueOffsets(i)
	out := make([]int64, end-start)
	for j := start; j < end; j++ {
		out[j-start] = values.Value(int(j))
	}
	return out, false
}

func TestListUnion_Int64Basic(t *testing.T) {
	f := twoListFrame(t, arrow.PrimitiveTypes.Int64)
	out, err := f.WithColumnExpr("ab", Col("a").ListUnion(Col("b")))
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := out.Column("ab")
	if ab.DataType().ID() != arrow.LIST {
		t.Fatalf("ab type = %s, want LIST", ab.DataType())
	}
	// Row 0: [1,2,3] ∪ [3,4,5] preserving first-seen order = [1,2,3,4,5].
	if got, isNull := listRowInt64(t, ab, 0); isNull || !int64Equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("row 0 = %v (null=%v), want [1 2 3 4 5]", got, isNull)
	}
	// Row 1: [10,20] ∪ [20,30] = [10,20,30]
	if got, _ := listRowInt64(t, ab, 1); !int64Equal(got, []int64{10, 20, 30}) {
		t.Fatalf("row 1 = %v, want [10 20 30]", got)
	}
	// Row 2: [] ∪ [100] = [100]
	if got, _ := listRowInt64(t, ab, 2); !int64Equal(got, []int64{100}) {
		t.Fatalf("row 2 = %v, want [100]", got)
	}
	// Row 3: [7] ∪ [] = [7]
	if got, _ := listRowInt64(t, ab, 3); !int64Equal(got, []int64{7}) {
		t.Fatalf("row 3 = %v, want [7]", got)
	}
	// Row 4: [1,1,2] ∪ [2,2,3] with dedup = [1,2,3]
	if got, _ := listRowInt64(t, ab, 4); !int64Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("row 4 = %v, want [1 2 3]", got)
	}
	// Row 5: null ∪ anything = null
	if _, isNull := listRowInt64(t, ab, 5); !isNull {
		t.Fatalf("row 5 should be null (left side null)")
	}
	// Row 6: anything ∪ null = null
	if _, isNull := listRowInt64(t, ab, 6); !isNull {
		t.Fatalf("row 6 should be null (right side null)")
	}
	// Row 7: [] ∪ [] = [] (non-null)
	got, isNull := listRowInt64(t, ab, 7)
	if isNull {
		t.Fatalf("row 7 both-empty union should be non-null empty list")
	}
	if len(got) != 0 {
		t.Fatalf("row 7 = %v, want []", got)
	}
}

func TestListUnion_StringDedupPreservesOrder(t *testing.T) {
	f := twoListFrame(t, arrow.BinaryTypes.String)
	out, err := f.WithColumnExpr("ab", Col("a").ListUnion(Col("b")))
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := out.Column("ab")
	la := ab.col.Data().Chunks()[0].(*array.List)
	values := la.ListValues().(*array.String)
	get := func(row int) []string {
		start, end := la.ValueOffsets(row)
		out := make([]string, end-start)
		for j := start; j < end; j++ {
			out[j-start] = values.Value(int(j))
		}
		return out
	}
	// Row 4: ["1","1","2"] ∪ ["2","2","3"] = ["1","2","3"]
	if got := get(4); len(got) != 3 || got[0] != "1" || got[1] != "2" || got[2] != "3" {
		t.Fatalf("row 4 = %v, want [1 2 3]", got)
	}
}

// Element-type mismatch surfaces at eval time with a clear error.
func TestListUnion_ElementTypeMismatch(t *testing.T) {
	pool := memory.DefaultAllocator
	i64B := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer i64B.Release()
	i64B.Append(true)
	i64B.ValueBuilder().(*array.Int64Builder).Append(1)
	sB := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer sB.Release()
	sB.Append(true)
	sB.ValueBuilder().(*array.StringBuilder).Append("x")

	iArr := i64B.NewArray()
	defer iArr.Release()
	sArr := sB.NewArray()
	defer sArr.Release()

	fields := []arrow.Field{
		{Name: "a", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
		{Name: "b", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(iArr.DataType(), []arrow.Array{iArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(sArr.DataType(), []arrow.Array{sArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WithColumnExpr("ab", Col("a").ListUnion(Col("b")))
	if err == nil {
		t.Fatal("expected element-type-mismatch error")
	}
	if !strings.Contains(err.Error(), "element-type mismatch") {
		t.Fatalf("error should mention element-type mismatch; got %v", err)
	}
}

// Non-list inputs are rejected.
func TestListUnion_NonListInputs(t *testing.T) {
	f := nullyFrame(t)
	_, err := f.WithColumnExpr("ab", Col("price").ListUnion(Col("tag")))
	if err == nil {
		t.Fatal("expected type error for non-List inputs")
	}
}

// End-to-end via LazyFrame.Collect — the "aligned pipeline" pattern
// the user described: two CollectSet-style aggregations joined on cell,
// then ListUnion them.
func TestListUnion_LazyChainWithCollectSet(t *testing.T) {
	f := setAggFrame(t)
	// Two independent aggregations of provider distinct-sets per region.
	// Then chain a ListUnion on itself just to verify it works in-plan.
	out, err := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"}).
		WithColumn("s2", Col("s").ListUnion(Col("s"))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// s ∪ s = s (idempotent). Row count should still be 2 (NA, EU).
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	s, _ := out.Column("s")
	s2, _ := out.Column("s2")
	// Both columns should hold the same lists after idempotent union.
	sLA := s.col.Data().Chunks()[0].(*array.List)
	s2LA := s2.col.Data().Chunks()[0].(*array.List)
	for i := 0; i < 2; i++ {
		s1Start, s1End := sLA.ValueOffsets(i)
		s2Start, s2End := s2LA.ValueOffsets(i)
		if (s1End - s1Start) != (s2End - s2Start) {
			t.Fatalf("row %d list length differs after idempotent union: %d vs %d",
				i, s1End-s1Start, s2End-s2Start)
		}
	}
}

func int64Equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package gobi

import (
	"errors"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// exprFrame builds a small frame for expression tests:
//
//	name   price (f64)  qty (i64)  region (str)  active (bool)
//	Alpha    10.0            3       "US"          true
//	Bravo    20.0            5       "EU"          false
//	Charlie  30.0            7       "US"          true
//	Delta    40.0            2       "EU"          false
func exprFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"Alpha", "Bravo", "Charlie", "Delta"}, nil)

	priceB := array.NewFloat64Builder(pool)
	defer priceB.Release()
	priceB.AppendValues([]float64{10, 20, 30, 40}, nil)

	qtyB := array.NewInt64Builder(pool)
	defer qtyB.Release()
	qtyB.AppendValues([]int64{3, 5, 7, 2}, nil)

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"US", "EU", "US", "EU"}, nil)

	activeB := array.NewBooleanBuilder(pool)
	defer activeB.Release()
	activeB.AppendValues([]bool{true, false, true, false}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "price", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "qty", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "active", Type: arrow.FixedWidthTypes.Boolean, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{
		nameB.NewArray(), priceB.NewArray(), qtyB.NewArray(),
		regionB.NewArray(), activeB.NewArray(),
	}
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

// -- constructor + printing -----------------------------------------------

func TestExpr_String(t *testing.T) {
	e := Col("price").Mul(Lit(1.08)).Gt(Lit(100.0))
	got := e.String()
	want := `((col("price") * lit(1.08)) > lit(100))`
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestExpr_NilString(t *testing.T) {
	var e Expr
	if e.String() != "<nil-expr>" {
		t.Fatalf("nil expr.String() = %q", e.String())
	}
}

// -- Eval fast path (col op lit) ------------------------------------------

func TestExpr_ColMulLit(t *testing.T) {
	df := exprFrame(t)
	// price * 2 → 20, 40, 60, 80
	e := Col("price").Mul(Lit(2.0))
	out, err := df.WithColumnExpr("price2", e)
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("price2")
	arr := col.Column().Data().Chunks()[0].(*array.Float64)
	want := []float64{20, 40, 60, 80}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

func TestExpr_ColGtLit(t *testing.T) {
	df := exprFrame(t)
	// price > 25 → false, false, true, true
	e := Col("price").Gt(Lit(25.0))
	out, err := df.FilterExpr(e)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 (Charlie, Delta)", out.NumRows())
	}
	names, _ := out.Column("name")
	got := names.Column().Data().Chunks()[0].(*array.String)
	if got.Value(0) != "Charlie" || got.Value(1) != "Delta" {
		t.Fatalf("names = %s, %s", got.Value(0), got.Value(1))
	}
}

func TestExpr_ChainedArithmetic(t *testing.T) {
	df := exprFrame(t)
	// (price * 1.08) > 25   for price=[10,20,30,40] → [10.8,21.6,32.4,43.2]
	// > 25  → [false,false,true,true]
	e := Col("price").Mul(Lit(1.08)).Gt(Lit(25.0))
	out, err := df.FilterExpr(e)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("chained arith filter rows = %d, want 2", out.NumRows())
	}
}

// -- col vs col (no fast path) --------------------------------------------

func TestExpr_ColEqCol(t *testing.T) {
	df := exprFrame(t)
	// price + qty and compare to some constant
	// Actually simpler: qty + qty vs qty*2 (both == qty*2)
	// Use price > qty (Float > Int cross-type comparison via promotion).
	e := Col("price").Gt(Col("qty"))
	out, err := df.FilterExpr(e)
	if err != nil {
		t.Fatal(err)
	}
	// price > qty for all 4 rows (10>3, 20>5, 30>7, 40>2) → 4 rows
	if out.NumRows() != 4 {
		t.Fatalf("price>qty rows = %d, want 4", out.NumRows())
	}
}

// -- boolean combinators + Not --------------------------------------------

func TestExpr_And(t *testing.T) {
	df := exprFrame(t)
	// price > 15 AND active   → row 3 only (Charlie price=30 active=true)
	e := Col("price").Gt(Lit(15.0)).And(Col("active"))
	out, err := df.FilterExpr(e)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 1 {
		t.Fatalf("AND rows = %d, want 1", out.NumRows())
	}
	names, _ := out.Column("name")
	got := names.Column().Data().Chunks()[0].(*array.String)
	if got.Value(0) != "Charlie" {
		t.Fatalf("AND row = %s, want Charlie", got.Value(0))
	}
}

func TestExpr_Or(t *testing.T) {
	df := exprFrame(t)
	// price < 15 OR price > 35  → Alpha (10) and Delta (40)
	e := Col("price").Lt(Lit(15.0)).Or(Col("price").Gt(Lit(35.0)))
	out, err := df.FilterExpr(e)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("OR rows = %d, want 2", out.NumRows())
	}
}

func TestExpr_Not(t *testing.T) {
	df := exprFrame(t)
	// NOT active → Bravo, Delta
	e := Col("active").Not()
	out, err := df.FilterExpr(e)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("NOT rows = %d, want 2", out.NumRows())
	}
}

// -- non-fast-path scalar ops (Ne, Le, Ge) -------------------------------

func TestExpr_NeScalar(t *testing.T) {
	df := exprFrame(t)
	// price != 20 → 3 rows (Alpha, Charlie, Delta)
	out, err := df.FilterExpr(Col("price").Ne(Lit(20.0)))
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 3 {
		t.Fatalf("!= rows = %d, want 3", out.NumRows())
	}
}

func TestExpr_LeScalar(t *testing.T) {
	df := exprFrame(t)
	// price <= 20 → 2 rows (Alpha, Bravo)
	out, err := df.FilterExpr(Col("price").Le(Lit(20.0)))
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("<= rows = %d, want 2", out.NumRows())
	}
}

// -- string comparison -----------------------------------------------------

func TestExpr_StringEq(t *testing.T) {
	df := exprFrame(t)
	// region == "US" → Alpha, Charlie
	out, err := df.FilterExpr(Col("region").Eq(Lit("US")))
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("region=US rows = %d, want 2", out.NumRows())
	}
}

// -- error paths -----------------------------------------------------------

func TestExpr_FilterMustBeBool(t *testing.T) {
	df := exprFrame(t)
	// price * 2 is Float64, not Bool — filter must reject.
	_, err := df.FilterExpr(Col("price").Mul(Lit(2.0)))
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Fatalf("want ErrExprTypeMismatch, got %v", err)
	}
}

func TestExpr_MissingColumnErrors(t *testing.T) {
	df := exprFrame(t)
	_, err := df.FilterExpr(Col("nope").Gt(Lit(0.0)))
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestExpr_UnsupportedLiteral(t *testing.T) {
	df := exprFrame(t)
	_, err := df.WithColumnExpr("bad", Lit([]int{1, 2}))
	if !errors.Is(err, ErrUnsupportedLiteral) {
		t.Fatalf("want ErrUnsupportedLiteral, got %v", err)
	}
}

// -- type inference --------------------------------------------------------

func TestExpr_TypeInference(t *testing.T) {
	df := exprFrame(t)
	schema := df.Schema()
	cases := []struct {
		e    Expr
		want arrow.Type
	}{
		{Col("price"), arrow.FLOAT64},
		{Col("qty"), arrow.INT64},
		{Col("price").Add(Lit(1.0)), arrow.FLOAT64},
		{Col("qty").Add(Col("price")), arrow.FLOAT64}, // int + float → float
		{Col("qty").Add(Lit(int64(1))), arrow.INT64},  // int + int → int
		{Col("price").Gt(Lit(1.0)), arrow.BOOL},
		{Col("active").Not(), arrow.BOOL},
		{Col("active").And(Col("active")), arrow.BOOL},
	}
	for _, c := range cases {
		got, err := c.e.node.Type(schema)
		if err != nil {
			t.Errorf("Type(%s) err: %v", c.e, err)
			continue
		}
		if got.ID() != c.want {
			t.Errorf("Type(%s) = %s, want %s", c.e, got, c.want)
		}
	}
}

func TestExpr_TypeInference_ArithOnBoolErrors(t *testing.T) {
	df := exprFrame(t)
	// active + qty is not allowed.
	_, err := Col("active").Add(Col("qty")).node.Type(df.Schema())
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Fatalf("want ErrExprTypeMismatch, got %v", err)
	}
}

// -- WithColumnExpr replacement -------------------------------------------

func TestExpr_WithColumnExpr_ReplacesInPlace(t *testing.T) {
	df := exprFrame(t)
	// Overwrite "price" with (price * 2).
	out, err := df.WithColumnExpr("price", Col("price").Mul(Lit(2.0)))
	if err != nil {
		t.Fatal(err)
	}
	if out.NumCols() != df.NumCols() {
		t.Fatalf("cols = %d, want %d (in-place replace)", out.NumCols(), df.NumCols())
	}
	col, _ := out.Column("price")
	arr := col.Column().Data().Chunks()[0].(*array.Float64)
	if arr.Value(0) != 20 {
		t.Fatalf("row 0 = %v, want 20", arr.Value(0))
	}
}

// -- Custom node extension point ------------------------------------------

// squareNode: user-defined expression that squares a numeric column
// element-wise, producing Float64 output. Written the way an external
// package (e.g. h3x, hashcol) would ship one.
type squareNode struct {
	inner Expr
}

func (n *squareNode) Eval(input *Frame) (Series, error) {
	inner, err := n.inner.Node().Eval(input)
	if err != nil {
		return Series{}, err
	}
	return inner.Mul(inner)
}

func (n *squareNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	return n.inner.Node().Type(schema)
}

func (n *squareNode) Children() []Expr { return []Expr{n.inner} }
func (n *squareNode) String() string   { return "square(" + n.inner.String() + ")" }

func TestExpr_CustomNode(t *testing.T) {
	df := exprFrame(t)
	// price² > 500  → Charlie (900), Delta (1600)
	sq := Custom(&squareNode{inner: Col("price")})
	out, err := df.FilterExpr(sq.Gt(Lit(500.0)))
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("custom-node filter rows = %d, want 2", out.NumRows())
	}
	// String surface reflects the custom name.
	if !strings.Contains(sq.String(), "square(") {
		t.Fatalf("custom String() = %s, want to contain 'square('", sq.String())
	}
}

// -- Alias -----------------------------------------------------------------

func TestExpr_Alias(t *testing.T) {
	// Alias only affects downstream naming; the string form should
	// still show the original tree so users can see what the alias
	// points at.
	e := Col("price").Mul(Lit(1.08)).Alias("usd")
	if !strings.Contains(e.String(), `AS "usd"`) {
		t.Fatalf("alias not in string: %s", e.String())
	}
}

// bitFlagsFrame builds a one-column Int64 Frame of packed-flag
// values for exercising BitAnd/BitOr/BitXor with a scalar mask.
func bitFlagsFrame(t testing.TB) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	// Bit 0 set: 1, 3, 5. Bit 1 set: 2, 3, 6, 7.
	b.AppendValues([]int64{0, 1, 2, 3, 4, 5, 6, 7}, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "flags", Type: arrow.PrimitiveTypes.Int64, Nullable: false}
	col := arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestExpr_BitAnd_Scalar — Col & Lit(bit) unpacks a single flag,
// output stays Int64.
func TestExpr_BitAnd_Scalar(t *testing.T) {
	f := bitFlagsFrame(t)
	out, err := f.WithColumnExpr("bit0", Col("flags").BitAnd(Lit(int64(1))))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("bit0")
	if col.DataType().ID() != arrow.INT64 {
		t.Fatalf("dtype = %s, want INT64", col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	want := []int64{0, 1, 0, 1, 0, 1, 0, 1}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

// TestExpr_BitOr_BitXor_Scalar — sanity for the other two ops on the
// same fixture.
func TestExpr_BitOr_BitXor_Scalar(t *testing.T) {
	f := bitFlagsFrame(t)
	out, err := f.WithColumnExpr("or8", Col("flags").BitOr(Lit(int64(8))))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.mustCol("or8").col.Data().Chunks()[0].(*array.Int64)
	// Every value gets bit 3 set → 8, 9, 10, 11, 12, 13, 14, 15.
	want := []int64{8, 9, 10, 11, 12, 13, 14, 15}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("or8 row %d = %d, want %d", i, arr.Value(i), w)
		}
	}

	out, err = f.WithColumnExpr("xor5", Col("flags").BitXor(Lit(int64(5))))
	if err != nil {
		t.Fatal(err)
	}
	arr = out.mustCol("xor5").col.Data().Chunks()[0].(*array.Int64)
	xorWant := []int64{5, 4, 7, 6, 1, 0, 3, 2}
	for i, w := range xorWant {
		if arr.Value(i) != w {
			t.Errorf("xor5 row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

// TestExpr_BitAnd_ColCol — col & col path (falls through the scalar
// fast path when both operands are ExprNodes rather than literals).
func TestExpr_BitAnd_ColCol(t *testing.T) {
	pool := memory.DefaultAllocator
	aB := array.NewInt64Builder(pool)
	defer aB.Release()
	aB.AppendValues([]int64{0xF0, 0xF0, 0xFF, 0x0F}, nil)
	bB := array.NewInt64Builder(pool)
	defer bB.Release()
	bB.AppendValues([]int64{0x0F, 0xFF, 0xAA, 0xF0}, nil)
	arrA := aB.NewArray()
	defer arrA.Release()
	arrB := bB.NewArray()
	defer arrB.Release()
	fields := []arrow.Field{
		{Name: "a", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "b", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(arrA.DataType(), []arrow.Array{arrA})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(arrB.DataType(), []arrow.Array{arrB})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.WithColumnExpr("and", Col("a").BitAnd(Col("b")))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.mustCol("and").col.Data().Chunks()[0].(*array.Int64)
	want := []int64{0x00, 0xF0, 0xAA, 0x00}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %x, want %x", i, arr.Value(i), w)
		}
	}
}

// TestExpr_Bitwise_RejectsFloat — bitwise on Float column errors
// at Type() time.
func TestExpr_Bitwise_RejectsFloat(t *testing.T) {
	pool := memory.DefaultAllocator
	fb := array.NewFloat64Builder(pool)
	defer fb.Release()
	fb.AppendValues([]float64{1.5, 2.5}, nil)
	arr := fb.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	col := arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, _ := NewFrame(schema, []arrow.Column{*col})
	_, err := f.WithColumnExpr("bad", Col("x").BitAnd(Lit(int64(1))))
	if err == nil {
		t.Fatal("expected error for BitAnd on Float64 column")
	}
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("error should wrap ErrExprTypeMismatch, got %v", err)
	}
}

// mustCol returns the named column or panics — test-only helper for
// tighter assertions.
func (f *Frame) mustCol(name string) Series {
	s, err := f.Column(name)
	if err != nil {
		panic(err)
	}
	return s
}

// --- LitNull ------------------------------------------------------------

func TestLitNull_StringBroadcast(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.WithColumnExpr("provider", LitNull(arrow.BinaryTypes.String))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := out.Column("provider")
	if err != nil {
		t.Fatal(err)
	}
	if provider.DataType().ID() != arrow.STRING {
		t.Fatalf("provider type = %s, want STRING", provider.DataType())
	}
	arr := provider.col.Data().Chunks()[0].(*array.String)
	for i := range 5 {
		if !arr.IsNull(i) {
			t.Fatalf("row %d not null (LitNull should produce all nulls)", i)
		}
	}
}

func TestLitNull_ComposesWithCollectSet(t *testing.T) {
	f := lazyFrame(t)
	// Adding a null provider column then aggregating: the null-of-type
	// String should be skipped by the set aggregator, yielding an
	// empty list per group.
	out, err := f.Lazy().
		WithColumn("provider", LitNull(arrow.BinaryTypes.String)).
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "providers"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Two regions (US, EU), each with empty provider set.
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	providers, _ := out.Column("providers")
	la := providers.col.Data().Chunks()[0].(*array.List)
	for i := 0; i < 2; i++ {
		start, end := la.ValueOffsets(i)
		if end != start {
			t.Fatalf("row %d producer list should be empty; got %d values", i, end-start)
		}
	}
}

func TestLitNull_TypeIsPreserved(t *testing.T) {
	f := lazyFrame(t)
	// Verify Type() reports the requested dtype at plan time.
	lf := f.Lazy().WithColumn("k", LitNull(arrow.PrimitiveTypes.Uint64))
	fields, ok := lf.Schema().FieldsByName("k")
	if !ok || len(fields) == 0 {
		t.Fatalf("k field missing")
	}
	if fields[0].Type.ID() != arrow.UINT64 {
		t.Fatalf("k type = %s, want UINT64", fields[0].Type)
	}
}

// --- SelectCols ---------------------------------------------------------

func TestSelectCols_Eager(t *testing.T) {
	f := lazyFrame(t)
	// Reorder: region first, then price. Drop id and active.
	out, err := f.SelectCols("region", "price")
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if len(names) != 2 || names[0] != "region" || names[1] != "price" {
		t.Fatalf("column names = %v, want [region price]", names)
	}
}

func TestSelectCols_MissingColumn(t *testing.T) {
	f := lazyFrame(t)
	_, err := f.SelectCols("region", "nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestSelectCols_Lazy(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Lazy().SelectCols("region", "id").Collect()
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if len(names) != 2 || names[0] != "region" || names[1] != "id" {
		t.Fatalf("column names = %v, want [region id]", names)
	}
}

func TestSelectCols_Empty(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.SelectCols()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ColumnNames()) != 0 {
		t.Fatalf("empty SelectCols should produce a 0-column Frame, got %d columns", len(out.ColumnNames()))
	}
}

// --- Rename -------------------------------------------------------------

func TestRename_EagerPreservesBuffers(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Rename("price", "cost")
	if err != nil {
		t.Fatal(err)
	}
	// The renamed column should be present under the new name...
	cost, err := out.Column("cost")
	if err != nil {
		t.Fatal(err)
	}
	if cost.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("cost dtype = %s, want FLOAT64", cost.DataType())
	}
	// ...and absent under the old name.
	if _, err := out.Column("price"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("old name should be gone; got err %v", err)
	}
	// Column order preserved.
	oldNames := f.ColumnNames()
	newNames := out.ColumnNames()
	if len(oldNames) != len(newNames) {
		t.Fatalf("column count changed: %v -> %v", oldNames, newNames)
	}
	// Only the renamed position differs.
	for i := range oldNames {
		want := oldNames[i]
		if oldNames[i] == "price" {
			want = "cost"
		}
		if newNames[i] != want {
			t.Fatalf("column %d: %q, want %q", i, newNames[i], want)
		}
	}
}

func TestRename_MissingErrors(t *testing.T) {
	f := lazyFrame(t)
	_, err := f.Rename("nope", "new")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestRename_SameNameIsNoop(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Rename("price", "price")
	if err != nil {
		t.Fatal(err)
	}
	// Frame.Rename with old==new returns the receiver — cheap no-op,
	// matches LazyFrame.Rename's identity path.
	if out != f {
		t.Fatal("Frame.Rename(same, same) should return the receiver unchanged")
	}
}

func TestRename_Lazy(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Lazy().Rename("price", "cost").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Column("cost"); err != nil {
		t.Fatalf("cost column missing after lazy rename: %v", err)
	}
	if _, err := out.Column("price"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("price should be gone; got err %v", err)
	}
}

func TestRename_LazySameNameNoop(t *testing.T) {
	f := lazyFrame(t)
	// LazyFrame.Rename with old==new returns receiver — the plan tree
	// shouldn't grow a rename node.
	lf := f.Lazy()
	lf2 := lf.Rename("price", "price")
	if lf2 != lf {
		t.Fatal("LazyFrame.Rename(same, same) should be a no-op returning the receiver")
	}
}

// End-to-end: rename + SelectCols + LitNull composing.
func TestRename_ComposedPipeline(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Lazy().
		Rename("price", "cost").
		WithColumn("provider", LitNull(arrow.BinaryTypes.String)).
		SelectCols("id", "cost", "provider").
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if len(names) != 3 || names[0] != "id" || names[1] != "cost" || names[2] != "provider" {
		t.Fatalf("column names = %v, want [id cost provider]", names)
	}
}

// TestExprShift_WithColumn — Col("price").Shift(1) as an appended
// column. Row 0 becomes null; rows 1..N take the prior row's value.
func TestExprShift_WithColumn(t *testing.T) {
	f := exprFrame(t)
	out, err := f.WithColumnExpr("prev_price", Col("price").Shift(1))
	if err != nil {
		t.Fatal(err)
	}
	prev, err := out.Column("prev_price")
	if err != nil {
		t.Fatal(err)
	}
	arr := prev.col.Data().Chunks()[0].(*array.Float64)
	if !arr.IsNull(0) {
		t.Fatalf("row 0 should be null after Shift(1), got %v", arr.Value(0))
	}
	want := []float64{0, 10, 20, 30}
	for i := 1; i < 4; i++ {
		if arr.IsNull(i) {
			t.Fatalf("row %d null after Shift(1); expected %v", i, want[i])
		}
		if arr.Value(i) != want[i] {
			t.Fatalf("row %d = %v, want %v", i, arr.Value(i), want[i])
		}
	}
}

// TestExprShift_NegativeLead — Shift(-1) produces a lead (i+1's value
// in position i). Last row becomes null.
func TestExprShift_NegativeLead(t *testing.T) {
	f := exprFrame(t)
	out, err := f.WithColumnExpr("next_price", Col("price").Shift(-1))
	if err != nil {
		t.Fatal(err)
	}
	next, _ := out.Column("next_price")
	arr := next.col.Data().Chunks()[0].(*array.Float64)
	want := []float64{20, 30, 40}
	for i := 0; i < 3; i++ {
		if arr.IsNull(i) {
			t.Fatalf("row %d null after Shift(-1); expected %v", i, want[i])
		}
		if arr.Value(i) != want[i] {
			t.Fatalf("row %d = %v, want %v", i, arr.Value(i), want[i])
		}
	}
	if !arr.IsNull(3) {
		t.Fatalf("last row should be null after Shift(-1), got %v", arr.Value(3))
	}
}

// TestExprShift_ComposesWithArithmetic — a period-over-period delta
// via Sub(Shift(1)). Row 0 is null; the rest carry the arithmetic
// difference.
func TestExprShift_ComposesWithArithmetic(t *testing.T) {
	f := exprFrame(t)
	// delta = price - price.shift(1)
	out, err := f.WithColumnExpr("delta", Col("price").Sub(Col("price").Shift(1)))
	if err != nil {
		t.Fatal(err)
	}
	delta, _ := out.Column("delta")
	arr := delta.col.Data().Chunks()[0].(*array.Float64)
	if !arr.IsNull(0) {
		t.Fatalf("row 0 should be null (Sub with null RHS); got %v", arr.Value(0))
	}
	// prices are 10, 20, 30, 40 → deltas at rows 1..3 are all 10.
	for i := 1; i < 4; i++ {
		if arr.IsNull(i) || arr.Value(i) != 10 {
			t.Fatalf("row %d = %v (null=%v), want 10", i, arr.Value(i), arr.IsNull(i))
		}
	}
}

// TestExprShift_Lazy — same expression through the lazy plan surface,
// verifying the ExprNode round-trips through Compile/Execute.
func TestExprShift_Lazy(t *testing.T) {
	f := exprFrame(t)
	out, err := f.Lazy().
		WithColumn("prev_price", Col("price").Shift(1)).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_price")
	arr := prev.col.Data().Chunks()[0].(*array.Float64)
	if !arr.IsNull(0) {
		t.Fatalf("row 0 should be null via lazy Shift(1)")
	}
	if arr.Value(3) != 30 {
		t.Fatalf("row 3 via lazy = %v, want 30", arr.Value(3))
	}
}

// TestExprShift_StringColumn — Shift on a non-numeric column also
// works (Series.Shift routes through builderForType, which covers
// strings). Verifies we haven't accidentally locked Shift to numeric-
// only paths at the Expr layer.
func TestExprShift_StringColumn(t *testing.T) {
	f := exprFrame(t)
	out, err := f.WithColumnExpr("prev_name", Col("name").Shift(1))
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_name")
	arr := prev.col.Data().Chunks()[0].(*array.String)
	if !arr.IsNull(0) {
		t.Fatalf("row 0 should be null after Shift(1); got %q", arr.Value(0))
	}
	if arr.Value(1) != "Alpha" || arr.Value(3) != "Charlie" {
		t.Fatalf("Shift preserves values wrong: got %q, %q", arr.Value(1), arr.Value(3))
	}
}

// shiftOverFrame builds a per-partition Shift fixture:
//
//	k    t   v
//	A    3   100
//	B    1   200
//	A    1   300
//	B    3   400
//	A    2   500
//
// Groups A rows in input order: [100, 300, 500]. Sorted by t: [300, 500, 100].
// Groups B rows in input order: [200, 400]. Sorted by t: [200, 400].
func shiftOverFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	kb := array.NewStringBuilder(pool)
	defer kb.Release()
	kb.AppendValues([]string{"A", "B", "A", "B", "A"}, nil)
	tb := array.NewInt64Builder(pool)
	defer tb.Release()
	tb.AppendValues([]int64{3, 1, 1, 3, 2}, nil)
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	vb.AppendValues([]int64{100, 200, 300, 400, 500}, nil)

	fields := []arrow.Field{
		{Name: "k", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "t", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{kb.NewArray(), tb.NewArray(), vb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestExprShift_OverUnordered — Shift(1) per partition using input row
// order within each partition (polars default when no order_by given).
// Row-order-preserving output: each row gets the prior in-partition v
// at its own position, or null if it's the first row in that partition.
func TestExprShift_OverUnordered(t *testing.T) {
	f := shiftOverFrame(t)
	out, err := f.WithColumnExpr("prev_v", Col("v").Shift(1).Over("k"))
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_v")
	arr := prev.col.Data().Chunks()[0].(*array.Int64)
	// A rows in input order: 0, 2, 4 with v = 100, 300, 500
	//   → shift(1) within A yields: null, 100, 300 at rows 0, 2, 4.
	// B rows in input order: 1, 3 with v = 200, 400
	//   → shift(1) within B yields: null, 200 at rows 1, 3.
	if !arr.IsNull(0) {
		t.Fatalf("row 0 (first A) should be null, got %d", arr.Value(0))
	}
	if !arr.IsNull(1) {
		t.Fatalf("row 1 (first B) should be null, got %d", arr.Value(1))
	}
	if arr.Value(2) != 100 {
		t.Fatalf("row 2 (2nd A) = %d, want 100", arr.Value(2))
	}
	if arr.Value(3) != 200 {
		t.Fatalf("row 3 (2nd B) = %d, want 200", arr.Value(3))
	}
	if arr.Value(4) != 300 {
		t.Fatalf("row 4 (3rd A) = %d, want 300", arr.Value(4))
	}
}

// TestExprShift_OverOrdered — Shift(1) per partition, sorted by t
// within each partition. Uses polars-shaped `.OverOrdered` API.
// Row-order in the output still matches input row order — orderBy only
// affects what "previous row" means inside the partition.
func TestExprShift_OverOrdered(t *testing.T) {
	f := shiftOverFrame(t)
	out, err := f.WithColumnExpr("prev_v",
		Col("v").Shift(1).OverOrdered([]string{"k"}, SortKey{Column: "t"}))
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_v")
	arr := prev.col.Data().Chunks()[0].(*array.Int64)
	// A rows sorted by t: row 2 (t=1, v=300), row 4 (t=2, v=500), row 0 (t=3, v=100).
	//   Shift(1) within sorted A: row 2 → null, row 4 → 300, row 0 → 500.
	// B rows sorted by t: row 1 (t=1, v=200), row 3 (t=3, v=400).
	//   Shift(1) within sorted B: row 1 → null, row 3 → 200.
	// Scatter back to input row positions:
	//   row 0: 500 (A, t=3, prior in sorted A is row 4 with v=500)
	//   row 1: null (B, t=1, first in sorted B)
	//   row 2: null (A, t=1, first in sorted A)
	//   row 3: 200 (B, t=3, prior in sorted B is row 1 with v=200)
	//   row 4: 300 (A, t=2, prior in sorted A is row 2 with v=300)
	want := []struct {
		row  int
		val  int64
		null bool
	}{
		{0, 500, false},
		{1, 0, true},
		{2, 0, true},
		{3, 200, false},
		{4, 300, false},
	}
	for _, tc := range want {
		if tc.null {
			if !arr.IsNull(tc.row) {
				t.Errorf("row %d: expected null, got %d", tc.row, arr.Value(tc.row))
			}
			continue
		}
		if arr.IsNull(tc.row) {
			t.Errorf("row %d: expected %d, got null", tc.row, tc.val)
			continue
		}
		if arr.Value(tc.row) != tc.val {
			t.Errorf("row %d = %d, want %d", tc.row, arr.Value(tc.row), tc.val)
		}
	}
}

// TestExprShift_OverOrderedDescending — orderBy Descending semantics.
// Same partitions as above but sorted by t descending changes what
// "previous" means. Sorted A (t desc): row 0 (t=3, v=100), row 4 (t=2, v=500), row 2 (t=1, v=300).
// Shift(1) yields at input positions: row 0 → null, row 4 → 100, row 2 → 500.
func TestExprShift_OverOrderedDescending(t *testing.T) {
	f := shiftOverFrame(t)
	out, err := f.WithColumnExpr("prev_v",
		Col("v").Shift(1).OverOrdered([]string{"k"}, SortKey{Column: "t", Descending: true}))
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_v")
	arr := prev.col.Data().Chunks()[0].(*array.Int64)
	// A sorted t desc: rows [0, 4, 2] with v [100, 500, 300].
	// Shift(1): row 0 → null, row 4 → 100, row 2 → 500.
	// B sorted t desc: rows [3, 1] with v [400, 200].
	// Shift(1): row 3 → null, row 1 → 400.
	if !arr.IsNull(0) || !arr.IsNull(3) {
		t.Fatalf("row 0 and row 3 should be null (first in each partition sorted desc)")
	}
	if arr.Value(1) != 400 {
		t.Fatalf("row 1 = %d, want 400", arr.Value(1))
	}
	if arr.Value(2) != 500 {
		t.Fatalf("row 2 = %d, want 500", arr.Value(2))
	}
	if arr.Value(4) != 100 {
		t.Fatalf("row 4 = %d, want 100", arr.Value(4))
	}
}

// TestExprShift_OverAlignedFastPath — same result via the aligned
// fast path: input pre-sorted by [k, t], with a matching
// PartitionMetadata claim. Verifies the fast path produces the same
// output as the general path. Uses WithPartitionAssertion at the
// LazyFrame level (fast path detection reads the plan node's metadata
// via inputMeta at Compile time).
func TestExprShift_OverAlignedFastPath(t *testing.T) {
	pool := memory.DefaultAllocator
	// Pre-sorted by [k, t]: A rows first (t=1,2,3), then B (t=1,3).
	kb := array.NewStringBuilder(pool)
	defer kb.Release()
	kb.AppendValues([]string{"A", "A", "A", "B", "B"}, nil)
	tb := array.NewInt64Builder(pool)
	defer tb.Release()
	tb.AppendValues([]int64{1, 2, 3, 1, 3}, nil)
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	// Corresponds to shiftOverFrame's values under the (k,t) sort.
	vb.AppendValues([]int64{300, 500, 100, 200, 400}, nil)

	fields := []arrow.Field{
		{Name: "k", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "t", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{kb.NewArray(), tb.NewArray(), vb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	// Attach the aligned+sorted claim so the fast path fires.
	lf, err := f.Lazy().WithPartitionAssertion(&PartitionMetadata{
		Columns:      []string{"k"},
		HashFn:       "test/v1",
		SortedBy:     []SortKey{{Column: "k"}, {Column: "t"}},
		SortEnforced: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := lf.
		WithColumn("prev_v", Col("v").Shift(1).OverOrdered([]string{"k"}, SortKey{Column: "t"})).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_v")
	arr := prev.col.Data().Chunks()[0].(*array.Int64)
	// A partition [t=1,2,3] with v=[300,500,100]. Shift(1): [null, 300, 500].
	// B partition [t=1,3] with v=[200,400]. Shift(1): [null, 200].
	// Rows 0..4 map to (A,t=1), (A,t=2), (A,t=3), (B,t=1), (B,t=3).
	if !arr.IsNull(0) || !arr.IsNull(3) {
		t.Fatalf("rows 0 and 3 should be null (first in each partition)")
	}
	want := map[int]int64{1: 300, 2: 500, 4: 200}
	for row, w := range want {
		if arr.IsNull(row) {
			t.Fatalf("row %d: expected %d, got null", row, w)
		}
		if arr.Value(row) != w {
			t.Fatalf("row %d = %d, want %d", row, arr.Value(row), w)
		}
	}
}

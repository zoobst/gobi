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
		{Col("qty").Add(Col("price")), arrow.FLOAT64},   // int + float → float
		{Col("qty").Add(Lit(int64(1))), arrow.INT64},    // int + int → int
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

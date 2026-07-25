package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// nullyFrame builds a small frame with mixed null / non-null values
// across two columns, for exercising IsNull / IsNotNull.
//
//	id  price       tag
//	1   10.0        "a"
//	2   null        null
//	3   30.0        null
//	4   null        "d"
//	5   50.0        "e"
func nullyFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3, 4, 5}, nil)

	priceB := array.NewFloat64Builder(pool)
	defer priceB.Release()
	priceB.Append(10)
	priceB.AppendNull()
	priceB.Append(30)
	priceB.AppendNull()
	priceB.Append(50)

	tagB := array.NewStringBuilder(pool)
	defer tagB.Release()
	tagB.Append("a")
	tagB.AppendNull()
	tagB.AppendNull()
	tagB.Append("d")
	tagB.Append("e")

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "price", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "tag", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), priceB.NewArray(), tagB.NewArray()}
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

func TestExprIsNull_BasicFloat(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.WithColumnExpr("missing_price", Col("price").IsNull())
	if err != nil {
		t.Fatal(err)
	}
	missing, err := out.Column("missing_price")
	if err != nil {
		t.Fatal(err)
	}
	if missing.DataType().ID() != arrow.BOOL {
		t.Fatalf("output type = %s, want BOOL", missing.DataType())
	}
	arr := missing.col.Data().Chunks()[0].(*array.Boolean)
	want := []bool{false, true, false, true, false}
	for i, w := range want {
		if arr.IsNull(i) {
			t.Fatalf("row %d unexpectedly null (IsNull output must not itself be null)", i)
		}
		if arr.Value(i) != w {
			t.Fatalf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

func TestExprIsNotNull_BasicString(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.WithColumnExpr("has_tag", Col("tag").IsNotNull())
	if err != nil {
		t.Fatal(err)
	}
	has, _ := out.Column("has_tag")
	arr := has.col.Data().Chunks()[0].(*array.Boolean)
	want := []bool{true, false, false, true, true}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Fatalf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

// IsNull as a Filter predicate — narrows the frame to null rows.
func TestExprIsNull_UseAsFilter(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.FilterExpr(Col("price").IsNull())
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("filtered rows = %d, want 2 (rows 2 and 4 have null price)", out.NumRows())
	}
	idS, _ := out.Column("id")
	idArr := idS.col.Data().Chunks()[0].(*array.Int64)
	if idArr.Value(0) != 2 || idArr.Value(1) != 4 {
		t.Fatalf("ids = %v %v, want 2 4", idArr.Value(0), idArr.Value(1))
	}
}

// Combine IsNull with And/Or — sanity check that the emitted Boolean
// composes with the existing logical combinators.
func TestExprIsNull_ComposesWithAnd(t *testing.T) {
	f := nullyFrame(t)
	// Rows where price is set AND tag is missing.
	out, err := f.FilterExpr(Col("price").IsNotNull().And(Col("tag").IsNull()))
	if err != nil {
		t.Fatal(err)
	}
	// Only row 3 has price=30, tag=null.
	if out.NumRows() != 1 {
		t.Fatalf("filtered rows = %d, want 1 (only row 3)", out.NumRows())
	}
	idS, _ := out.Column("id")
	if got := idS.col.Data().Chunks()[0].(*array.Int64).Value(0); got != 3 {
		t.Fatalf("id = %d, want 3", got)
	}
}

// IsNull + IsNotNull applied to the same column always partition the
// row space: exactly one is true per row, never both, never neither.
func TestExprIsNull_PartitionsRowSpace(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.
		WithColumnExpr("null_price", Col("price").IsNull())
	if err != nil {
		t.Fatal(err)
	}
	out, err = out.WithColumnExpr("has_price", Col("price").IsNotNull())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := out.Column("null_price")
	h, _ := out.Column("has_price")
	nA := n.col.Data().Chunks()[0].(*array.Boolean)
	hA := h.col.Data().Chunks()[0].(*array.Boolean)
	for i := 0; i < out.NumRows(); i++ {
		if nA.Value(i) == hA.Value(i) {
			t.Fatalf("row %d has null_price==has_price==%v", i, nA.Value(i))
		}
	}
}

// Lazy path — verify the plan compiles + executes end-to-end via
// LazyFrame.Collect (Optimize + Compile + Execute).
func TestExprIsNull_Lazy(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.Lazy().
		Filter(Col("price").IsNotNull()).
		Select(Col("id"), Col("price"), Col("tag").IsNotNull().Alias("has_tag")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 3 {
		t.Fatalf("row count = %d, want 3 (price non-null: rows 1, 3, 5)", out.NumRows())
	}
	hasTagS, err := out.Column("has_tag")
	if err != nil {
		t.Fatal(err)
	}
	arr := hasTagS.col.Data().Chunks()[0].(*array.Boolean)
	// Rows kept: id=1 (tag=a), id=3 (tag=null), id=5 (tag=e).
	want := []bool{true, false, true}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Fatalf("row %d has_tag = %v, want %v", i, arr.Value(i), w)
		}
	}
}

// Default-named output — Select without Alias gets a stable
// {col}_is_null / {col}_is_not_null suffix via the Namer interface.
func TestExprIsNull_DefaultOutputName(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.Lazy().
		Select(Col("price").IsNull()).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	fields := out.Schema().Fields()
	if len(fields) != 1 || fields[0].Name != "price_is_null" {
		t.Fatalf("output field = %v, want price_is_null", fields)
	}
}

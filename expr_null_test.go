package gobi

import (
	"strings"
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

// --- LitEmptyList -------------------------------------------------------

func TestLitEmptyList_BroadcastsEmptyLists(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.WithColumnExpr("providers", LitEmptyList(arrow.BinaryTypes.String))
	if err != nil {
		t.Fatal(err)
	}
	providers, err := out.Column("providers")
	if err != nil {
		t.Fatal(err)
	}
	if providers.DataType().ID() != arrow.LIST {
		t.Fatalf("providers type = %s, want LIST", providers.DataType())
	}
	la := providers.col.Data().Chunks()[0].(*array.List)
	if la.Len() != f.NumRows() {
		t.Fatalf("row count = %d, want %d", la.Len(), f.NumRows())
	}
	for i := 0; i < la.Len(); i++ {
		if la.IsNull(i) {
			t.Fatalf("row %d unexpectedly null (LitEmptyList must produce non-null rows)", i)
		}
		start, end := la.ValueOffsets(i)
		if start != end {
			t.Fatalf("row %d non-empty (%d values), want empty", i, end-start)
		}
	}
}

func TestLitEmptyList_DistinctFromLitNull(t *testing.T) {
	f := lazyFrame(t)
	// LitNull(ListOf(String)) → all rows null.
	// LitEmptyList(String)    → all rows non-null empty.
	nullOut, _ := f.WithColumnExpr("null_col", LitNull(arrow.ListOf(arrow.BinaryTypes.String)))
	emptyOut, _ := f.WithColumnExpr("empty_col", LitEmptyList(arrow.BinaryTypes.String))
	nullS, _ := nullOut.Column("null_col")
	emptyS, _ := emptyOut.Column("empty_col")
	nullLA := nullS.col.Data().Chunks()[0].(*array.List)
	emptyLA := emptyS.col.Data().Chunks()[0].(*array.List)
	for i := 0; i < nullLA.Len(); i++ {
		if !nullLA.IsNull(i) {
			t.Fatalf("LitNull row %d should be null; got non-null", i)
		}
		if emptyLA.IsNull(i) {
			t.Fatalf("LitEmptyList row %d should be non-null; got null", i)
		}
	}
}

// --- Coalesce -----------------------------------------------------------

// Coalesces two nullable Float64 columns: uses first-non-null per row.
func TestCoalesce_TwoFloat64(t *testing.T) {
	f := nullyFrame(t)
	// nullyFrame: price = [10, null, 30, null, 50]
	// Provide a second Float64 column via WithColumn (backup = 999 everywhere).
	f2, _ := f.WithColumnExpr("backup", Lit(999.0))
	out, err := f2.WithColumnExpr("filled",
		Coalesce(Col("price"), Col("backup")))
	if err != nil {
		t.Fatal(err)
	}
	filled, _ := out.Column("filled")
	arr := filled.col.Data().Chunks()[0].(*array.Float64)
	want := []float64{10, 999, 30, 999, 50}
	for i, w := range want {
		if arr.IsNull(i) {
			t.Fatalf("row %d null, want %v", i, w)
		}
		if arr.Value(i) != w {
			t.Fatalf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

// Variadic: Coalesce(a, b, c) picks the first non-null across all three.
func TestCoalesce_Variadic(t *testing.T) {
	pool := memory.DefaultAllocator
	// Three nullable Int64 columns:
	//   a = [1, null, null, null]
	//   b = [null, 2, null, null]
	//   c = [100, 200, 3, null]
	// Expected: [1, 2, 3, null]
	build := func(vals []int64, nullMask []bool) arrow.Array {
		b := array.NewInt64Builder(pool)
		defer b.Release()
		for i, v := range vals {
			if nullMask[i] {
				b.AppendNull()
			} else {
				b.Append(v)
			}
		}
		return b.NewArray()
	}
	aArr := build([]int64{1, 0, 0, 0}, []bool{false, true, true, true})
	bArr := build([]int64{0, 2, 0, 0}, []bool{true, false, true, true})
	cArr := build([]int64{100, 200, 3, 0}, []bool{false, false, false, true})
	defer aArr.Release()
	defer bArr.Release()
	defer cArr.Release()
	fields := []arrow.Field{
		{Name: "a", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "b", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "c", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(aArr.DataType(), []arrow.Array{aArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(bArr.DataType(), []arrow.Array{bArr})),
		*arrow.NewColumn(fields[2], arrow.NewChunked(cArr.DataType(), []arrow.Array{cArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.WithColumnExpr("first",
		Coalesce(Col("a"), Col("b"), Col("c")))
	if err != nil {
		t.Fatal(err)
	}
	filled, _ := out.Column("first")
	arr := filled.col.Data().Chunks()[0].(*array.Int64)
	wantVal := []int64{1, 2, 3, 0}
	wantNull := []bool{false, false, false, true}
	for i := range 4 {
		if wantNull[i] {
			if !arr.IsNull(i) {
				t.Fatalf("row %d expected null, got %d", i, arr.Value(i))
			}
			continue
		}
		if arr.IsNull(i) {
			t.Fatalf("row %d expected %d, got null", i, wantVal[i])
		}
		if arr.Value(i) != wantVal[i] {
			t.Fatalf("row %d = %d, want %d", i, arr.Value(i), wantVal[i])
		}
	}
}

// The core JoinFull + ListUnion use case: a null-list on one side
// gets coalesced to empty, so ListUnion doesn't propagate null.
func TestCoalesce_ListWithEmptyFallback(t *testing.T) {
	pool := memory.DefaultAllocator
	// seg_providers = [ ["a","b"], null, ["c"] ]
	segB := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer segB.Release()
	segVB := segB.ValueBuilder().(*array.StringBuilder)
	segB.Append(true)
	segVB.Append("a")
	segVB.Append("b")
	segB.AppendNull()
	segB.Append(true)
	segVB.Append("c")
	segArr := segB.NewArray()
	defer segArr.Release()

	// ping_providers = [ null, ["x","y"], ["c","d"] ]
	pingB := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer pingB.Release()
	pingVB := pingB.ValueBuilder().(*array.StringBuilder)
	pingB.AppendNull()
	pingB.Append(true)
	pingVB.Append("x")
	pingVB.Append("y")
	pingB.Append(true)
	pingVB.Append("c")
	pingVB.Append("d")
	pingArr := pingB.NewArray()
	defer pingArr.Release()

	fields := []arrow.Field{
		{Name: "seg", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
		{Name: "ping", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(segArr.DataType(), []arrow.Array{segArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(pingArr.DataType(), []arrow.Array{pingArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	// Wrap both with Coalesce → LitEmptyList, then ListUnion.
	empty := LitEmptyList(arrow.BinaryTypes.String)
	out, err := f.
		WithColumnExpr("seg_safe", Coalesce(Col("seg"), empty))
	if err != nil {
		t.Fatal(err)
	}
	out, err = out.WithColumnExpr("ping_safe", Coalesce(Col("ping"), empty))
	if err != nil {
		t.Fatal(err)
	}
	out, err = out.WithColumnExpr("merged", Col("seg_safe").ListUnion(Col("ping_safe")))
	if err != nil {
		t.Fatalf("ListUnion after Coalesce: %v", err)
	}
	merged, _ := out.Column("merged")
	if merged.DataType().ID() != arrow.LIST {
		t.Fatalf("merged type = %s, want LIST", merged.DataType())
	}
	la := merged.col.Data().Chunks()[0].(*array.List)
	values := la.ListValues().(*array.String)
	get := func(row int) []string {
		if la.IsNull(row) {
			return nil
		}
		start, end := la.ValueOffsets(row)
		out := make([]string, end-start)
		for j := start; j < end; j++ {
			out[j-start] = values.Value(int(j))
		}
		return out
	}
	// Row 0: seg=[a,b], ping=null→[] → union = [a, b]
	if got := get(0); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("row 0 = %v, want [a b]", got)
	}
	// Row 1: seg=null→[], ping=[x,y] → union = [x, y]
	if got := get(1); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("row 1 = %v, want [x y]", got)
	}
	// Row 2: seg=[c], ping=[c,d] → union = [c, d]
	if got := get(2); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("row 2 = %v, want [c d]", got)
	}
}

func TestCoalesce_AllNullReturnsNull(t *testing.T) {
	f := nullyFrame(t)
	// Coalesce two null literals of the same type.
	out, err := f.WithColumnExpr("out",
		Coalesce(LitNull(arrow.BinaryTypes.String), LitNull(arrow.BinaryTypes.String)))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("out")
	arr := col.col.Data().Chunks()[0].(*array.String)
	for i := 0; i < arr.Len(); i++ {
		if !arr.IsNull(i) {
			t.Fatalf("row %d not null; want null (all operands null)", i)
		}
	}
}

func TestCoalesce_TypeMismatch(t *testing.T) {
	f := nullyFrame(t)
	// Int64 + Float64 — must error.
	_, err := f.WithColumnExpr("bad",
		Coalesce(Col("id"), Col("price")))
	if err == nil {
		t.Fatal("expected type mismatch error")
	}
	if !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("error should mention type mismatch; got %v", err)
	}
}

func TestCoalesce_EmptyErrors(t *testing.T) {
	f := nullyFrame(t)
	_, err := f.WithColumnExpr("bad", Coalesce())
	if err == nil {
		t.Fatal("expected error for zero-operand Coalesce")
	}
}

func TestCoalesce_SingleOperandDegenerate(t *testing.T) {
	f := nullyFrame(t)
	// Coalesce(x) should behave like x — no-op wrapper.
	out, err := f.WithColumnExpr("copy", Coalesce(Col("price")))
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := out.Column("price")
	copy, _ := out.Column("copy")
	if orig.Len() != copy.Len() {
		t.Fatalf("length mismatch")
	}
	a := orig.col.Data().Chunks()[0].(*array.Float64)
	b := copy.col.Data().Chunks()[0].(*array.Float64)
	for i := 0; i < a.Len(); i++ {
		if a.IsNull(i) != b.IsNull(i) {
			t.Fatalf("row %d null mismatch: orig=%v copy=%v", i, a.IsNull(i), b.IsNull(i))
		}
		if !a.IsNull(i) && a.Value(i) != b.Value(i) {
			t.Fatalf("row %d value mismatch: orig=%v copy=%v", i, a.Value(i), b.Value(i))
		}
	}
}

// End-to-end via LazyFrame.Collect — Coalesce+LitEmptyList in a plan.
func TestCoalesce_LazyPipeline(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.Lazy().
		WithColumn("safe_tag", Coalesce(Col("tag"), Lit("fallback"))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	safe, _ := out.Column("safe_tag")
	arr := safe.col.Data().Chunks()[0].(*array.String)
	// tag = ["a", null, null, "d", "e"] → safe = ["a", "fallback", "fallback", "d", "e"]
	want := []string{"a", "fallback", "fallback", "d", "e"}
	for i, w := range want {
		if arr.IsNull(i) || arr.Value(i) != w {
			t.Fatalf("row %d = %q (null=%v), want %q", i, arr.Value(i), arr.IsNull(i), w)
		}
	}
}

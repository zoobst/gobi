package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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

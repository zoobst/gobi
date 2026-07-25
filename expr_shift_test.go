package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
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

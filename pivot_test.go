package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestFrame_Pivot_Sum reshapes the lazyFrame (5 rows, region ∈
// {US, EU}, active ∈ {true, false}) into wide form: rows are
// regions, columns are the two active values, cells are the sum of
// price. Expected:
//
//	region  |  false | true
//	EU      |  60    | null    (rows: id=2 price=20 active=false, id=4 price=40 active=false)
//	US      |  null  | 90      (rows: id=1 p=10 t=true, id=3 p=30 t=true, id=5 p=50 t=true)
func TestFrame_Pivot_Sum(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Pivot("region", "active", "price", AggSum)
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := out.Shape()
	if rows != 2 || cols != 3 {
		t.Fatalf("shape = (%d, %d), want (2, 3)", rows, cols)
	}
	names := out.ColumnNames()
	if names[0] != "region" || names[1] != "false" || names[2] != "true" {
		t.Fatalf("column names = %v, want [region false true]", names)
	}
	regionCol, _ := out.Column("region")
	falseCol, _ := out.Column("false")
	trueCol, _ := out.Column("true")
	regions := regionCol.Column().Data().Chunks()[0].(*array.String)
	falses := falseCol.Column().Data().Chunks()[0].(*array.Float64)
	trues := trueCol.Column().Data().Chunks()[0].(*array.Float64)

	// EU row: false=60, true=null.
	if regions.Value(0) != "EU" {
		t.Fatalf("row 0 region = %q, want EU", regions.Value(0))
	}
	if falses.IsNull(0) || math.Abs(falses.Value(0)-60) > 1e-9 {
		t.Errorf("EU/false = %v, want 60", falses.Value(0))
	}
	if !trues.IsNull(0) {
		t.Errorf("EU/true should be null, got %v", trues.Value(0))
	}
	// US row: false=null, true=90.
	if regions.Value(1) != "US" {
		t.Fatalf("row 1 region = %q, want US", regions.Value(1))
	}
	if !falses.IsNull(1) {
		t.Errorf("US/false should be null, got %v", falses.Value(1))
	}
	if trues.IsNull(1) || math.Abs(trues.Value(1)-90) > 1e-9 {
		t.Errorf("US/true = %v, want 90", trues.Value(1))
	}
}

// TestFrame_Pivot_Count aggregates row counts instead of a value —
// verifies AggCount produces an Int64 output column and the null
// cell semantics still hold.
func TestFrame_Pivot_Count(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Pivot("region", "active", "price", AggCount)
	if err != nil {
		t.Fatal(err)
	}
	// EU has 2 rows (both active=false); US has 3 rows (all active=true).
	// Expect Int64 count columns.
	falseCol, _ := out.Column("false")
	trueCol, _ := out.Column("true")
	falses := falseCol.Column().Data().Chunks()[0].(*array.Int64)
	trues := trueCol.Column().Data().Chunks()[0].(*array.Int64)
	if !trues.IsNull(0) || falses.IsNull(0) || falses.Value(0) != 2 {
		t.Errorf("EU/false = %d isnull=%v, want 2", falses.Value(0), falses.IsNull(0))
	}
	if !falses.IsNull(1) || trues.IsNull(1) || trues.Value(1) != 3 {
		t.Errorf("US/true = %d isnull=%v, want 3", trues.Value(1), trues.IsNull(1))
	}
}

// TestFrame_Pivot_RejectsSameColumns catches the trivial misuse.
func TestFrame_Pivot_RejectsSameColumns(t *testing.T) {
	df := lazyFrame(t)
	if _, err := df.Pivot("region", "region", "price", AggSum); err == nil {
		t.Fatal("expected error for duplicate index/columns")
	}
	if _, err := df.Pivot("", "active", "price", AggSum); err == nil {
		t.Fatal("expected error for empty index name")
	}
}

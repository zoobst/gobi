package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestFrame_Unique_SingleColumn — dedupe rows by region column.
// lazyFrame has regions [US, EU, US, EU, US] so first-occurrence
// dedupe yields 2 rows: (id=1, region=US) and (id=2, region=EU).
// All 4 source columns are preserved (this is drop_duplicates
// semantics; for just the distinct region values use
// `Series.Unique()`).
func TestFrame_Unique_SingleColumn(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Unique("region")
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := out.Shape()
	if rows != 2 || cols != 4 {
		t.Fatalf("shape = (%d, %d), want (2, 4)", rows, cols)
	}
	regionCol, _ := out.Column("region")
	idCol, _ := out.Column("id")
	regions := regionCol.Column().Data().Chunks()[0].(*array.String)
	ids := idCol.Column().Data().Chunks()[0].(*array.Int64)
	if regions.Value(0) != "US" || ids.Value(0) != 1 {
		t.Errorf("row 0 = (id=%d, region=%q), want (id=1, region=US)", ids.Value(0), regions.Value(0))
	}
	if regions.Value(1) != "EU" || ids.Value(1) != 2 {
		t.Errorf("row 1 = (id=%d, region=%q), want (id=2, region=EU)", ids.Value(1), regions.Value(1))
	}
}

// TestFrame_Unique_MultiColumn — distinct (region, active) pairs.
// lazyFrame has:  (US,true),(EU,false),(US,true),(EU,false),(US,true)
// → distinct pairs: (US,true), (EU,false). First-occurrence order.
func TestFrame_Unique_MultiColumn(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Unique("region", "active")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := out.Shape()
	if rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
}

// TestFrame_Unique_AllColumns — every row is unique on id, so
// distinct-over-all returns every row.
func TestFrame_Unique_AllColumns(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Unique() // no args = all columns
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 5 {
		t.Fatalf("rows = %d, want 5 (all rows distinct on id)", out.NumRows())
	}
}

// TestFrame_ValueCounts checks the frequency table + sort order.
// regions are US×3, EU×2 → US=3, EU=2 (sorted desc by count).
func TestFrame_ValueCounts(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.ValueCounts("region")
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := out.Shape()
	if rows != 2 || cols != 2 {
		t.Fatalf("shape = (%d, %d), want (2, 2)", rows, cols)
	}
	regionCol, _ := out.Column("region")
	countCol, _ := out.Column("count")
	regions := regionCol.Column().Data().Chunks()[0].(*array.String)
	counts := countCol.Column().Data().Chunks()[0].(*array.Int64)
	if regions.Value(0) != "US" || counts.Value(0) != 3 {
		t.Errorf("row 0 = (%q, %d), want (US, 3)", regions.Value(0), counts.Value(0))
	}
	if regions.Value(1) != "EU" || counts.Value(1) != 2 {
		t.Errorf("row 1 = (%q, %d), want (EU, 2)", regions.Value(1), counts.Value(1))
	}
}

// TestSeries_Unique reproduces Series.Unique against the region
// column. Return type must be Series with 2 rows (US, EU) in
// first-occurrence order.
func TestSeries_Unique(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("region")
	got, err := s.Unique()
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != 2 {
		t.Fatalf("Unique len = %d, want 2", got.Len())
	}
	arr := got.Column().Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "US" || arr.Value(1) != "EU" {
		t.Fatalf("unique = [%q, %q], want [US, EU]", arr.Value(0), arr.Value(1))
	}
}

// TestSeries_NUnique covers the count-only path — no arrow array
// build, just the distinct count.
func TestSeries_NUnique(t *testing.T) {
	df := lazyFrame(t)
	regionS, _ := df.Column("region")
	n, err := regionS.NUnique()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("region NUnique = %d, want 2", n)
	}
	idS, _ := df.Column("id")
	n, err = idS.NUnique()
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("id NUnique = %d, want 5", n)
	}
}

package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestSeries_Shift_Positive shifts a series down (positive n): the
// first n rows become null, subsequent rows carry the previous
// values. lazyFrame id = [1,2,3,4,5]; Shift(2) → [null, null, 1, 2, 3].
func TestSeries_Shift_Positive(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("id")
	got, err := s.Shift(2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != s.Len() {
		t.Fatalf("Shift len = %d, want %d", got.Len(), s.Len())
	}
	arr := got.Column().Data().Chunks()[0].(*array.Int64)
	if !arr.IsNull(0) || !arr.IsNull(1) {
		t.Error("row 0 and 1 should be null")
	}
	want := []int64{1, 2, 3}
	for i, w := range want {
		if arr.IsNull(2+i) || arr.Value(2+i) != w {
			t.Errorf("row %d = %d, want %d", 2+i, arr.Value(2+i), w)
		}
	}
}

// TestSeries_Shift_Negative shifts up: the last n rows become null,
// the head is drawn from the tail. id=[1,2,3,4,5]; Shift(-2)
// → [3, 4, 5, null, null].
func TestSeries_Shift_Negative(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("id")
	got, err := s.Shift(-2)
	if err != nil {
		t.Fatal(err)
	}
	arr := got.Column().Data().Chunks()[0].(*array.Int64)
	want := []int64{3, 4, 5}
	for i, w := range want {
		if arr.IsNull(i) || arr.Value(i) != w {
			t.Errorf("row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
	if !arr.IsNull(3) || !arr.IsNull(4) {
		t.Error("rows 3 and 4 should be null")
	}
}

// TestSeries_Shift_ZeroNoOp — Shift(0) returns a copy with identical
// values. Useful for callers that dispatch through Shift generically.
func TestSeries_Shift_ZeroNoOp(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("id")
	got, err := s.Shift(0)
	if err != nil {
		t.Fatal(err)
	}
	arr := got.Column().Data().Chunks()[0].(*array.Int64)
	for i, w := range []int64{1, 2, 3, 4, 5} {
		if arr.IsNull(i) || arr.Value(i) != w {
			t.Errorf("row %d = %v, want %d", i, arr.Value(i), w)
		}
	}
}

// TestSeries_Shift_LargerThanLength — |n| ≥ length returns all
// nulls with the source's length.
func TestSeries_Shift_LargerThanLength(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("id")
	got, err := s.Shift(100)
	if err != nil {
		t.Fatal(err)
	}
	arr := got.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Len() != s.Len() {
		t.Fatalf("Shift len = %d, want %d", arr.Len(), s.Len())
	}
	for i := 0; i < arr.Len(); i++ {
		if !arr.IsNull(i) {
			t.Errorf("row %d should be null", i)
		}
	}
}

// TestSeries_Diff computes the first-difference of a Float64 column.
// price = [10, 20, 30, 40, 50], Diff(1) = [null, 10, 10, 10, 10].
func TestSeries_Diff(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("price")
	got, err := s.Diff(1)
	if err != nil {
		t.Fatal(err)
	}
	arr := got.Column().Data().Chunks()[0].(*array.Float64)
	if !arr.IsNull(0) {
		t.Fatal("Diff(1)[0] should be null")
	}
	for i := 1; i < arr.Len(); i++ {
		if arr.IsNull(i) || arr.Value(i) != 10 {
			t.Errorf("Diff(1)[%d] = %v, want 10", i, arr.Value(i))
		}
	}
}

// TestSeries_Diff_NPositive rejects zero + negative n. Callers who
// want look-ahead differences should compose Shift(-k) + Sub.
func TestSeries_Diff_NPositive(t *testing.T) {
	df := lazyFrame(t)
	s, _ := df.Column("price")
	if _, err := s.Diff(0); err == nil {
		t.Fatal("Diff(0) should be an error")
	}
	if _, err := s.Diff(-1); err == nil {
		t.Fatal("Diff(-1) should be an error")
	}
}

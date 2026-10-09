package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// intSeries builds a Series of Int64 values, treating missing as null.
func intSeries(name string, values []int64, valid []bool) Series {
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(values, valid)
	return newSeriesFromArray(name, b.NewArray())
}

func floatSeries(name string, values []float64, valid []bool) Series {
	b := array.NewFloat64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(values, valid)
	return newSeriesFromArray(name, b.NewArray())
}

func TestSeries_Add_Int(t *testing.T) {
	a := intSeries("a", []int64{1, 2, 3}, nil)
	b := intSeries("b", []int64{10, 20, 30}, nil)
	out, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.DataType().ID() != arrow.INT64 {
		t.Fatalf("Add(int,int) type = %s, want INT64", out.DataType())
	}
	for i, want := range []float64{11, 22, 33} {
		v, _, _ := out.numericAt(i)
		if v != want {
			t.Errorf("row %d = %v, want %v", i, v, want)
		}
	}
}

func TestSeries_Div_PromotesToFloat(t *testing.T) {
	a := intSeries("a", []int64{10, 20, 30}, nil)
	b := intSeries("b", []int64{4, 5, 3}, nil)
	out, err := a.Div(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("Div type = %s, want FLOAT64", out.DataType())
	}
	v, _, _ := out.numericAt(0)
	if v != 2.5 {
		t.Errorf("10/4 = %v, want 2.5", v)
	}
}

func TestSeries_NullPropagates(t *testing.T) {
	a := floatSeries("a", []float64{1, 2, 3}, []bool{true, false, true})
	b := floatSeries("b", []float64{4, 5, 6}, nil)
	out, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	_, ok, _ := out.numericAt(1)
	if ok {
		t.Fatalf("row 1 should be null")
	}
	v, ok, _ := out.numericAt(2)
	if !ok || v != 9 {
		t.Fatalf("row 2: v=%v ok=%v", v, ok)
	}
}

func TestSeries_LengthMismatch(t *testing.T) {
	a := intSeries("a", []int64{1, 2}, nil)
	b := intSeries("b", []int64{1, 2, 3}, nil)
	_, err := a.Add(b)
	if !errors.Is(err, ErrColumnLenMismatch) {
		t.Fatalf("want ErrColumnLenMismatch, got %v", err)
	}
}

func TestSeries_Scalar(t *testing.T) {
	s := intSeries("s", []int64{1, 2, 3}, nil)
	out, err := s.MulScalar(10)
	if err != nil {
		t.Fatal(err)
	}
	if out.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("MulScalar type = %s, want FLOAT64", out.DataType())
	}
	v, _, _ := out.numericAt(1)
	if v != 20 {
		t.Fatalf("v = %v", v)
	}
}

func TestSeries_Aggregations(t *testing.T) {
	s := floatSeries("v", []float64{1, 2, 3, 4}, []bool{true, true, false, true})
	sum, err := s.Sum()
	if err != nil {
		t.Fatal(err)
	}
	if sum != 7 {
		t.Errorf("sum = %v, want 7", sum)
	}
	mean, _ := s.Mean()
	if math.Abs(mean-7.0/3) > 1e-12 {
		t.Errorf("mean = %v, want ~2.333", mean)
	}
	minV, _ := s.Min()
	if minV != 1 {
		t.Errorf("min = %v", minV)
	}
	maxV, _ := s.Max()
	if maxV != 4 {
		t.Errorf("max = %v", maxV)
	}
	if s.Count() != 3 {
		t.Errorf("count = %d, want 3", s.Count())
	}
}

func TestSeries_Comparisons(t *testing.T) {
	a := intSeries("a", []int64{1, 2, 3, 4}, nil)
	b := intSeries("b", []int64{1, 3, 3, 2}, nil)
	eq, _ := a.Eq(b)
	arr := eq.col.Data().Chunks()[0].(*array.Boolean)
	wantEq := []bool{true, false, true, false}
	for i, w := range wantEq {
		if arr.Value(i) != w {
			t.Errorf("eq[%d] = %v, want %v", i, arr.Value(i), w)
		}
	}
	lt, _ := a.Lt(b)
	arr = lt.col.Data().Chunks()[0].(*array.Boolean)
	wantLt := []bool{false, true, false, false}
	for i, w := range wantLt {
		if arr.Value(i) != w {
			t.Errorf("lt[%d] = %v, want %v", i, arr.Value(i), w)
		}
	}
}

func TestSeries_NotNumeric(t *testing.T) {
	sb := array.NewStringBuilder(memory.DefaultAllocator)
	defer sb.Release()
	sb.AppendValues([]string{"a", "b"}, nil)
	s := newSeriesFromArray("s", sb.NewArray())

	other := intSeries("o", []int64{1, 2}, nil)
	_, err := s.Add(other)
	if !errors.Is(err, ErrNotNumeric) {
		t.Fatalf("want ErrNotNumeric, got %v", err)
	}
}

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

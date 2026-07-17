package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
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

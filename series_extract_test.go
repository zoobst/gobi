package gobi

import (
	"errors"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestSeriesExtract_Int64WithNulls(t *testing.T) {
	f := nullyFrame(t)
	// nullyFrame's id column: [1, 2, 3, 4, 5], non-null.
	vals, err := f.series[0].Int64s()
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1, 2, 3, 4, 5}
	if !int64Equal(vals, want) {
		t.Fatalf("id = %v, want %v", vals, want)
	}
	// Nullable Float64 column: price = [10, null, 30, null, 50].
	priceVals, err := f.series[1].Float64s()
	if err != nil {
		t.Fatal(err)
	}
	nulls := f.series[1].Nulls()
	if len(priceVals) != 5 || len(nulls) != 5 {
		t.Fatalf("unexpected lengths: values=%d nulls=%d", len(priceVals), len(nulls))
	}
	wantVal := []float64{10, 0, 30, 0, 50}
	wantNull := []bool{false, true, false, true, false}
	for i := range 5 {
		if priceVals[i] != wantVal[i] {
			t.Fatalf("row %d value = %v, want %v", i, priceVals[i], wantVal[i])
		}
		if nulls[i] != wantNull[i] {
			t.Fatalf("row %d null = %v, want %v", i, nulls[i], wantNull[i])
		}
	}
}

func TestSeriesExtract_StringsWithNulls(t *testing.T) {
	f := nullyFrame(t)
	// tag = ["a", null, null, "d", "e"]
	tagS, _ := f.Column("tag")
	vals, err := tagS.Strings()
	if err != nil {
		t.Fatal(err)
	}
	nulls := tagS.Nulls()
	wantVal := []string{"a", "", "", "d", "e"}
	wantNull := []bool{false, true, true, false, false}
	for i := range 5 {
		if vals[i] != wantVal[i] {
			t.Fatalf("row %d value = %q, want %q", i, vals[i], wantVal[i])
		}
		if nulls[i] != wantNull[i] {
			t.Fatalf("row %d null = %v, want %v", i, nulls[i], wantNull[i])
		}
	}
}

func TestSeriesExtract_TypeMismatch(t *testing.T) {
	f := nullyFrame(t)
	// price is Float64 — asking for Int64s should error.
	_, err := f.series[1].Int64s()
	if err == nil {
		t.Fatal("expected type mismatch error")
	}
	if !errors.Is(err, ErrColumnTypeMismatch) {
		t.Fatalf("error should wrap ErrColumnTypeMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "float64") {
		t.Fatalf("error should name the actual type; got %v", err)
	}
}

func TestSeriesExtract_Bools(t *testing.T) {
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	b.Append(true)
	b.AppendNull()
	b.Append(false)
	b.Append(true)
	arr := b.NewArray()
	defer arr.Release()

	field := arrow.Field{Name: "flag", Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	s := NewSeries(arrow.NewColumn(field, chunked))

	vals, err := s.Bools()
	if err != nil {
		t.Fatal(err)
	}
	nulls := s.Nulls()
	wantVal := []bool{true, false, false, true}
	wantNull := []bool{false, true, false, false}
	for i := range 4 {
		if vals[i] != wantVal[i] {
			t.Fatalf("row %d value = %v, want %v", i, vals[i], wantVal[i])
		}
		if nulls[i] != wantNull[i] {
			t.Fatalf("row %d null = %v, want %v", i, nulls[i], wantNull[i])
		}
	}
}

func TestSeriesExtract_Uint64H3Shape(t *testing.T) {
	f := setAggFrame(t)
	cellS, _ := f.Column("h3_cell")
	vals, err := cellS.Uint64s()
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{100, 100, 200, 300, 300, 100}
	if len(vals) != len(want) {
		t.Fatalf("len=%d want %d", len(vals), len(want))
	}
	for i, w := range want {
		if vals[i] != w {
			t.Fatalf("row %d = %d, want %d", i, vals[i], w)
		}
	}
}

func TestSeriesExtract_ReturnedSliceIsSafeToMutate(t *testing.T) {
	f := nullyFrame(t)
	vals, err := f.series[0].Int64s()
	if err != nil {
		t.Fatal(err)
	}
	original := make([]int64, len(vals))
	copy(original, vals)
	// Mutate the returned slice — should NOT affect the source Series.
	for i := range vals {
		vals[i] = 999
	}
	vals2, err := f.series[0].Int64s()
	if err != nil {
		t.Fatal(err)
	}
	for i := range original {
		if vals2[i] != original[i] {
			t.Fatalf("mutating extracted slice leaked back into Series at row %d", i)
		}
	}
}

func TestSeriesExtract_EmptySeries(t *testing.T) {
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	arr := b.NewArray() // zero rows
	defer arr.Release()

	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	s := NewSeries(arrow.NewColumn(field, chunked))

	vals, err := s.Int64s()
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 0 {
		t.Fatalf("empty series returned %d values", len(vals))
	}
	if len(s.Nulls()) != 0 {
		t.Fatalf("empty series returned %d nulls entries", len(s.Nulls()))
	}
}

func TestSeriesExtract_MultiChunk(t *testing.T) {
	pool := memory.DefaultAllocator
	// Two chunks of Int64: [1, 2] and [3, null, 5].
	b1 := array.NewInt64Builder(pool)
	defer b1.Release()
	b1.AppendValues([]int64{1, 2}, nil)
	c1 := b1.NewArray()
	defer c1.Release()

	b2 := array.NewInt64Builder(pool)
	defer b2.Release()
	b2.Append(3)
	b2.AppendNull()
	b2.Append(5)
	c2 := b2.NewArray()
	defer c2.Release()

	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	chunked := arrow.NewChunked(arr64Type(), []arrow.Array{c1, c2})
	s := NewSeries(arrow.NewColumn(field, chunked))

	vals, err := s.Int64s()
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1, 2, 3, 0, 5} // null-slot represented as 0
	if !int64Equal(vals, want) {
		t.Fatalf("multi-chunk values = %v, want %v", vals, want)
	}
	nulls := s.Nulls()
	wantNull := []bool{false, false, false, true, false}
	for i, w := range wantNull {
		if nulls[i] != w {
			t.Fatalf("row %d null = %v, want %v", i, nulls[i], w)
		}
	}
}

// arr64Type is a shim for the type argument to arrow.NewChunked.
func arr64Type() arrow.DataType { return arrow.PrimitiveTypes.Int64 }

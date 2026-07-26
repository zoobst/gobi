package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// singleChunkSeries builds a one-chunk Series over the given values,
// with a specified null bitmap (or nil for no nulls).
func singleChunkInt64Series(t testing.TB, vals []int64, valid []bool) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues(vals, valid)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	return NewSeries(arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr})))
}

func singleChunkFloat64SeriesVals(t testing.TB, vals []float64, valid []bool) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	b.AppendValues(vals, valid)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: true}
	return NewSeries(arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr})))
}

// TestSeries_Int64Values_ZeroCopy — Int64Values returns the same
// backing slice arrow-go's array.Int64 exposes, single-chunk only.
func TestSeries_Int64Values_ZeroCopy(t *testing.T) {
	s := singleChunkInt64Series(t, []int64{1, 2, 3, 4}, nil)
	vals, ok := s.Int64Values()
	if !ok {
		t.Fatal("Int64Values on single-chunk Int64 should succeed")
	}
	if len(vals) != 4 {
		t.Fatalf("len(vals) = %d, want 4", len(vals))
	}
	want := []int64{1, 2, 3, 4}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("vals[%d] = %d, want %d", i, vals[i], w)
		}
	}
}

// TestSeries_Int64Values_TypeMismatch — Float64 column returns
// (nil, false) via Int64Values.
func TestSeries_Int64Values_TypeMismatch(t *testing.T) {
	s := singleChunkFloat64SeriesVals(t, []float64{1, 2, 3}, nil)
	vals, ok := s.Int64Values()
	if ok || vals != nil {
		t.Fatalf("Int64Values on Float64 = (%v, %v), want (nil, false)", vals, ok)
	}
}

// TestSeries_Float64Values_ZeroCopy — same shape, Float64.
func TestSeries_Float64Values_ZeroCopy(t *testing.T) {
	s := singleChunkFloat64SeriesVals(t, []float64{1.5, 2.5, 3.5}, nil)
	vals, ok := s.Float64Values()
	if !ok {
		t.Fatal("Float64Values on single-chunk Float64 should succeed")
	}
	want := []float64{1.5, 2.5, 3.5}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("vals[%d] = %v, want %v", i, vals[i], w)
		}
	}
}

// TestSeries_Values_MultiChunkReturnsFalse — multi-chunk Series
// can't return a single zero-copy slice; accessor returns false.
func TestSeries_Values_MultiChunkReturnsFalse(t *testing.T) {
	pool := memory.DefaultAllocator
	b1 := array.NewInt64Builder(pool)
	defer b1.Release()
	b1.AppendValues([]int64{1, 2}, nil)
	a1 := b1.NewArray()
	defer a1.Release()
	b2 := array.NewInt64Builder(pool)
	defer b2.Release()
	b2.AppendValues([]int64{3, 4}, nil)
	a2 := b2.NewArray()
	defer a2.Release()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	chunked := arrow.NewChunked(a1.DataType(), []arrow.Array{a1, a2})
	s := NewSeries(arrow.NewColumn(field, chunked))
	if _, ok := s.Int64Values(); ok {
		t.Fatal("multi-chunk Int64Values should return false")
	}
}

// TestSeries_HasNulls_And_NullCount — HasNulls reads NullN metadata;
// NullCount sums it across chunks.
func TestSeries_HasNulls_And_NullCount(t *testing.T) {
	// No nulls.
	s := singleChunkInt64Series(t, []int64{1, 2, 3}, nil)
	if s.HasNulls() {
		t.Error("HasNulls = true, want false")
	}
	if got := s.NullCount(); got != 0 {
		t.Errorf("NullCount = %d, want 0", got)
	}

	// Two nulls.
	s = singleChunkInt64Series(t, []int64{10, 0, 30, 0, 50},
		[]bool{true, false, true, false, true})
	if !s.HasNulls() {
		t.Error("HasNulls = false, want true")
	}
	if got := s.NullCount(); got != 2 {
		t.Errorf("NullCount = %d, want 2", got)
	}
}

// TestSeries_Nulls_FastPath — the optimized bitmap walk agrees with
// per-row IsNull. Verify against a mixed-null series.
func TestSeries_Nulls_FastPath(t *testing.T) {
	s := singleChunkInt64Series(t,
		[]int64{10, 0, 30, 0, 50, 0, 70},
		[]bool{true, false, true, false, true, false, true})
	nulls := s.Nulls()
	if len(nulls) != s.Len() {
		t.Fatalf("len(nulls) = %d, want %d", len(nulls), s.Len())
	}
	want := []bool{false, true, false, true, false, true, false}
	for i, w := range want {
		if nulls[i] != w {
			t.Errorf("nulls[%d] = %v, want %v", i, nulls[i], w)
		}
	}
}

// TestSeries_Nulls_NoNullsChunkSkipped — a chunk with NullN==0 falls
// through the fast path leaving its output slots at false. Combined
// with a null-bearing chunk to cover both branches.
func TestSeries_Nulls_MultiChunk(t *testing.T) {
	pool := memory.DefaultAllocator
	b1 := array.NewInt64Builder(pool)
	defer b1.Release()
	b1.AppendValues([]int64{1, 2, 3}, nil) // no nulls
	a1 := b1.NewArray()
	defer a1.Release()
	b2 := array.NewInt64Builder(pool)
	defer b2.Release()
	b2.AppendValues([]int64{4, 0, 6}, []bool{true, false, true}) // one null at idx 1
	a2 := b2.NewArray()
	defer a2.Release()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	chunked := arrow.NewChunked(a1.DataType(), []arrow.Array{a1, a2})
	s := NewSeries(arrow.NewColumn(field, chunked))

	nulls := s.Nulls()
	if len(nulls) != 6 {
		t.Fatalf("len(nulls) = %d, want 6", len(nulls))
	}
	// Chunk 0: rows 0-2 all valid. Chunk 1: rows 3, 5 valid; row 4 null.
	want := []bool{false, false, false, false, true, false}
	for i, w := range want {
		if nulls[i] != w {
			t.Errorf("nulls[%d] = %v, want %v", i, nulls[i], w)
		}
	}
	if got := s.NullCount(); got != 1 {
		t.Errorf("NullCount = %d, want 1", got)
	}
}

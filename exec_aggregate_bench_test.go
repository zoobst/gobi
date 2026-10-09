package gobi

import (
	"context"
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// vecAggFrame builds a single-chunk Frame of nGroups × rowsPerGroup
// with a string key column and a Float64 value column. The
// single-chunk shape is what the vectorized Update fast paths hit —
// multi-chunk falls through to the numericAt walker unchanged.
func vecAggFrame(b testing.TB, nGroups, rowsPerGroup int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nRows := nGroups * rowsPerGroup

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	valueB := array.NewFloat64Builder(pool)
	defer valueB.Release()
	for g := range nGroups {
		region := fmt.Sprintf("g%06d", g)
		for r := range rowsPerGroup {
			regionB.Append(region)
			valueB.Append(float64(r + 1))
		}
	}
	_ = nRows

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), valueB.NewArray()}
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
		b.Fatal(err)
	}
	return f
}

// BenchmarkAggregate_SumFloat64 — GroupBy(region).Agg(Sum(value)) on
// a single-chunk Float64 fixture. Exercises the vectorized sumAcc
// fast path.
func BenchmarkAggregate_SumFloat64(b *testing.B) {
	f := vecAggFrame(b, 100, 1000) // 100k rows, 100 groups
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().
			GroupBy("region").
			Agg(Aggregation{Column: "value", Kind: AggSum, Alias: "sum_v"}).
			Plan()))
		if err != nil {
			b.Fatal(err)
		}
		out, err := Execute(ctx, op)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkAggregate_MeanFloat64 — Mean via the vectorized meanAcc
// fast path.
func BenchmarkAggregate_MeanFloat64(b *testing.B) {
	f := vecAggFrame(b, 100, 1000)
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().
			GroupBy("region").
			Agg(Aggregation{Column: "value", Kind: AggMean, Alias: "mean_v"}).
			Plan()))
		if err != nil {
			b.Fatal(err)
		}
		out, err := Execute(ctx, op)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkAggregate_MinMaxFloat64 — Min + Max via vectorized minMaxAcc.
func BenchmarkAggregate_MinMaxFloat64(b *testing.B) {
	f := vecAggFrame(b, 100, 1000)
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().
			GroupBy("region").
			Agg(
				Aggregation{Column: "value", Kind: AggMin, Alias: "min_v"},
				Aggregation{Column: "value", Kind: AggMax, Alias: "max_v"},
			).
			Plan()))
		if err != nil {
			b.Fatal(err)
		}
		out, err := Execute(ctx, op)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// singleChunkFloat64Series builds a single-chunk Float64 Series
// exercising the vectorized sumAcc/meanAcc/minMaxAcc fast path.
func singleChunkFloat64Series(b testing.TB, n int) Series {
	b.Helper()
	pool := memory.DefaultAllocator
	fb := array.NewFloat64Builder(pool)
	defer fb.Release()
	for i := range n {
		fb.Append(float64(i))
	}
	arr := fb.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked))
}

// multiChunkFloat64Series builds the same values as
// singleChunkFloat64Series but split across two chunks so the
// vectorized fast path bails to the numericAt walker.
func multiChunkFloat64Series(b testing.TB, n int) Series {
	b.Helper()
	pool := memory.DefaultAllocator
	half := n / 2
	fb1 := array.NewFloat64Builder(pool)
	defer fb1.Release()
	for i := range half {
		fb1.Append(float64(i))
	}
	fb2 := array.NewFloat64Builder(pool)
	defer fb2.Release()
	for i := half; i < n; i++ {
		fb2.Append(float64(i))
	}
	a1 := fb1.NewArray()
	defer a1.Release()
	a2 := fb2.NewArray()
	defer a2.Release()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	chunked := arrow.NewChunked(a1.DataType(), []arrow.Array{a1, a2})
	return NewSeries(arrow.NewColumn(field, chunked))
}

// BenchmarkSumAcc_VectorizedSingleChunk isolates the sumAcc.Update
// vectorized fast path.
func BenchmarkSumAcc_VectorizedSingleChunk(b *testing.B) {
	s := singleChunkFloat64Series(b, 1_000_000)
	rows := make([]int, s.Len())
	for i := range rows {
		rows[i] = i
	}
	b.ReportAllocs()

	for b.Loop() {
		acc := &sumAcc{}
		if err := acc.Update(s, rows); err != nil {
			b.Fatal(err)
		}
		_ = acc.Finalize()
	}
}

// BenchmarkSumAcc_NumericAtMultiChunk exercises the fallback path.
// Same data but multi-chunk, so vec fast path bails to numericAt.
func BenchmarkSumAcc_NumericAtMultiChunk(b *testing.B) {
	s := multiChunkFloat64Series(b, 1_000_000)
	rows := make([]int, s.Len())
	for i := range rows {
		rows[i] = i
	}
	b.ReportAllocs()

	for b.Loop() {
		acc := &sumAcc{}
		if err := acc.Update(s, rows); err != nil {
			b.Fatal(err)
		}
		_ = acc.Finalize()
	}
}

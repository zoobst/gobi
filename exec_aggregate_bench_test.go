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

// groupSweepFrame is the group-count sweep fixture: 200,000 rows, a
// string key column "key" with `groups` distinct values (rows assigned
// round-robin, so with groups == rows every row is its own group), an
// Int64 "v", and four String attribute columns a0..a3.
func groupSweepFrame(b testing.TB, groups int) (*Frame, []string, []int64) {
	b.Helper()
	const rows = 200_000
	pool := memory.DefaultAllocator
	keyNames := make([]string, groups)
	for g := range keyNames {
		keyNames[g] = fmt.Sprintf("cell-%08d", g*7919%groups)
	}
	keys := make([]string, rows)
	vals := make([]int64, rows)
	kb := array.NewStringBuilder(pool)
	vb := array.NewInt64Builder(pool)
	ab := make([]*array.StringBuilder, 4)
	for i := range ab {
		ab[i] = array.NewStringBuilder(pool)
	}
	for r := range rows {
		keys[r] = keyNames[r%groups]
		vals[r] = int64(r % 97)
		kb.Append(keys[r])
		vb.Append(vals[r])
		for i, a := range ab {
			a.Append(fmt.Sprintf("attr%d-%d", i, r%1000))
		}
	}
	builders := []array.Builder{kb, vb, ab[0], ab[1], ab[2], ab[3]}
	names := []string{"key", "v", "a0", "a1", "a2", "a3"}
	fields := make([]arrow.Field, len(builders))
	cols := make([]arrow.Column, len(builders))
	for i, bld := range builders {
		arr := bld.NewArray()
		bld.Release()
		fields[i] = arrow.Field{Name: names[i], Type: arr.DataType(), Nullable: true}
		ch := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		cols[i] = *arrow.NewColumn(fields[i], ch)
		ch.Release()
		arr.Release()
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		b.Fatal(err)
	}
	return f, keys, vals
}

// BenchmarkGroupBy_GroupCountSweep tracks the streaming aggregate's
// per-group cost: a lazy GroupBy over 200,000 rows as the number of
// distinct keys goes from 100 to one per row. "map_floor" is a plain
// Go map fold over the same columns — no Arrow output, no key sort —
// as the reference point. The unordered variants skip the key sort
// (LazyGroupBy.Unordered).
func BenchmarkGroupBy_GroupCountSweep(b *testing.B) {
	for _, groups := range []int{100, 20_000, 200_000} {
		f, keys, vals := groupSweepFrame(b, groups)
		sum := []Aggregation{{Column: "v", Kind: AggSum, Alias: "v"}}
		sumFirst := append([]Aggregation{}, sum...)
		for _, c := range []string{"a0", "a1", "a2", "a3"} {
			sumFirst = append(sumFirst, Aggregation{Column: c, Kind: AggFirst, Alias: c})
		}
		for name, aggs := range map[string][]Aggregation{"sum": sum, "sum+4first": sumFirst} {
			for _, unordered := range []bool{false, true} {
				label := name
				if unordered {
					label += "/unordered"
				}
				b.Run(fmt.Sprintf("keys=%d/%s", groups, label), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						gb := f.Lazy().GroupBy("key")
						if unordered {
							gb = gb.Unordered()
						}
						out, err := gb.Agg(aggs...).Collect()
						if err != nil {
							b.Fatal(err)
						}
						out.Release()
					}
				})
			}
		}
		b.Run(fmt.Sprintf("keys=%d/map_floor", groups), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m := make(map[string]int64)
				for i, k := range keys {
					m[k] += vals[i]
				}
			}
		})
		f.Release()
	}
}

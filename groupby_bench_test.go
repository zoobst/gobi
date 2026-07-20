package gobi

import (
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// buildBenchGroupFrame returns a frame with n rows, `groups` distinct string
// keys, and two numeric columns (revenue float64, units int64).
func buildBenchGroupFrame(b *testing.B, n, groups int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	keyB := array.NewStringBuilder(pool)
	defer keyB.Release()
	revB := array.NewFloat64Builder(pool)
	defer revB.Release()
	uniB := array.NewInt64Builder(pool)
	defer uniB.Release()

	for i := range n {
		keyB.Append(fmt.Sprintf("k%d", i%groups))
		revB.Append(float64(i) * 0.5)
		uniB.Append(int64(i))
	}
	fields := []arrow.Field{
		{Name: "key", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "revenue", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "units", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{keyB.NewArray(), revB.NewArray(), uniB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 3)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		b.Fatal(err)
	}
	return f
}

func BenchmarkGroupBy_100k_by_100(b *testing.B) {
	f := buildBenchGroupFrame(b, 100_000, 100)

	b.ReportAllocs()
	for b.Loop() {
		gb, err := f.GroupBy("key")
		if err != nil {
			b.Fatal(err)
		}
		out, err := gb.Agg(
			Aggregation{Column: "revenue", Kind: AggSum},
			Aggregation{Column: "revenue", Kind: AggMean},
			Aggregation{Column: "units", Kind: AggMax},
		)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

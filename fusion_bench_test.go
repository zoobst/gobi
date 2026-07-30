package gobi

import (
	"context"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// chainedOpBenchFrame builds a numeric Frame for stress-testing the
// streaming batch-transform chain. nRows × nCols shape — wide frames
// magnify the per-batch Frame↔batch column-header conversion cost
// each fused op boundary would save.
func chainedOpBenchFrame(b testing.TB, nRows, nCols int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	fields := make([]arrow.Field, nCols)
	arrs := make([]arrow.Array, nCols)
	for c := range nCols {
		fb := array.NewFloat64Builder(pool)
		for i := range nRows {
			fb.Append(float64(i + c))
		}
		arrs[c] = fb.NewArray()
		fields[c] = arrow.Field{
			Name:     fmtCol(c),
			Type:     arrow.PrimitiveTypes.Float64,
			Nullable: false,
		}
		fb.Release()
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	schema := arrow.NewSchema(fields, nil)
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

func fmtCol(c int) string {
	buf := []byte{'c'}
	if c == 0 {
		return "c0"
	}
	// Simple base-10 encode.
	digits := []byte{}
	for c > 0 {
		digits = append([]byte{byte('0' + c%10)}, digits...)
		c /= 10
	}
	return string(append(buf, digits...))
}

// BenchmarkFusion_ChainedOps — 4-op streaming chain
// (WithColumn.WithColumn.WithColumn.Filter). Compiled via Compile()
// which invokes fuseStreamChains, so this measures the fused path.
func BenchmarkFusion_ChainedOps(b *testing.B) {
	f := chainedOpBenchFrame(b, 200_000, 20)
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().
			WithColumn("a", Col("c0").Add(Lit(1.0))).
			WithColumn("b", Col("c1").Mul(Lit(2.0))).
			WithColumn("d", Col("c0").Sub(Col("c1"))).
			Filter(Col("a").Gt(Lit(0.5))).
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

// BenchmarkFusion_ChainedOpsUnfused — same 4-op chain, but compiled
// via compileNode directly, bypassing fuseStreamChains. Provides the
// baseline for measuring the fusion win.
func BenchmarkFusion_ChainedOpsUnfused(b *testing.B) {
	f := chainedOpBenchFrame(b, 200_000, 20)
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := compileNode(Optimize(f.Lazy().
			WithColumn("a", Col("c0").Add(Lit(1.0))).
			WithColumn("b", Col("c1").Mul(Lit(2.0))).
			WithColumn("d", Col("c0").Sub(Col("c1"))).
			Filter(Col("a").Gt(Lit(0.5))).
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

package gobi

import (
	"math/rand/v2"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// BenchmarkAndFusion_Range_100k — Slice 22b two-sided range on
// a 100k-row Float64 column via `x >= lo AND x <= hi`. Fast path
// dispatches to compute.AndChainF64Range; baseline runs two
// scalar comparisons + a boolean AND.
func BenchmarkAndFusion_Range_100k(b *testing.B) {
	f := buildRandF64Frame(b, "x", 100_000)
	expr := Col("x").Ge(Lit(200.0)).And(Col("x").Le(Lit(800.0)))
	b.ReportAllocs()
	for b.Loop() {
		out, err := f.FilterExpr(expr)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkAndFusion_BBox_100k — Slice 22c four-cmp bbox filter.
// Two Float64 columns; four scalar comparisons ANDed together.
// Fast path dispatches to compute.AndChainF64BBox.
func BenchmarkAndFusion_BBox_100k(b *testing.B) {
	f := buildRandF64PairFrame(b, "x", "y", 100_000)
	expr := Col("x").Ge(Lit(200.0)).And(Col("x").Le(Lit(800.0))).
		And(Col("y").Ge(Lit(200.0)).And(Col("y").Le(Lit(800.0))))
	b.ReportAllocs()
	for b.Loop() {
		out, err := f.FilterExpr(expr)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkAndFusion_Range_100k_LegacyPath — reproduces the
// pre-Slice-22 shape: evaluate two scalar comparisons into two
// boolean columns, then AND them elementwise into a third. Delta
// vs the SoA bench above is the Slice 22b fusion win.
func BenchmarkAndFusion_Range_100k_LegacyPath(b *testing.B) {
	f := buildRandF64Frame(b, "x", 100_000)
	geomCol, _ := f.Column("x")
	b.ReportAllocs()
	for b.Loop() {
		left, err := geomCol.GeScalar(200.0)
		if err != nil {
			b.Fatal(err)
		}
		right, err := geomCol.LeScalar(800.0)
		if err != nil {
			b.Fatal(err)
		}
		mask, err := boolBinary(left, right, func(a, b bool) bool { return a && b })
		if err != nil {
			b.Fatal(err)
		}
		out, err := f.Filter(mask)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func BenchmarkAndFusion_BBox_100k_LegacyPath(b *testing.B) {
	f := buildRandF64PairFrame(b, "x", "y", 100_000)
	xCol, _ := f.Column("x")
	yCol, _ := f.Column("y")
	b.ReportAllocs()
	for b.Loop() {
		xLo, _ := xCol.GeScalar(200.0)
		xHi, _ := xCol.LeScalar(800.0)
		yLo, _ := yCol.GeScalar(200.0)
		yHi, _ := yCol.LeScalar(800.0)
		xMask, _ := boolBinary(xLo, xHi, func(a, b bool) bool { return a && b })
		yMask, _ := boolBinary(yLo, yHi, func(a, b bool) bool { return a && b })
		mask, _ := boolBinary(xMask, yMask, func(a, b bool) bool { return a && b })
		out, err := f.Filter(mask)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func buildRandF64Frame(b testing.TB, name string, n int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	bld := array.NewFloat64Builder(pool)
	defer bld.Release()
	rng := rand.New(rand.NewPCG(0xF00, 0xBAA))
	for range n {
		bld.Append(rng.Float64() * 1000)
	}
	arr := bld.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	return frameFromArrays(b, []arrow.Field{field}, []arrow.Array{arr})
}

func buildRandF64PairFrame(b testing.TB, xName, yName string, n int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	xb := array.NewFloat64Builder(pool)
	yb := array.NewFloat64Builder(pool)
	defer xb.Release()
	defer yb.Release()
	rng := rand.New(rand.NewPCG(0xF00, 0xBAA))
	for range n {
		xb.Append(rng.Float64() * 1000)
		yb.Append(rng.Float64() * 1000)
	}
	xa := xb.NewArray()
	ya := yb.NewArray()
	defer xa.Release()
	defer ya.Release()
	fields := []arrow.Field{
		{Name: xName, Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: yName, Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	return frameFromArrays(b, fields, []arrow.Array{xa, ya})
}

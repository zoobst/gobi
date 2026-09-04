package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestAndFusion_RangeParity — Slice 22b: two-sided range fusion
// must produce identical masks to the two-step composition.
func TestAndFusion_RangeParity(t *testing.T) {
	f := buildF64Frame(t, "x", []float64{-5, 0, 1, 2, 3, 5, 7, 10, 15, 20})
	// Inclusive range 1 ≤ x ≤ 10.
	cases := []struct {
		name string
		expr Expr
		want []bool
	}{
		{
			"ge-and-le",
			Col("x").Ge(Lit(1.0)).And(Col("x").Le(Lit(10.0))),
			[]bool{false, false, true, true, true, true, true, true, false, false},
		},
		{
			"gt-and-lt",
			Col("x").Gt(Lit(1.0)).And(Col("x").Lt(Lit(10.0))),
			[]bool{false, false, false, true, true, true, true, false, false, false},
		},
		{
			"reversed-order",
			Col("x").Le(Lit(10.0)).And(Col("x").Ge(Lit(1.0))),
			[]bool{false, false, true, true, true, true, true, true, false, false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evalBool(t, f, c.expr)
			if len(got) != len(c.want) {
				t.Fatalf("len got=%d want=%d", len(got), len(c.want))
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("row %d: got %v want %v (x=%v)",
						i, got[i], c.want[i], -5+float64(i)*2.5)
				}
			}
		})
	}
}

// TestAndFusion_BBoxParity — Slice 22c: 4-cmp bbox fusion must
// match the naive 3-AND composition.
func TestAndFusion_BBoxParity(t *testing.T) {
	f := buildBBoxFrame(t)
	// Rows are (x, y) pairs: (-5,-5), (0,0), (2,2), (5,5), (10,10).
	// Bbox: 0 ≤ x ≤ 5 AND 0 ≤ y ≤ 5.
	expr := Col("x").Ge(Lit(0.0)).And(Col("x").Le(Lit(5.0))).
		And(Col("y").Ge(Lit(0.0)).And(Col("y").Le(Lit(5.0))))
	got := evalBool(t, f, expr)
	want := []bool{false, true, true, true, false}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %v want %v", i, got[i], want[i])
		}
	}
}

// TestAndFusion_FallthroughOnNulls — a column with nulls must not
// take the fast path (which assumes non-null); the general two-step
// AND handles nulls correctly and must be preserved.
func TestAndFusion_FallthroughOnNulls(t *testing.T) {
	pool := memory.DefaultAllocator
	bld := array.NewFloat64Builder(pool)
	defer bld.Release()
	bld.Append(1.0)
	bld.AppendNull()
	bld.Append(5.0)
	arr := bld.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Float64, Nullable: true}
	f := frameFromArrays(t, []arrow.Field{field}, []arrow.Array{arr})
	expr := Col("x").Ge(Lit(0.0)).And(Col("x").Le(Lit(10.0)))
	got := evalBool(t, f, expr)
	want := []any{true, nil, true}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	// evalBool returns []any (bool or nil) — compare directly.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %v want %v", i, got[i], want[i])
		}
	}
}

// TestAndFusion_MixedColumnsFallsThrough — `Col(x)>0 AND Col(y)>0`
// (different columns, same-side ops) doesn't form a valid range;
// must fall through to general AND without error.
func TestAndFusion_MixedColumnsFallsThrough(t *testing.T) {
	f := buildBBoxFrame(t)
	expr := Col("x").Gt(Lit(0.0)).And(Col("y").Gt(Lit(0.0)))
	got := evalBool(t, f, expr)
	// Rows: (-5,-5), (0,0), (2,2), (5,5), (10,10). Only rows where
	// both x > 0 and y > 0 → 2,2 and 5,5 and 10,10.
	want := []bool{false, false, true, true, true}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %v want %v", i, got[i], want[i])
		}
	}
}

func buildF64Frame(t testing.TB, name string, vals []float64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	bld := array.NewFloat64Builder(pool)
	defer bld.Release()
	for _, v := range vals {
		bld.Append(v)
	}
	arr := bld.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	return frameFromArrays(t, []arrow.Field{field}, []arrow.Array{arr})
}

func buildBBoxFrame(t testing.TB) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	xb := array.NewFloat64Builder(pool)
	yb := array.NewFloat64Builder(pool)
	defer xb.Release()
	defer yb.Release()
	for _, xy := range [][2]float64{{-5, -5}, {0, 0}, {2, 2}, {5, 5}, {10, 10}} {
		xb.Append(xy[0])
		yb.Append(xy[1])
	}
	xa := xb.NewArray()
	ya := yb.NewArray()
	defer xa.Release()
	defer ya.Release()
	fields := []arrow.Field{
		{Name: "x", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "y", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	return frameFromArrays(t, fields, []arrow.Array{xa, ya})
}

func frameFromArrays(t testing.TB, fields []arrow.Field, arrs []arrow.Array) *Frame {
	t.Helper()
	schema := arrow.NewSchema(fields, nil)
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	// Frame owns one ref per Column (transferred via NewColumn +
	// chunked.Release above). Without Release, the underlying
	// chunked buffer stays alive until the process exits — a
	// per-frame test-only leak that hides under Arrow's
	// process-lifetime pool. Wire it to the test's cleanup so
	// -race + -count=100 doesn't grow RSS with every rerun.
	t.Cleanup(f.Release)
	return f
}

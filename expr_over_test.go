package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// overFrame builds a small (group, value) frame:
//
//	group  v
//	A      1
//	A      2
//	B      10
//	A      3
//	B      20
//	C      100
//
// Interleaved so tests can verify row-order preservation (Over must
// not sort or reshuffle).
func overFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	gb := array.NewStringBuilder(pool)
	defer gb.Release()
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	gb.AppendValues([]string{"A", "A", "B", "A", "B", "C"}, nil)
	vb.AppendValues([]int64{1, 2, 10, 3, 20, 100}, nil)

	fields := []arrow.Field{
		{Name: "group", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{gb.NewArray(), vb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOver_SumBroadcasts(t *testing.T) {
	f := overFrame(t)
	out, err := f.WithColumnExpr("group_sum", Col("v").Sum().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	// Row count preserved.
	if r, _ := out.Shape(); r != 6 {
		t.Fatalf("row count = %d, want 6 (Over must not reshape)", r)
	}
	// Group sums: A = 1+2+3 = 6; B = 10+20 = 30; C = 100.
	// The frame has 3 A rows, 2 B rows, 1 C row in that interleaved order.
	sumS, _ := out.Column("group_sum")
	arr := sumS.col.Data().Chunks()[0].(*array.Float64) // AggSum output is Float64
	want := []float64{6, 6, 30, 6, 30, 100}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d group_sum = %v, want %v", i, arr.Value(i), w)
		}
	}
}

func TestOver_PreservesRowOrder(t *testing.T) {
	f := overFrame(t)
	out, err := f.WithColumnExpr("mean", Col("v").Mean().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	// The v column should remain byte-identical (Over doesn't touch
	// the other columns).
	vS, _ := out.Column("v")
	vArr := vS.col.Data().Chunks()[0].(*array.Int64)
	wantV := []int64{1, 2, 10, 3, 20, 100}
	for i, w := range wantV {
		if vArr.Value(i) != w {
			t.Fatalf("Over reshuffled the input: row %d v = %d, want %d", i, vArr.Value(i), w)
		}
	}
	// And the mean column follows row-partition:
	// A: mean(1,2,3) = 2.0; B: mean(10,20) = 15.0; C: 100.0.
	meanArr := out.series[2].col.Data().Chunks()[0].(*array.Float64)
	wantMean := []float64{2, 2, 15, 2, 15, 100}
	for i, w := range wantMean {
		if meanArr.Value(i) != w {
			t.Errorf("row %d mean = %v, want %v", i, meanArr.Value(i), w)
		}
	}
}

func TestOver_MinMaxCount(t *testing.T) {
	f := overFrame(t)
	out, err := f.WithColumnExpr("mn", Col("v").MinAgg().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	// AggMin output type on Int64 input is Float64 (matches the eager
	// engine's normalization).
	mnArr := out.series[2].col.Data().Chunks()[0].(*array.Float64)
	wantMn := []float64{1, 1, 10, 1, 10, 100}
	for i, w := range wantMn {
		if mnArr.Value(i) != w {
			t.Errorf("row %d min = %v, want %v", i, mnArr.Value(i), w)
		}
	}

	out, err = f.WithColumnExpr("mx", Col("v").MaxAgg().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	mxArr := out.series[2].col.Data().Chunks()[0].(*array.Float64)
	wantMx := []float64{3, 3, 20, 3, 20, 100}
	for i, w := range wantMx {
		if mxArr.Value(i) != w {
			t.Errorf("row %d max = %v, want %v", i, mxArr.Value(i), w)
		}
	}

	out, err = f.WithColumnExpr("n", Col("v").Count().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	nArr := out.series[2].col.Data().Chunks()[0].(*array.Int64)
	wantN := []int64{3, 3, 2, 3, 2, 1}
	for i, w := range wantN {
		if nArr.Value(i) != w {
			t.Errorf("row %d count = %d, want %d", i, nArr.Value(i), w)
		}
	}
}

func TestOver_MultiKeyPartition(t *testing.T) {
	pool := memory.DefaultAllocator

	// Two partition keys (group, tag). Cross-product creates 4
	// distinct groups.
	gb := array.NewStringBuilder(pool)
	defer gb.Release()
	tb := array.NewStringBuilder(pool)
	defer tb.Release()
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	gb.AppendValues([]string{"A", "A", "A", "A", "B", "B"}, nil)
	tb.AppendValues([]string{"x", "y", "x", "y", "x", "x"}, nil)
	vb.AppendValues([]int64{1, 10, 2, 20, 100, 200}, nil)

	fields := []arrow.Field{
		{Name: "g", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "t", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{gb.NewArray(), tb.NewArray(), vb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	out, err := f.WithColumnExpr("s", Col("v").Sum().Over("g", "t"))
	if err != nil {
		t.Fatal(err)
	}
	// Groups:
	//   (A,x): rows 0, 2 → sum = 3
	//   (A,y): rows 1, 3 → sum = 30
	//   (B,x): rows 4, 5 → sum = 300
	sumArr := out.series[3].col.Data().Chunks()[0].(*array.Float64)
	want := []float64{3, 30, 3, 30, 300, 300}
	for i, w := range want {
		if sumArr.Value(i) != w {
			t.Errorf("row %d sum = %v, want %v", i, sumArr.Value(i), w)
		}
	}
}

func TestOver_RejectsNonAggregate(t *testing.T) {
	f := overFrame(t)
	// Col("v").Over(...) skips the aggregate — should error.
	_, err := f.WithColumnExpr("bad", Col("v").Over("group"))
	if err == nil {
		t.Fatal("expected error when Over wraps a non-aggregate expression")
	}
}

func TestOver_ArithmeticComposition(t *testing.T) {
	// Over composes with arithmetic: (v - v.mean().over(g)) computes
	// mean-centered values per group.
	f := overFrame(t)
	out, err := f.WithColumnExpr("centered",
		Col("v").Sub(Col("v").Mean().Over("group")))
	if err != nil {
		t.Fatal(err)
	}
	// Group means: A=2, B=15, C=100. Centered = v - mean.
	arr := out.series[2].col.Data().Chunks()[0].(*array.Float64)
	want := []float64{1 - 2, 0, 10 - 15, 3 - 2, 20 - 15, 0}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d centered = %v, want %v", i, arr.Value(i), w)
		}
	}
}

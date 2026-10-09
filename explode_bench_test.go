package gobi

import (
	"context"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// explodeBenchFrame builds a fixture large enough to span multiple
// default-sized batches: nRows rows each holding a listSize-element
// List<Int64>. Post-Explode = nRows * listSize rows.
func explodeBenchFrame(b testing.TB, nRows, listSize int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	for i := range nRows {
		idB.Append(int64(i))
	}

	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.Int64Builder)
	for i := range nRows {
		lb.Append(true)
		for j := range listSize {
			vb.Append(int64(i*listSize + j))
		}
	}

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "items", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), lb.NewArray()}
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

// BenchmarkExplode_Streaming — the new per-batch explodeExecOp path.
// Compare against Explode_Materialize below (uses a wrapped node that
// forces the old materializing behavior) to isolate the win.
func BenchmarkExplode_Streaming(b *testing.B) {
	f := explodeBenchFrame(b, 10000, 3) // 10k rows × 3 list-elem = 30k exploded rows
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().Explode("items").Plan()))
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

// BenchmarkExplode_Materialize — force the old materializing path
// via a hand-built materializeExecOp that wraps the scan directly and
// calls Frame.Explode on the concatenated Frame. Provides the
// pre-optimization baseline for the same fixture.
func BenchmarkExplode_Materialize(b *testing.B) {
	f := explodeBenchFrame(b, 10000, 3)
	ctx := context.Background()
	// Compute the target schema once so we can inject the same shape
	// into a materializeExecOp for a fair comparison.
	explodeName := "items"
	planSchema := explodeItemsSchema(f)
	b.ReportAllocs()

	for b.Loop() {
		lf := f.Lazy() // rebuild plan each iter to match streaming bench
		child, err := Compile(Optimize(lf.Plan()))
		if err != nil {
			b.Fatal(err)
		}
		op := &materializeExecOp{
			input:     child,
			outSchema: planSchema,
			compute: func(fr *Frame) (*Frame, error) {
				return fr.Explode(explodeName)
			},
		}
		out, err := Execute(ctx, op)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// explodeItemsSchema derives the post-Explode schema for the bench
// fixture — items becomes an Int64 column, id stays Int64.
func explodeItemsSchema(_ *Frame) *arrow.Schema {
	return arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "items", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}, nil)
}

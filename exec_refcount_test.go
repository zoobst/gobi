package gobi

import (
	"context"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestFrameToBatch_RefcountBalanced verifies that frameToBatch does not
// over-Retain the underlying arrow arrays. Historically, frameToBatch
// called both `arr.Retain()` explicitly and relied on NewRecordBatch's
// internal Retain — the extra Retain leaked one refcount per column
// per call, which compounded across every batch of every streaming
// pipeline. This test guards the invariant that a matching pair of
// (batch.Release, Frame.Release) brings the array refcount to zero.
func TestFrameToBatch_RefcountBalanced(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64Frame(t, pool, "x", []int64{1, 2, 3, 4})

	batch := frameToBatch(f)
	// Source Frame and Batch each own an independent ref chain. Release
	// both to drive the array refcount to zero. Any extra Retain inside
	// frameToBatch surfaces here as a nonzero CheckedAllocator size in
	// the deferred AssertSize.
	batch.Release()
	f.Release()
}

// TestScanFrameExec_ReleasesSliceFrames drives scanFrameExec to EOF and
// asserts that every per-batch slice Frame's refs are Released. Before
// v0.2.22 each slice() leaked its Column/Chunked ref chain.
func TestScanFrameExec_ReleasesSliceFrames(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64Frame(t, pool, "x", []int64{1, 2, 3, 4, 5, 6, 7, 8})
	defer f.Release()

	op := newScanFrameExec(f, 3) // 3 batches: 3+3+2
	defer op.Close()

	ctx := context.Background()
	for {
		batch, err := op.Next(ctx)
		if err != nil {
			break
		}
		batch.Release()
	}
}

// TestFilterExec_ReleasesIntermediateFrames drives a filter over a
// scan and asserts the per-batch intermediate `frame` and `filtered`
// don't leak. Before v0.2.22 both were orphaned on every batch that
// produced non-zero output.
func TestFilterExec_ReleasesIntermediateFrames(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64Frame(t, pool, "x", []int64{1, 2, 3, 4, 5, 6, 7, 8})
	defer f.Release()

	scan := newScanFrameExec(f, 3)
	// Filter for x >= 3 — matches on some batches, empty on others.
	op := &filterExecOp{input: scan, cond: Col("x").Ge(Lit(int64(3)))}
	defer op.Close()

	got, err := Execute(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	got.Release()
}

// TestProjectExec_ReleasesIntermediateFrames — analog of the filter
// test for the project op path.
func TestProjectExec_ReleasesIntermediateFrames(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64Frame(t, pool, "x", []int64{1, 2, 3, 4, 5, 6, 7, 8})
	defer f.Release()

	scan := newScanFrameExec(f, 3)
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "y", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}, nil)
	op := &projectExecOp{
		input:     scan,
		exprs:     []Expr{Col("x").Mul(Lit(int64(2))).Alias("y")},
		outSchema: schema,
	}
	defer op.Close()

	got, err := Execute(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	got.Release()
}

// TestWithColumnExec_ReleasesIntermediateFrames covers the
// withColumnExecOp intermediate-Frame path.
func TestWithColumnExec_ReleasesIntermediateFrames(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64Frame(t, pool, "x", []int64{1, 2, 3, 4, 5, 6, 7, 8})
	defer f.Release()

	scan := newScanFrameExec(f, 3)
	outFields := []arrow.Field{
		{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "doubled", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	op := &withColumnExecOp{
		input:     scan,
		name:      "doubled",
		expr:      Col("x").Mul(Lit(int64(2))),
		outSchema: arrow.NewSchema(outFields, nil),
	}
	defer op.Close()

	got, err := Execute(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	got.Release()
}

// TestLazy_FilterSelect_RefcountBalanced runs the full lazy pipeline —
// Filter + Select + Collect — under a CheckedAllocator. Catches
// refcount leaks anywhere in the compile-through-execute path,
// including the fused-streaming op that chains multiple frame
// appliers per batch.
func TestLazy_FilterSelect_RefcountBalanced(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64Frame(t, pool, "x", []int64{1, 2, 3, 4, 5, 6, 7, 8})

	out, err := f.Lazy().
		Filter(Col("x").Ge(Lit(int64(3)))).
		Select(Col("x").Mul(Lit(int64(10))).Alias("y")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	out.Release()
	f.Release()
}

// buildI64Frame constructs a single-column Int64 Frame whose arrow
// buffers are all allocated from pool. Used by the refcount tests to
// tie the source data's lifetime to the pool so pool.AssertSize(0)
// reports any leaked buffer.
func buildI64Frame(t *testing.T, pool memory.Allocator, colName string, vals []int64) *Frame {
	t.Helper()

	b := array.NewInt64Builder(pool)
	b.AppendValues(vals, nil)
	arr := b.NewArray()
	b.Release()

	field := arrow.Field{Name: colName, Type: arrow.PrimitiveTypes.Int64, Nullable: false}
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	arr.Release()
	chunked.Release()

	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

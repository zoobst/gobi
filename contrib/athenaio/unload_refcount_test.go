package athenaio

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
)

// TestConcatFramesSingleChunk_ReleasesInputs guards the invariant
// that concatFramesSingleChunk consumes its input frames. Before
// v0.2.22 the concat portion of readBucketFiles held N source Frames
// in a slice with no Release path — a straight ~800 MB per bucket
// leak on UnloadAndRead workloads that touched multi-GB CTAS outputs.
//
// The test allocates the source Frames' arrow buffers from a
// CheckedAllocator so any missed Release surfaces as a non-zero
// pool size in the deferred AssertSize.
func TestConcatFramesSingleChunk_ReleasesInputs(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f1 := buildI64FrameForTest(t, pool, "x", []int64{1, 2, 3})
	f2 := buildI64FrameForTest(t, pool, "x", []int64{4, 5, 6})

	out, err := concatFramesSingleChunk([]*gobi.Frame{f1, f2}, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumRows(); got != 6 {
		t.Fatalf("concat rows = %d, want 6", got)
	}
	out.Release()
}

// TestConcatFramesSingleChunk_ErrorPathReleasesInputs verifies that
// concatFramesSingleChunk Releases its inputs even on the error path
// (schema mismatch, in this case). Missing that Release would leak
// on every failed concat attempt.
func TestConcatFramesSingleChunk_ErrorPathReleasesInputs(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	// Different column names → Concatenate rejects with a type error
	// (schema[0].Fields[0].Name mismatch propagates through
	// s.Column().Data().Chunks()... yielding arrays of mismatched
	// physical types when there are multiple columns; here we induce
	// the error via an incompatible column type in position 0).
	f1 := buildI64FrameForTest(t, pool, "x", []int64{1, 2, 3})
	f2 := buildF64FrameForTest(t, pool, "x", []float64{4, 5, 6})

	if _, err := concatFramesSingleChunk([]*gobi.Frame{f1, f2}, pool); err == nil {
		t.Fatal("expected concat error on mismatched types")
	}
	// Both inputs should still have been Released — deferred AssertSize
	// will fail otherwise.
}

// buildI64FrameForTest constructs a single-column Int64 Frame whose
// arrow buffers are all allocated from pool.
func buildI64FrameForTest(t *testing.T, pool memory.Allocator, colName string, vals []int64) *gobi.Frame {
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

	f, err := gobi.NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// buildF64FrameForTest is the Float64 analog — used to drive the
// concat error path in TestConcatFramesSingleChunk_ErrorPathReleasesInputs.
func buildF64FrameForTest(t *testing.T, pool memory.Allocator, colName string, vals []float64) *gobi.Frame {
	t.Helper()

	b := array.NewFloat64Builder(pool)
	b.AppendValues(vals, nil)
	arr := b.NewArray()
	b.Release()

	field := arrow.Field{Name: colName, Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	arr.Release()
	chunked.Release()

	f, err := gobi.NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

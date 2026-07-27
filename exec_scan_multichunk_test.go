package gobi

import (
	"context"
	"io"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// multiChunkInt64Frame builds a Frame whose Int64 column is chunked
// into per-column pieces. Chunk sizes control how the scan splits.
func multiChunkInt64Frame(t testing.TB, chunkSizes []int) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	chunks := make([]arrow.Array, 0, len(chunkSizes))
	var next int64
	for _, n := range chunkSizes {
		b := array.NewInt64Builder(pool)
		vals := make([]int64, n)
		for i := range vals {
			vals[i] = next
			next++
		}
		b.AppendValues(vals, nil)
		arr := b.NewArray()
		b.Release()
		chunks = append(chunks, arr)
	}
	defer func() {
		for _, a := range chunks {
			a.Release()
		}
	}()
	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	col := arrow.NewColumn(field, arrow.NewChunked(field.Type, chunks))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestScanFrameExec_MultiChunkNoPanic — scanFrameExec on a
// multi-chunk Frame must emit chunk-aligned batches, never a
// multi-chunk slice that trips frameToBatch's single-chunk
// invariant.
func TestScanFrameExec_MultiChunkNoPanic(t *testing.T) {
	// Chunks of 36 + 1000 + 500 rows.
	f := multiChunkInt64Frame(t, []int{36, 1000, 500})
	e := newScanFrameExec(f, 65536)
	defer e.Close()

	var total int
	for {
		batch, err := e.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if batch.NumCols() != 1 {
			t.Fatalf("batch cols = %d, want 1", batch.NumCols())
		}
		arr := batch.Column(0).(*array.Int64)
		if int64(arr.Len()) != batch.NumRows() {
			t.Fatalf("batch column len %d != batch NumRows %d — multi-chunk leak",
				arr.Len(), batch.NumRows())
		}
		total += int(batch.NumRows())
		batch.Release()
	}
	if total != 36+1000+500 {
		t.Fatalf("total rows = %d, want %d", total, 36+1000+500)
	}
}

// TestScanFrameExec_ChunkBoundariesRespected — verify batches never
// cross underlying chunk boundaries. A batchRows cap smaller than
// every chunk still gets emitted chunk-aligned; each batch's row
// count corresponds to one contiguous slice within one chunk.
func TestScanFrameExec_ChunkBoundariesRespected(t *testing.T) {
	f := multiChunkInt64Frame(t, []int{100, 200, 300})
	e := newScanFrameExec(f, 1000) // cap > every chunk
	defer e.Close()

	wantSizes := []int64{100, 200, 300}
	got := make([]int64, 0, 3)
	for {
		batch, err := e.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, batch.NumRows())
		batch.Release()
	}
	if len(got) != len(wantSizes) {
		t.Fatalf("batch count = %d, want %d (sizes %v)", len(got), len(wantSizes), got)
	}
	for i, w := range wantSizes {
		if got[i] != w {
			t.Errorf("batch %d rows = %d, want %d", i, got[i], w)
		}
	}
}

// TestScanFrameExec_BatchRowsCapSubdivides — within a large chunk,
// batchRows still caps batch size. Chunk of 10000 rows with
// batchRows=3000 should produce 4 batches of [3000, 3000, 3000, 1000].
func TestScanFrameExec_BatchRowsCapSubdivides(t *testing.T) {
	f := multiChunkInt64Frame(t, []int{10000})
	e := newScanFrameExec(f, 3000)
	defer e.Close()

	want := []int64{3000, 3000, 3000, 1000}
	got := make([]int64, 0, len(want))
	for {
		batch, err := e.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, batch.NumRows())
		batch.Release()
	}
	if len(got) != len(want) {
		t.Fatalf("batch count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("batch %d = %d, want %d", i, got[i], w)
		}
	}
}

// TestBinOp_Int64ScalarArithPreservesInt64 — regression for a bug
// where binOpNode.Type() declared Int64 for Int64Col.Add(Lit(int64))
// but the runtime scalar fast path (Series.AddScalar) unconditionally
// widened to Float64. Downstream concatBatchesToFrame's
// arrow.NewColumn then panicked on a field/dtype mismatch.
//
// Fix: Series.scalar now takes an Int64 fast path when the input
// column is Int64 single-chunk, the op isn't Div, and the scalar is
// a losslessly-representable int64. That matches promoteNumeric's
// Type() output. Div still widens to Float64 (per IEEE semantics).
//
// This test surfaces the bug end-to-end on multi-chunk input: the
// scan emits multiple batches, each hits the scalar fast path, and
// concatBatchesToFrame's arrow.NewColumn validates that the produced
// column type matches the declared schema field type.
func TestBinOp_Int64ScalarArithPreservesInt64(t *testing.T) {
	f := multiChunkInt64Frame(t, []int{36, 1000, 500})
	out, err := f.Lazy().
		WithColumn("v_plus_1", Col("v").Add(Lit(int64(1)))).
		Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	col, _ := out.Column("v_plus_1")
	if col.DataType().ID() != arrow.INT64 {
		t.Fatalf("v_plus_1 dtype = %s, want INT64 (Int64 preserved through scalar Add)",
			col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	if arr.Value(100) != 101 {
		t.Errorf("row 100 = %d, want 101", arr.Value(100))
	}
	if arr.Value(1500) != 1501 {
		t.Errorf("row 1500 = %d, want 1501", arr.Value(1500))
	}
}

// TestBinOp_Int64ScalarDivWidens — Div is the exception: even
// Int64 / Int64 widens to Float64 per IEEE semantics. Kept explicit
// so future changes don't accidentally preserve Int64 through Div.
func TestBinOp_Int64ScalarDivWidens(t *testing.T) {
	f := multiChunkInt64Frame(t, []int{36, 1000, 500})
	out, err := f.Lazy().
		WithColumn("v_div_2", Col("v").Div(Lit(int64(2)))).
		Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	col, _ := out.Column("v_div_2")
	if col.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("v_div_2 dtype = %s, want FLOAT64 (Div always widens)",
			col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Float64)
	// Row 5: 5 / 2 = 2.5 — proves the widening isn't just cosmetic.
	if arr.Value(5) != 2.5 {
		t.Errorf("row 5 = %v, want 2.5 (Div preserves fractional part)",
			arr.Value(5))
	}
}

// TestScanFrameExec_MultiChunkFilter — a LazyFrame Filter on a
// multi-chunk source Frame collects the right rows. Exercises the
// full scan → op → concatBatchesToFrame loop with an operation that
// preserves schema (unlike arithmetic-typed WithColumns whose type
// inference is orthogonal to the scan fix under test).
func TestScanFrameExec_MultiChunkFilter(t *testing.T) {
	f := multiChunkInt64Frame(t, []int{36, 1000, 500})
	out, err := f.Lazy().
		Filter(Col("v").Gt(Lit(int64(1000)))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Values are 0..1535; strictly greater than 1000 → 535 rows.
	want := int((36 + 1000 + 500) - 1001)
	if out.NumRows() != want {
		t.Fatalf("filtered rows = %d, want %d", out.NumRows(), want)
	}
	col, _ := out.Column("v")
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != 1001 {
		t.Errorf("first surviving row = %d, want 1001", arr.Value(0))
	}
}

// TestScanFrameExec_MultiChunkLazyIdentity — the simplest possible
// end-to-end check: Lazy().Collect() on a multi-chunk Frame should
// round-trip to a single-chunk Frame with identical values. Isolates
// the scan → concatBatchesToFrame path without any expression eval
// stacked on top.
func TestScanFrameExec_MultiChunkLazyIdentity(t *testing.T) {
	f := multiChunkInt64Frame(t, []int{36, 1000, 500})
	out, err := f.Lazy().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 36+1000+500 {
		t.Fatalf("rows = %d, want %d", out.NumRows(), 36+1000+500)
	}
	col, _ := out.Column("v")
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	if arr.Value(100) != 100 {
		t.Errorf("row 100 = %d, want 100 (row index)", arr.Value(100))
	}
	if arr.Value(1500) != 1500 {
		t.Errorf("row 1500 = %d, want 1500", arr.Value(1500))
	}
}

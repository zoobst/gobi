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

// TestFusion_ChainedWithColumnAndFilterFuses — a WithColumn.WithColumn.Filter
// chain compiles to a single fusedStreamExecOp, not three nested ops.
func TestFusion_ChainedWithColumnAndFilterFuses(t *testing.T) {
	f := lazyFrame(t)
	lf := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		WithColumn("tripled", Col("price").Mul(Lit(3.0))).
		Filter(Col("doubled").Gt(Lit(20.0)))
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	fused, ok := op.(*fusedStreamExecOp)
	if !ok {
		t.Fatalf("expected *fusedStreamExecOp, got %T (chain should fuse)", op)
	}
	// Chain should be [withColumn, withColumn, filter] — 3 ops.
	if len(fused.ops) != 3 {
		t.Fatalf("fused ops = %d, want 3", len(fused.ops))
	}
	if _, ok := fused.ops[0].(*withColumnExecOp); !ok {
		t.Errorf("ops[0] = %T, want *withColumnExecOp", fused.ops[0])
	}
	if _, ok := fused.ops[1].(*withColumnExecOp); !ok {
		t.Errorf("ops[1] = %T, want *withColumnExecOp", fused.ops[1])
	}
	if _, ok := fused.ops[2].(*filterExecOp); !ok {
		t.Errorf("ops[2] = %T, want *filterExecOp", fused.ops[2])
	}
}

// TestFusion_CorrectnessParity — fused pipeline produces same output
// as the equivalent non-lazy eager sequence.
func TestFusion_CorrectnessParity(t *testing.T) {
	f := lazyFrame(t)
	// Eager: apply each op in sequence directly.
	eager := f
	eager, err := eager.WithColumnExpr("doubled", Col("price").Mul(Lit(2.0)))
	if err != nil {
		t.Fatal(err)
	}
	eager, err = eager.WithColumnExpr("tripled", Col("price").Mul(Lit(3.0)))
	if err != nil {
		t.Fatal(err)
	}
	eager, err = eager.FilterExpr(Col("doubled").Gt(Lit(20.0)))
	if err != nil {
		t.Fatal(err)
	}
	// Streaming: same ops through the fused executor.
	streaming, err := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		WithColumn("tripled", Col("price").Mul(Lit(3.0))).
		Filter(Col("doubled").Gt(Lit(20.0))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if eager.NumRows() != streaming.NumRows() {
		t.Fatalf("row count mismatch: eager=%d streaming=%d",
			eager.NumRows(), streaming.NumRows())
	}
	// Verify a shared column matches.
	for _, col := range []string{"id", "price", "doubled", "tripled"} {
		eS, _ := eager.Column(col)
		sS, _ := streaming.Column(col)
		// Concatenate streaming chunks for cross-chunk comparison.
		if eS.Len() != sS.Len() {
			t.Fatalf("col %q length mismatch: eager=%d streaming=%d",
				col, eS.Len(), sS.Len())
		}
	}
}

// TestFusion_FilterMidChainShortCircuits — a filter that drops all
// rows partway through the chain must not run the remaining ops on
// that batch. Verified by checking the output row count matches the
// expected filter selectivity.
func TestFusion_FilterMidChainShortCircuits(t *testing.T) {
	f := lazyFrame(t)
	// Filter to a predicate that matches nothing, then WithColumn.
	// The WithColumn should still work correctly (returning 0 rows).
	out, err := f.Lazy().
		Filter(Col("price").Gt(Lit(9999.0))). // no rows match
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("row count = %d, want 0 (filter dropped all rows)", out.NumRows())
	}
	// Schema should still include the new column.
	if _, err := out.Column("doubled"); err != nil {
		t.Fatalf("doubled column missing from empty output: %v", err)
	}
}

// TestFusion_DoesNotFuseAcrossMaterializeBoundary — SortBy forces
// materialize. Ops before Sort fuse; ops after Sort start a new
// chain. The overall exec tree should have distinct fused blocks
// separated by the materialize boundary.
func TestFusion_DoesNotFuseAcrossMaterializeBoundary(t *testing.T) {
	f := lazyFrame(t)
	lf := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		SortBy(SortKey{Column: "price"}).
		WithColumn("tripled", Col("price").Mul(Lit(3.0)))
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	// Top op should be a fusedStreamExecOp OR a withColumnExecOp
	// depending on whether the post-Sort chain fused with itself.
	// Its input should eventually reach a materializeExecOp (from Sort).
	found := false
	cur := op
	for cur != nil {
		if _, ok := cur.(*materializeExecOp); ok {
			found = true
			break
		}
		cur = frameApplierChild(cur)
		// fusedStreamExecOp isn't a frameApplier but has an `input`.
		if cur == nil {
			// Try via fusedStreamExecOp.
			if fused, ok := op.(*fusedStreamExecOp); ok {
				cur = fused.input
				op = nil // exit outer sentinel
			}
		}
	}
	if !found {
		t.Fatalf("expected materializeExecOp somewhere in the chain (SortBy)")
	}
}

// TestFusion_ExplodeInChain — Explode fits the frameApplier
// contract (per-batch Frame.Explode). A chain like
// `.WithColumn(...).Explode(...)` should fuse.
func TestFusion_ExplodeInChain(t *testing.T) {
	f := listExplodeFrame(t)
	lf := f.Lazy().
		WithColumn("tag_count", Col("tags").ListLen()).
		Explode("tags")
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	fused, ok := op.(*fusedStreamExecOp)
	if !ok {
		t.Fatalf("expected *fusedStreamExecOp, got %T", op)
	}
	if len(fused.ops) != 2 {
		t.Fatalf("fused ops = %d, want 2 [withColumn, explode]", len(fused.ops))
	}
	if _, ok := fused.ops[0].(*withColumnExecOp); !ok {
		t.Errorf("ops[0] = %T, want *withColumnExecOp", fused.ops[0])
	}
	if _, ok := fused.ops[1].(*explodeExecOp); !ok {
		t.Errorf("ops[1] = %T, want *explodeExecOp", fused.ops[1])
	}
	// End-to-end run — verify correctness through the fused path.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	// 4 rows post-explode (3 + 1 + 0-null + 0-empty = 3 non-null + 2 null-fills)
	if out.NumRows() != 6 {
		t.Fatalf("row count = %d, want 6", out.NumRows())
	}
	tagCount, _ := out.Column("tag_count")
	// tag_count was computed BEFORE Explode. After Explode, each parent's
	// count is duplicated across its exploded rows.
	arr := tagCount.col.Data().Chunks()[0].(*array.Int64)
	// Row 0 (from list [10,20,30]): count=3
	// Row 1 (from list [10,20,30]): count=3
	// Row 2 (from list [10,20,30]): count=3
	// Row 3 (from list [40]): count=1
	// Row 4 (from null list): count=null (ListLen on null = null)
	// Row 5 (from empty list): count=0
	if arr.Value(0) != 3 || arr.Value(1) != 3 || arr.Value(2) != 3 {
		t.Fatalf("first 3 rows count = [%d %d %d], want [3 3 3]",
			arr.Value(0), arr.Value(1), arr.Value(2))
	}
	if arr.Value(3) != 1 {
		t.Fatalf("row 3 count = %d, want 1", arr.Value(3))
	}
}

// TestFusion_OverForcesMaterialize — the v0.2.7 exprContainsOver gate
// still routes Over to materialize. That op is NOT a frameApplier, so
// it breaks the fusion chain — verified by asserting the top op is a
// materializeExecOp (from the Over-containing WithColumn), not a
// fusedStreamExecOp.
func TestFusion_OverForcesMaterialize(t *testing.T) {
	// Use lazyFrame (which has price / region etc.) not exprFrame.
	f := lazyFrame(t)
	lf := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		WithColumn("region_max", Col("price").MaxAgg().Over("region"))
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	// The top op should be materializeExecOp (from the Over-WithColumn).
	if _, ok := op.(*materializeExecOp); !ok {
		t.Fatalf("expected top op *materializeExecOp (Over forces it), got %T", op)
	}
}

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

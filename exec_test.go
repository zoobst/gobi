package gobi

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// -- Streaming semantics -------------------------------------------------

func TestExec_ScanFrame_ProducesBatches(t *testing.T) {
	// A 10-row Frame with batchRows=3 should yield 4 batches: 3+3+3+1.
	df := lazyFrame(t) // 5 rows
	op := newScanFrameExec(df, 2)
	defer op.Close()

	sizes := drainBatchSizes(t, op)
	want := []int64{2, 2, 1}
	if len(sizes) != len(want) {
		t.Fatalf("batches = %v, want %v", sizes, want)
	}
	for i, w := range want {
		if sizes[i] != w {
			t.Errorf("batch %d rows = %d, want %d", i, sizes[i], w)
		}
	}
}

func TestExec_ScanFrame_EmptyInputYieldsEOFImmediately(t *testing.T) {
	empty, err := emptyFrame(lazyFrame(t).Schema())
	if err != nil {
		t.Fatal(err)
	}
	op := newScanFrameExec(empty, 100)
	defer op.Close()
	if _, err := op.Next(context.Background()); err != io.EOF {
		t.Fatalf("empty scan Next = %v, want io.EOF", err)
	}
}

// -- filterExec ----------------------------------------------------------

func TestExec_Filter_KeepsMatchingRows(t *testing.T) {
	// active=true rows: id 1, 3, 5. Filter should retain those.
	df := lazyFrame(t)
	scan := newScanFrameExec(df, 2)
	filter := &filterExecOp{input: scan, cond: Col("active")}

	got, err := Execute(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumRows() != 3 {
		t.Fatalf("filter rows = %d, want 3", got.NumRows())
	}
	ids, _ := got.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	want := []int64{1, 3, 5}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

func TestExec_Filter_AllFilteredOutGivesEmptyFrame(t *testing.T) {
	df := lazyFrame(t)
	scan := newScanFrameExec(df, 2)
	filter := &filterExecOp{input: scan, cond: Col("price").Gt(Lit(999.0))}
	got, err := Execute(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumRows() != 0 {
		t.Fatalf("all-filtered-out rows = %d, want 0", got.NumRows())
	}
	// Schema preserved.
	if got.NumCols() != df.NumCols() {
		t.Fatalf("cols = %d, want %d", got.NumCols(), df.NumCols())
	}
}

// -- limitExec: streams enough then short-circuits ---------------------

func TestExec_Limit_StopsPullingAtRemaining(t *testing.T) {
	df := lazyFrame(t) // 5 rows
	scan := newScanFrameExec(df, 2)
	lim := &limitExecOp{input: scan, remaining: 3}
	got, err := Execute(context.Background(), lim)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumRows() != 3 {
		t.Fatalf("limited rows = %d, want 3", got.NumRows())
	}
}

func TestExec_Limit_ZeroYieldsEmptyImmediately(t *testing.T) {
	df := lazyFrame(t)
	scan := newScanFrameExec(df, 2)
	lim := &limitExecOp{input: scan, remaining: 0}
	got, err := Execute(context.Background(), lim)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumRows() != 0 {
		t.Fatalf("Limit(0) rows = %d, want 0", got.NumRows())
	}
}

// -- Compile end-to-end ------------------------------------------------

func TestExec_Compile_FilterProjectLimit(t *testing.T) {
	// Correctness parity between the executor (Collect) and the eager
	// walker (CollectRaw) for a nontrivial chain.
	df := lazyFrame(t)
	lf := df.Lazy().
		Filter(Col("price").Gt(Lit(15.0))).
		Select(Col("id"), Col("price").Mul(Lit(2.0)).Alias("doubled")).
		Limit(2)

	optRes, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	rawRes, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if optRes.NumRows() != rawRes.NumRows() {
		t.Fatalf("streaming rows=%d, eager rows=%d", optRes.NumRows(), rawRes.NumRows())
	}
	if optRes.NumCols() != rawRes.NumCols() {
		t.Fatalf("streaming cols=%d, eager cols=%d", optRes.NumCols(), rawRes.NumCols())
	}
	// Confirm both agree on values in the derived column.
	optCol, _ := optRes.Column("doubled")
	rawCol, _ := rawRes.Column("doubled")
	optArr := optCol.Column().Data().Chunks()[0].(*array.Float64)
	rawArr := rawCol.Column().Data().Chunks()[0].(*array.Float64)
	for i := 0; i < optArr.Len(); i++ {
		if optArr.Value(i) != rawArr.Value(i) {
			t.Errorf("row %d: streaming=%v, eager=%v",
				i, optArr.Value(i), rawArr.Value(i))
		}
	}
}

// -- materializeExec fallback: blocking ops still work through executor

func TestExec_MaterializeFallback_SortAggregate(t *testing.T) {
	// Sort + Aggregate both materialize; verify the executor drives
	// them and result matches the eager walker.
	df := lazyFrame(t)
	lf := df.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "price", Kind: AggSum}).
		SortBy(SortKey{Column: "price_sum", Descending: true})

	opt, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if opt.NumRows() != raw.NumRows() {
		t.Fatalf("streaming rows=%d, eager rows=%d", opt.NumRows(), raw.NumRows())
	}
	// First row's region (post-sort by price_sum DESC) should match.
	optRegions, _ := opt.Column("region")
	rawRegions, _ := raw.Column("region")
	optA := optRegions.Column().Data().Chunks()[0].(*array.String)
	rawA := rawRegions.Column().Data().Chunks()[0].(*array.String)
	if optA.Value(0) != rawA.Value(0) {
		t.Fatalf("top region: streaming=%q, eager=%q",
			optA.Value(0), rawA.Value(0))
	}
}

func TestExec_MaterializeFallback_Join(t *testing.T) {
	// Join is blocking; the executor materializes both sides and
	// delegates to Frame.Join. Verify parity.
	left := lazyFrame(t)
	right := lazyRegions(t)
	lf := left.Lazy().Join(right.Lazy(), "region", "region", JoinInner)

	opt, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if opt.NumRows() != raw.NumRows() || opt.NumCols() != raw.NumCols() {
		t.Fatalf("streaming (%d, %d), eager (%d, %d)",
			opt.NumRows(), opt.NumCols(), raw.NumRows(), raw.NumCols())
	}
}

// -- emptyNode via executor -------------------------------------------

func TestExec_EmptyNode_YieldsEmptyFrame(t *testing.T) {
	df := lazyFrame(t)
	// Filter(false) collapses to emptyNode via the optimizer; the
	// executor sees emptyExecOp and returns io.EOF immediately.
	lf := df.Lazy().Filter(Lit(false))
	got, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got.NumRows() != 0 {
		t.Fatalf("empty result rows = %d, want 0", got.NumRows())
	}
	if got.NumCols() != df.NumCols() {
		t.Fatalf("empty result cols = %d, want %d (schema preserved)",
			got.NumCols(), df.NumCols())
	}
}

// -- Cancellation ------------------------------------------------------

func TestExec_ContextCancellation(t *testing.T) {
	df := lazyFrame(t)
	scan := newScanFrameExec(df, 2)
	filter := &filterExecOp{input: scan, cond: Col("active")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := Execute(ctx, filter)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: got %v, want context.Canceled", err)
	}
}

// -- streamingAggregateExec ----------------------------------------------

func TestExec_StreamingAggregate_UsedForBuiltInAggs(t *testing.T) {
	// Confirm the compiler picks the streaming path for built-in
	// aggregations (no materializeExec at the top of an Aggregate
	// subtree).
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(Aggregation{Column: "price", Kind: AggSum})
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if _, ok := op.(*streamingAggregateExec); !ok {
		t.Fatalf("aggregate should compile to streaming; got %T", op)
	}
}

func TestExec_StreamingAggregate_FallbackForCustomFn(t *testing.T) {
	// A custom Fn aggregator must route through materializeExec
	// because Aggregator.Aggregate(Series, []int) takes all rows.
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "price", Fn: countDistinctAggregator{}},
	)
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if _, ok := op.(*materializeExecOp); !ok {
		t.Fatalf("custom Fn should route to materializeExec; got %T", op)
	}
}

func TestExec_StreamingAggregate_ParityWithEager(t *testing.T) {
	// Correctness parity: streaming (Collect) and eager (CollectRaw)
	// must agree on all built-in aggregations.
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "price", Kind: AggSum},
		Aggregation{Column: "price", Kind: AggMean},
		Aggregation{Column: "price", Kind: AggMin},
		Aggregation{Column: "price", Kind: AggMax},
		Aggregation{Column: "price", Kind: AggCount, Alias: "n"},
	)
	streamed, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	eager, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if streamed.NumRows() != eager.NumRows() {
		t.Fatalf("streamed rows=%d, eager rows=%d",
			streamed.NumRows(), eager.NumRows())
	}
	// Compare cell-by-cell for every column.
	for _, col := range []string{"region", "price_sum", "price_mean", "price_min", "price_max", "n"} {
		s1, err := streamed.Column(col)
		if err != nil {
			t.Fatalf("streamed missing col %q", col)
		}
		s2, err := eager.Column(col)
		if err != nil {
			t.Fatalf("eager missing col %q", col)
		}
		compareSeriesValues(t, col, s1, s2)
	}
}

func TestExec_StreamingAggregate_MultipleBatches(t *testing.T) {
	// Force multi-batch input to exercise the incremental update
	// path — scanFrameExec with a batchRows smaller than the frame.
	df := lazyFrame(t) // 5 rows
	// Compile a plan by hand so we can force a tiny batch size on
	// the scan operator: LazyFrame's Compile uses defaultBatchRows.
	scan := newScanFrameExec(df, 2) // → 3 batches of 2/2/1
	agg := &streamingAggregateExec{
		input:     scan,
		keys:      []string{"region"},
		aggs:      []Aggregation{{Column: "price", Kind: AggSum}},
		outSchema: newAggregateNode(&scanFrameNode{frame: df}, []string{"region"},
			[]Aggregation{{Column: "price", Kind: AggSum}}).outSchema,
	}
	got, err := Execute(context.Background(), agg)
	if err != nil {
		t.Fatal(err)
	}
	// EU: 20+40=60; US: 10+30+50=90.
	if got.NumRows() != 2 {
		t.Fatalf("groups = %d, want 2", got.NumRows())
	}
	sums, _ := got.Column("price_sum")
	arr := sums.Column().Data().Chunks()[0].(*array.Float64)
	// Order: EU first, then US.
	if arr.Value(0) != 60 || arr.Value(1) != 90 {
		t.Fatalf("sums = %v, %v; want 60, 90", arr.Value(0), arr.Value(1))
	}
}

func TestExec_StreamingAggregate_MultiKey(t *testing.T) {
	// Two key columns: region + active. Each unique (region, active)
	// combination is its own group.
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region", "active").Agg(
		Aggregation{Column: "price", Kind: AggSum},
	)
	streamed, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	eager, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if streamed.NumRows() != eager.NumRows() {
		t.Fatalf("multi-key rows: streamed=%d, eager=%d",
			streamed.NumRows(), eager.NumRows())
	}
}

// -- streamingJoinExec ----------------------------------------------------

func TestExec_StreamingJoin_UsedForLeftDrivenKinds(t *testing.T) {
	// Compiler must select streamingJoinExec for Inner/Left/Semi/Anti.
	kinds := []JoinType{JoinInner, JoinLeft, JoinSemi, JoinAnti}
	for _, k := range kinds {
		left := lazyFrame(t).Lazy()
		right := lazyRegions(t).Lazy()
		lf := left.Join(right, "region", "region", k)
		op, err := Compile(Optimize(lf.Plan()))
		if err != nil {
			t.Fatalf("kind=%v: compile: %v", k, err)
		}
		if _, ok := op.(*streamingJoinExec); !ok {
			t.Errorf("kind=%v: compiled to %T, want *streamingJoinExec", k, op)
		}
		_ = op.Close()
	}
}

func TestExec_StreamingJoin_FallbackForRightAndFull(t *testing.T) {
	// Right and Full must route to the materializing fallback.
	kinds := []JoinType{JoinRight, JoinFull}
	for _, k := range kinds {
		left := lazyFrame(t).Lazy()
		right := lazyRegions(t).Lazy()
		lf := left.Join(right, "region", "region", k)
		op, err := Compile(Optimize(lf.Plan()))
		if err != nil {
			t.Fatalf("kind=%v: compile: %v", k, err)
		}
		if _, ok := op.(*materializeExecOp); !ok {
			t.Errorf("kind=%v: compiled to %T, want *materializeExecOp", k, op)
		}
		_ = op.Close()
	}
}

func TestExec_StreamingJoin_ParityWithEager(t *testing.T) {
	// Result parity: streaming (Collect) and eager (CollectRaw)
	// must produce identical joined output for every left-driven
	// kind.
	kinds := []JoinType{JoinInner, JoinLeft, JoinSemi, JoinAnti}
	for _, k := range kinds {
		left := lazyFrame(t)
		right := lazyRegions(t)
		lf := left.Lazy().Join(right.Lazy(), "region", "region", k)

		streamed, err := lf.Collect()
		if err != nil {
			t.Fatalf("kind=%v streaming: %v", k, err)
		}
		eager, err := lf.CollectRaw()
		if err != nil {
			t.Fatalf("kind=%v eager: %v", k, err)
		}
		if streamed.NumRows() != eager.NumRows() {
			t.Errorf("kind=%v: streamed=%d rows, eager=%d rows",
				k, streamed.NumRows(), eager.NumRows())
		}
		if streamed.NumCols() != eager.NumCols() {
			t.Errorf("kind=%v: streamed=%d cols, eager=%d cols",
				k, streamed.NumCols(), eager.NumCols())
		}
	}
}

func TestExec_StreamingJoin_ProbeSideStreams(t *testing.T) {
	// Force a multi-batch probe by driving the operator directly
	// with a small batchRows on the left scan. The build side goes
	// through Execute (single-batch pass) internally.
	left := lazyFrame(t)   // 5 rows
	right := lazyRegions(t) // 2 rows
	leftScan := newScanFrameExec(left, 2)  // 3 probe batches
	rightScan := newScanFrameExec(right, 100)

	join := &streamingJoinExec{
		left:      leftScan,
		right:     rightScan,
		leftKey:   "region",
		rightKey:  "region",
		kind:      JoinInner,
		outSchema: newJoinNode(&scanFrameNode{frame: left}, &scanFrameNode{frame: right}, "region", "region", JoinInner).outSchema,
	}

	got, err := Execute(context.Background(), join)
	if err != nil {
		t.Fatal(err)
	}
	// All 5 left rows match a region row → 5 output rows.
	if got.NumRows() != 5 {
		t.Fatalf("inner-join rows = %d, want 5", got.NumRows())
	}
	// Cross-check against the eager Join.
	want, err := left.Join(right, "region", "region", JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumRows() != want.NumRows() {
		t.Fatalf("streamed=%d, eager=%d", got.NumRows(), want.NumRows())
	}
}

func TestExec_StreamingJoin_EmptyProbeYieldsEmpty(t *testing.T) {
	// Probe side with zero rows → no output rows regardless of
	// build-side size. Uses Filter(false) to force an empty probe.
	left := lazyFrame(t)
	right := lazyRegions(t)
	lf := left.Lazy().
		Filter(Lit(false)).
		Join(right.Lazy(), "region", "region", JoinInner)

	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("empty-probe rows = %d, want 0", out.NumRows())
	}
	// Schema still comes from the join, so col count matches inner.
	if out.NumCols() == 0 {
		t.Fatalf("schema lost on empty-probe join")
	}
}

// (countDistinctAggregator lives in groupby_custom_test.go — reused here.)

// compareSeriesValues checks two Series' first-chunk arrays match
// value-by-value + null-flag-by-null-flag. Panics if the underlying
// types don't match — tests should catch this early.
func compareSeriesValues(t *testing.T, label string, a, b Series) {
	t.Helper()
	if a.Len() != b.Len() {
		t.Fatalf("%s: len mismatch %d vs %d", label, a.Len(), b.Len())
	}
	aChunk := a.col.Data().Chunks()[0]
	bChunk := b.col.Data().Chunks()[0]
	for i := 0; i < a.Len(); i++ {
		if aChunk.IsNull(i) != bChunk.IsNull(i) {
			t.Errorf("%s row %d: null flag mismatch", label, i)
			continue
		}
		if aChunk.IsNull(i) {
			continue
		}
		switch av := aChunk.(type) {
		case *array.Int64:
			bv := bChunk.(*array.Int64)
			if av.Value(i) != bv.Value(i) {
				t.Errorf("%s row %d: %v vs %v", label, i, av.Value(i), bv.Value(i))
			}
		case *array.Float64:
			bv := bChunk.(*array.Float64)
			if av.Value(i) != bv.Value(i) {
				t.Errorf("%s row %d: %v vs %v", label, i, av.Value(i), bv.Value(i))
			}
		case *array.String:
			bv := bChunk.(*array.String)
			if av.Value(i) != bv.Value(i) {
				t.Errorf("%s row %d: %q vs %q", label, i, av.Value(i), bv.Value(i))
			}
		case *array.Boolean:
			bv := bChunk.(*array.Boolean)
			if av.Value(i) != bv.Value(i) {
				t.Errorf("%s row %d: %v vs %v", label, i, av.Value(i), bv.Value(i))
			}
		}
	}
}

// -- Helper -------------------------------------------------------------

// drainBatchSizes drains op and returns the row count of each batch
// it produced. Handles ownership by releasing every batch.
func drainBatchSizes(t *testing.T, op ExecOperator) []int64 {
	t.Helper()
	var sizes []int64
	ctx := context.Background()
	for {
		batch, err := op.Next(ctx)
		if err == io.EOF {
			return sizes
		}
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, batch.NumRows())
		batch.Release()
	}
}

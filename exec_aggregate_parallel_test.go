package gobi

import (
	"context"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestParallelAggregate_ParityAcrossWorkerCounts verifies that the
// partitioned build produces identical results to the serial build
// for a range of worker counts. Same input, same aggregations, must
// produce the same key/value output in the same order (streaming
// output is sorted by key bytes).
func TestParallelAggregate_ParityAcrossWorkerCounts(t *testing.T) {
	df := lazyFrame(t) // small in-memory frame from exec_test.go helpers
	buildAgg := func() *aggregateNode {
		lf := df.Lazy().GroupBy("region", "active").Agg(
			Aggregation{Column: "price", Kind: AggSum},
			Aggregation{Column: "price", Kind: AggMean, Alias: "avg"},
			Aggregation{Column: "price", Kind: AggMin, Alias: "lo"},
			Aggregation{Column: "price", Kind: AggMax, Alias: "hi"},
			Aggregation{Column: "price", Kind: AggCount, Alias: "n"},
		)
		return lf.Plan().(*aggregateNode)
	}

	// Baseline: serial build (workers=1).
	baseline := runAggWithWorkers(t, buildAgg(), 1)

	for _, w := range []int{2, 4, 8, 16} {
		w := w
		t.Run("workers="+itoa(w), func(t *testing.T) {
			got := runAggWithWorkers(t, buildAgg(), w)
			if got.NumRows() != baseline.NumRows() {
				t.Fatalf("workers=%d rows=%d, want %d", w, got.NumRows(), baseline.NumRows())
			}
			for _, col := range []string{"region", "active", "price_sum", "avg", "lo", "hi", "n"} {
				sg, err := got.Column(col)
				if err != nil {
					t.Fatalf("workers=%d missing col %q", w, col)
				}
				sb, err := baseline.Column(col)
				if err != nil {
					t.Fatalf("baseline missing col %q", col)
				}
				compareSeriesValues(t, col, sg, sb)
			}
		})
	}
}

// TestParallelAggregate_EmptyInput exercises the parallel path against
// zero input rows. Should produce a zero-row result and Close cleanly.
func TestParallelAggregate_EmptyInput(t *testing.T) {
	// NOTE: Frame.Head(0) is a documented gotcha (treats 0 as "default
	// 5"). Route through Limit(0) on the lazy path which handles zero
	// correctly.
	df := lazyFrame(t)
	agg := df.Lazy().
		Limit(0).
		GroupBy("region").
		Agg(Aggregation{Column: "price", Kind: AggSum}).
		Plan().(*aggregateNode)

	got := runAggWithWorkers(t, agg, 8)
	if got.NumRows() != 0 {
		t.Fatalf("empty input should yield 0 groups; got %d", got.NumRows())
	}
}

// TestParallelAggregate_FewerKeysThanWorkers stress-tests the case
// where some worker inboxes receive zero messages (e.g. 2 distinct
// keys, 16 workers). Idle workers must Close cleanly and the merge
// step must skip empty partitions.
func TestParallelAggregate_FewerKeysThanWorkers(t *testing.T) {
	df := lazyFrame(t) // 5 rows, 2 distinct regions
	agg := df.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "price", Kind: AggSum}).
		Plan().(*aggregateNode)

	got := runAggWithWorkers(t, agg, 16)
	if got.NumRows() != 2 {
		t.Fatalf("expected 2 groups (regions), got %d", got.NumRows())
	}
}

// TestParallelAggregate_CancellationStopsWorkers verifies that a
// cancelled parent context propagates through the parallel build and
// returns an error without deadlocking. -race enforces the goroutine
// safety of the drain-on-cancel logic in workerConsume.
func TestParallelAggregate_CancellationStopsWorkers(t *testing.T) {
	df := lazyFrame(t)
	agg := df.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "price", Kind: AggSum}).
		Plan().(*aggregateNode)

	op, err := Compile(agg)
	if err != nil {
		t.Fatal(err)
	}
	// Force multi-worker even if the machine's GOMAXPROCS is small.
	if e, ok := op.(*streamingAggregateExec); ok {
		e.workers = 8
	}
	defer op.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: build should return ctx.Err() promptly

	if _, err := Execute(ctx, op); err == nil {
		t.Fatal("expected context.Canceled from Execute after pre-cancel")
	}
}

// TestParallelAggregate_ExplainShowsWorkerCount confirms the physical
// label surfaces the resolved worker count for the streaming
// aggregate — the mirror of TestParallelScan_ExplainShowsWorkerCount.
func TestParallelAggregate_ExplainShowsWorkerCount(t *testing.T) {
	prevMax := MaxParallelism()
	SetMaxParallelism(4)
	t.Cleanup(func() { SetMaxParallelism(prevMax) })

	df := lazyFrame(t)
	lf := df.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "price", Kind: AggSum})

	explain := lf.ExplainPhysical()
	if !strings.Contains(explain, "[workers=4]") {
		t.Fatalf("expected StreamingAggregate label to include [workers=4]:\n%s", explain)
	}
}

// -- helpers -----------------------------------------------------------

// runAggWithWorkers Compiles an aggregate plan, pins the worker count,
// executes, and returns the collected Frame. Fatals on any error.
func runAggWithWorkers(t *testing.T, plan *aggregateNode, workers int) *Frame {
	t.Helper()
	op, err := Compile(plan)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	agg, ok := op.(*streamingAggregateExec)
	if !ok {
		op.Close()
		t.Fatalf("expected streamingAggregateExec, got %T", op)
	}
	agg.workers = workers
	defer op.Close()
	got, err := Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("execute (workers=%d): %v", workers, err)
	}
	return got
}

// itoa: local strconv-free stringifier for test-name suffixes so we
// don't drag in strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Silence unused-import warning when array only used in future tests.
var _ = array.NewInt64Builder

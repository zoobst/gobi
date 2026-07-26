package gobi

import (
	"context"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Verify: NewStringSetAggregator now implements IncrementalAggregator
// — a compile-time assertion via interface conversion.
func TestIncrementalAggregator_SetAggregatorImplements(t *testing.T) {
	agg := NewStringSetAggregator()
	if _, ok := agg.(IncrementalAggregator); !ok {
		t.Fatal("NewStringSetAggregator() should implement IncrementalAggregator")
	}
	// Int64 / Int32 / Uint64 / Uint32 variants too.
	if _, ok := NewInt64SetAggregator().(IncrementalAggregator); !ok {
		t.Fatal("NewInt64SetAggregator() should implement IncrementalAggregator")
	}
	if _, ok := NewUint64SetAggregator().(IncrementalAggregator); !ok {
		t.Fatal("NewUint64SetAggregator() should implement IncrementalAggregator")
	}
}

// Verify: an IncrementalAggregator's Clone yields a fresh instance
// with empty state (not a shared reference to the same seen map).
func TestIncrementalAggregator_CloneIsFreshState(t *testing.T) {
	orig := NewStringSetAggregator().(IncrementalAggregator)
	// Prime original with some state.
	pool := memory.DefaultAllocator
	b := array.NewStringBuilder(pool)
	defer b.Release()
	b.Append("primed")
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "x", Type: arrow.BinaryTypes.String, Nullable: true}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	s := NewSeries(arrow.NewColumn(field, chunked))
	if err := orig.Update(s, []int{0}); err != nil {
		t.Fatal(err)
	}

	// Clone — must not carry "primed" into the clone's state.
	clone := orig.Clone()
	res := clone.Finalize().([]string)
	if len(res) != 0 {
		t.Fatalf("clone Finalize returned %v; expected empty ([] — clone must start fresh)", res)
	}
	// Original still has its state.
	origRes := orig.Finalize().([]string)
	if len(origRes) != 1 || origRes[0] != "primed" {
		t.Fatalf("original changed after Clone: %v", origRes)
	}
}

// The key user-visible win: an IncrementalAggregator custom Fn now
// routes through streamingAggregateExec — verified by observing
// that a pipeline with just NewStringSetAggregator no longer requires
// the materializing fallback.
//
// We check by compiling the plan and asserting the operator tree
// contains a streamingAggregateExec (not a materializeExecOp) at the
// aggregate level.
func TestIncrementalAggregator_CompilesToStreamingExec(t *testing.T) {
	f := setAggFrame(t)
	lf := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"})

	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	// Streaming exec — direct type assertion.
	if _, ok := op.(*streamingAggregateExec); !ok {
		t.Fatalf("expected *streamingAggregateExec, got %T (custom IncrementalAggregator should stream, not materialize)", op)
	}
}

// Regression: a legacy Aggregator-only custom Fn (no IncrementalAggregator
// methods) still routes through the materializing fallback — the interface
// unification is additive, not breaking.
func TestIncrementalAggregator_LegacyAggregatorStillMaterializes(t *testing.T) {
	f := salesFrame(t)
	lf := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{
			Column: "units",
			// modeAggregator is defined in groupby_custom_test.go — only
			// implements Aggregator, not IncrementalAggregator.
			Fn:    &modeAggregator{},
			Alias: "mode_units",
		})
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := op.(*materializeExecOp); !ok {
		t.Fatalf("expected *materializeExecOp for Aggregator-only Fn, got %T", op)
	}
	// Verify it still runs correctly.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := out.Shape(); r != 3 {
		t.Fatalf("row count = %d, want 3", r)
	}
}

// Correctness parity: same result whether routed through streaming
// exec (IncrementalAggregator custom Fn) or the eager path
// (Frame.GroupBy.Agg). Both should produce identical output.
func TestIncrementalAggregator_StreamingMatchesEager(t *testing.T) {
	f := setAggFrame(t)

	// Eager path — Frame.GroupBy.Agg directly.
	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	eager, err := gb.Agg(Aggregation{
		Column: "provider", Fn: NewStringSetAggregator(), Alias: "s",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Streaming path — LazyFrame.Collect via streamingAggregateExec.
	streaming, err := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}

	if eager.NumRows() != streaming.NumRows() {
		t.Fatalf("row count mismatch: eager=%d streaming=%d",
			eager.NumRows(), streaming.NumRows())
	}
	// Extract per-region provider sets from both.
	extract := func(f *Frame) map[string][]string {
		out := make(map[string][]string)
		region, _ := f.Column("region")
		rArr := region.col.Data().Chunks()[0].(*array.String)
		set, _ := f.Column("s")
		la := set.col.Data().Chunks()[0].(*array.List)
		values := la.ListValues().(*array.String)
		for i := range f.NumRows() {
			start, end := la.ValueOffsets(i)
			vals := make([]string, end-start)
			for j := start; j < end; j++ {
				vals[j-start] = values.Value(int(j))
			}
			out[rArr.Value(i)] = vals
		}
		return out
	}
	eagerSet := extract(eager)
	streamingSet := extract(streaming)
	if len(eagerSet) != len(streamingSet) {
		t.Fatalf("group count mismatch: eager=%d streaming=%d", len(eagerSet), len(streamingSet))
	}
	for region, want := range eagerSet {
		got, ok := streamingSet[region]
		if !ok {
			t.Fatalf("region %q missing from streaming output", region)
		}
		if len(got) != len(want) {
			t.Fatalf("region %q length mismatch: eager=%v streaming=%v", region, want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("region %q element %d mismatch: eager=%q streaming=%q",
					region, i, want[i], got[i])
			}
		}
	}
}

// Multi-batch input: verify that Update is called incrementally
// across multiple batches and the group's accumulated set correctly
// spans all batches. Achieved by feeding a source with a small
// defaultBatchRows so the input gets split.
func TestIncrementalAggregator_MultiBatchAccumulation(t *testing.T) {
	// Build a frame large enough to span multiple default-sized batches
	// (defaultBatchRows is 1024 today). One key group with 3000 rows
	// covering 3 distinct provider strings.
	pool := memory.DefaultAllocator
	const nRows = 3000
	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	providerB := array.NewStringBuilder(pool)
	defer providerB.Release()
	for i := range nRows {
		regionB.Append("only")
		switch i % 3 {
		case 0:
			providerB.Append("att")
		case 1:
			providerB.Append("verizon")
		case 2:
			providerB.Append("tmobile")
		}
	}
	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "provider", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), providerB.NewArray()}
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
		t.Fatal(err)
	}

	// Collect via the streaming path.
	out, err := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 1 {
		t.Fatalf("row count = %d, want 1", out.NumRows())
	}
	set, _ := out.Column("s")
	la := set.col.Data().Chunks()[0].(*array.List)
	values := la.ListValues().(*array.String)
	start, end := la.ValueOffsets(0)
	if end-start != 3 {
		t.Fatalf("expected 3 distinct providers, got %d", end-start)
	}
	// Sorted output: att, tmobile, verizon.
	want := []string{"att", "tmobile", "verizon"}
	for i, w := range want {
		if got := values.Value(int(start) + i); got != w {
			t.Fatalf("elem %d = %q, want %q", i, got, w)
		}
	}
}

// buildIfNeeded uses context; guard against a nil-derived crash by
// exercising the streaming exec through a normal Collect.
func TestIncrementalAggregator_ContextDeadline(t *testing.T) {
	// This test doesn't set a deadline — it just ensures the streaming
	// path doesn't panic when the context flows through the executor.
	f := setAggFrame(t)
	ctx := context.Background()
	op, err := Compile(Optimize(f.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"}).
		Plan()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(ctx, op); err != nil {
		t.Fatal(err)
	}
}

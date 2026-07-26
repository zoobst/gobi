package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestOver_StreamingCrossBatchCorrectness — REGRESSION: a streaming
// pipeline that includes `.Over(K)` must not see partitions split
// across batch boundaries.
//
// `withColumnExecOp` evaluates the expression against each batch's
// Frame independently. When the expression is
// `Col("v").Shift(1).Over("id")` and partition id=X has rows in
// batches 1 and 2, the two batches are treated as two disjoint
// partitions — Shift's row 0 in batch 2 wrongly emits null instead
// of the last row of batch 1.
//
// Fix: at Compile, if a WithColumn / Filter / Select expression
// contains an overNode, wrap the operator in materializeExecOp so
// it sees the whole input Frame in one shot.
func TestOver_StreamingCrossBatchCorrectness(t *testing.T) {
	pool := memory.DefaultAllocator
	// 200_000 rows, all id=1 → one big partition. Rows v = 0..N-1.
	// defaultBatchRows is 64K, so this produces ~3 batches. If the
	// streaming exec treats each batch's partition independently, rows
	// 65536 and 131072 (batch boundaries) come out null when they
	// should hold the previous row's v.
	const nRows = 200_000
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	vB := array.NewInt64Builder(pool)
	defer vB.Release()
	for i := range nRows {
		idB.Append(1)
		vB.Append(int64(i))
	}
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), vB.NewArray()}
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

	// Streaming path — LazyFrame.Collect. Was broken before the fix.
	out, err := f.Lazy().
		WithColumn("prev_v", Col("v").Shift(1).Over("id")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := out.Column("prev_v")

	// Expected: row 0 = null, row i (i > 0) = i-1. All same partition.
	// Walk all chunks because materialize slices back into batches.
	values := make([]int64, 0, nRows)
	nulls := make([]bool, 0, nRows)
	for _, chunk := range prev.col.Data().Chunks() {
		arr := chunk.(*array.Int64)
		for i := range arr.Len() {
			if arr.IsNull(i) {
				nulls = append(nulls, true)
				values = append(values, 0)
			} else {
				nulls = append(nulls, false)
				values = append(values, arr.Value(i))
			}
		}
	}

	if len(values) != nRows {
		t.Fatalf("output row count = %d, want %d", len(values), nRows)
	}

	// Row 0: null (first row of the partition).
	if !nulls[0] {
		t.Fatalf("row 0 should be null, got %d", values[0])
	}
	// Rows 1..N-1: prev row's v.
	// Critical: row 1024 (2nd batch boundary) and row 2048 (3rd)
	// should NOT be null. Pre-fix, they'd be null because the streaming
	// exec treated each batch as its own partition.
	for i := 1; i < nRows; i++ {
		if nulls[i] {
			t.Fatalf("row %d unexpectedly null (partition crossing bug: pre-fix, rows at batch boundaries were null)", i)
		}
		if values[i] != int64(i-1) {
			t.Fatalf("row %d = %d, want %d", i, values[i], i-1)
		}
	}
}

// TestOver_StreamingScalarAggCrossBatch — same fix must cover the
// scalar-aggregate Over shape too. Sum().Over("id") must sum the
// whole partition across batches, not just the rows in each batch.
func TestOver_StreamingScalarAggCrossBatch(t *testing.T) {
	pool := memory.DefaultAllocator
	const nRows = 200_000
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	vB := array.NewInt64Builder(pool)
	defer vB.Release()
	for range nRows {
		idB.Append(1)
		vB.Append(int64(1)) // sum = nRows
	}
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), vB.NewArray()}
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
	out, err := f.Lazy().
		WithColumn("group_sum", Col("v").Sum().Over("id")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	sums, _ := out.Column("group_sum")
	// AggSum output type is Float64. Every row should hold nRows.
	values := make([]float64, 0, nRows)
	for _, chunk := range sums.col.Data().Chunks() {
		arr := chunk.(*array.Float64)
		for i := range arr.Len() {
			values = append(values, arr.Value(i))
		}
	}
	if len(values) != nRows {
		t.Fatalf("row count = %d, want %d", len(values), nRows)
	}
	want := float64(nRows)
	// Spot-check row 0, batch boundaries (65536, 131072), and last row.
	for _, i := range []int{0, 65536, 131072, nRows - 1} {
		if values[i] != want {
			t.Fatalf("row %d group_sum = %v, want %v (pre-fix: per-batch sum, not partition-wide)",
				i, values[i], want)
		}
	}
}

// TestOver_StreamingInFilterPredicate — Over inside a Filter must
// also force materialize. Filter with a batch-local Over evaluates
// its predicate per batch, so partition-based filters produce wrong
// results at batch boundaries.
func TestOver_StreamingInFilterPredicate(t *testing.T) {
	pool := memory.DefaultAllocator
	const nRows = 200_000
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	vB := array.NewInt64Builder(pool)
	defer vB.Release()
	for i := range nRows {
		idB.Append(1)
		vB.Append(int64(i))
	}
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), vB.NewArray()}
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
	// Filter to rows where v equals the partition-wide mean. Since v
	// = 0..N-1, mean = (N-1)/2. Exactly one row satisfies. Pre-fix,
	// each batch's local mean would let ~1 row per batch through,
	// producing 3 rows instead of 1.
	out, err := f.Lazy().
		Filter(Col("v").Eq(Col("v").MaxAgg().Over("id"))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Only the last row (v = N-1, which is the partition max) survives.
	if out.NumRows() != 1 {
		t.Fatalf("filtered row count = %d, want 1 (pre-fix: per-batch max let ~1 row per batch through)",
			out.NumRows())
	}
}

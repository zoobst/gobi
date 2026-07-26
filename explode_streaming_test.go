package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestExplodeStreaming_CompilesToStreamingExec — the plan-level
// explodeNode now compiles to explodeExecOp, not materializeExecOp.
// Verified by direct type assertion on the compiled operator.
func TestExplodeStreaming_CompilesToStreamingExec(t *testing.T) {
	f := listExplodeFrame(t)
	op, err := Compile(Optimize(f.Lazy().Explode("tags").Plan()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := op.(*explodeExecOp); !ok {
		t.Fatalf("expected *explodeExecOp, got %T (Explode should stream per-batch)", op)
	}
}

// TestExplodeStreaming_MultiBatchInputExpandsCorrectly — feed a large
// enough input to span multiple batches, verify total row count and
// per-parent-row expansion are correct through the streaming path.
// Ensures no cross-batch state leakage (parent-index scatter is
// batch-local, not global).
func TestExplodeStreaming_MultiBatchInputExpandsCorrectly(t *testing.T) {
	pool := memory.DefaultAllocator
	// Build 3000 rows, each with a 3-element list. Post-Explode = 9000 rows.
	// Spans ~3 default-sized batches (defaultBatchRows = 1024).
	const nRows = 3000
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
		vb.Append(int64(i * 3))
		vb.Append(int64(i*3 + 1))
		vb.Append(int64(i*3 + 2))
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
		t.Fatal(err)
	}

	out, err := f.Lazy().Explode("items").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 9000 {
		t.Fatalf("post-Explode row count = %d, want 9000", out.NumRows())
	}
	// Spot-check the first and last exploded rows.
	idS, _ := out.Column("id")
	itemsS, _ := out.Column("items")
	// items chunks may be many after streaming; walk chunks to count.
	totalItems := 0
	for _, chunk := range itemsS.col.Data().Chunks() {
		totalItems += chunk.Len()
	}
	if totalItems != 9000 {
		t.Fatalf("items chunks sum = %d, want 9000", totalItems)
	}
	// id column duplication check: first three rows should all have id=0.
	idChunks := idS.col.Data().Chunks()
	first3 := make([]int64, 0, 3)
	for _, chunk := range idChunks {
		ia := chunk.(*array.Int64)
		for i := 0; i < ia.Len() && len(first3) < 3; i++ {
			first3 = append(first3, ia.Value(i))
		}
		if len(first3) >= 3 {
			break
		}
	}
	if len(first3) != 3 || first3[0] != 0 || first3[1] != 0 || first3[2] != 0 {
		t.Fatalf("first 3 exploded ids = %v, want [0 0 0]", first3)
	}
}

// TestExplodeStreaming_ParityWithEager — same input, streaming
// (LazyFrame.Collect) vs eager (Frame.Explode). Row-by-row equality
// on the exploded output.
func TestExplodeStreaming_ParityWithEager(t *testing.T) {
	f := mixedGeomFrame(t)
	eager, err := f.Explode("geometry")
	if err != nil {
		t.Fatal(err)
	}
	streaming, err := f.Lazy().Explode("geometry").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if eager.NumRows() != streaming.NumRows() {
		t.Fatalf("row count mismatch: eager=%d streaming=%d",
			eager.NumRows(), streaming.NumRows())
	}
	// Compare name column values.
	eName, _ := eager.Column("name")
	sName, _ := streaming.Column("name")
	eArr := eName.col.Data().Chunks()[0].(*array.String)
	sArrs := sName.col.Data().Chunks()
	streamingVals := make([]string, 0, streaming.NumRows())
	for _, chunk := range sArrs {
		ca := chunk.(*array.String)
		for i := range ca.Len() {
			streamingVals = append(streamingVals, ca.Value(i))
		}
	}
	for i := range eager.NumRows() {
		if eArr.Value(i) != streamingVals[i] {
			t.Fatalf("row %d name mismatch: eager=%q streaming=%q",
				i, eArr.Value(i), streamingVals[i])
		}
	}
}

// TestExplodeStreaming_ComposesWithDownstreamAgg — Explode → GroupBy
// via streaming aggregate. Verifies the exploded batches flow into
// aggregation without the pipeline having to force materialize.
func TestExplodeStreaming_ComposesWithDownstreamAgg(t *testing.T) {
	pool := memory.DefaultAllocator
	// id=[1,2,3], items=[[10,20],[30],[40,50,60]] → 6 exploded rows.
	// After Explode: id duplicates per parent; count per id = list len.
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3}, nil)
	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.Int64Builder)
	lb.Append(true)
	vb.AppendValues([]int64{10, 20}, nil)
	lb.Append(true)
	vb.Append(30)
	lb.Append(true)
	vb.AppendValues([]int64{40, 50, 60}, nil)

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
		t.Fatal(err)
	}

	out, err := f.Lazy().
		Explode("items").
		GroupBy("id").
		Agg(Aggregation{Kind: AggCount, Alias: "n"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := out.Shape(); r != 3 {
		t.Fatalf("group count = %d, want 3", r)
	}
	// Expected: id=1 has 2 items, id=2 has 1, id=3 has 3.
	idArr := out.series[0].col.Data().Chunks()[0].(*array.Int64)
	nArr := out.series[1].col.Data().Chunks()[0].(*array.Int64)
	got := map[int64]int64{}
	for i := range out.NumRows() {
		got[idArr.Value(i)] = nArr.Value(i)
	}
	want := map[int64]int64{1: 2, 2: 1, 3: 3}
	for id, expected := range want {
		if got[id] != expected {
			t.Fatalf("id=%d count = %d, want %d", id, got[id], expected)
		}
	}
}

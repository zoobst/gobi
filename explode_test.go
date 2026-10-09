package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi/geometry"
)

// mixedGeomFrame builds a small frame with a name column and a geometry
// column mixing single- and multi-part geometries plus a null row.
func mixedGeomFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	// Include a null in the geometry column at row index 2.
	nameB.AppendValues([]string{"solo", "multi", "null", "collection"}, nil)

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	geomB.Append(geometry.WKB(geometry.Point{X: 1, Y: 1}))
	geomB.Append(geometry.WKB(geometry.MultiPoint{
		Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 2, Y: 0}},
	}))
	geomB.AppendNull()
	geomB.Append(geometry.WKB(geometry.GeometryCollection{
		Geometries: []geometry.Geometry{
			geometry.Point{X: 5, Y: 5},
			geometry.LineString{Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		},
	}))

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExplode_ExpandsMultisAndDuplicatesAttrs(t *testing.T) {
	f := mixedGeomFrame(t)
	out, err := f.Explode("geometry")
	if err != nil {
		t.Fatal(err)
	}
	// 1 (solo) + 3 (multi-point components) + 1 (null passthrough) + 2
	// (collection components) = 7 rows.
	if got := out.NumRows(); got != 7 {
		t.Fatalf("exploded row count = %d, want 7", got)
	}

	nameCol, _ := out.Column("name")
	nameArr := nameCol.col.Data().Chunks()[0].(*array.String)
	want := []string{"solo", "multi", "multi", "multi", "null", "collection", "collection"}
	for i, w := range want {
		if got := nameArr.Value(i); got != w {
			t.Fatalf("row %d name = %q, want %q", i, got, w)
		}
	}
}

func TestExplode_NullRowRetained(t *testing.T) {
	f := mixedGeomFrame(t)
	out, _ := f.Explode("geometry")
	geomCol, _ := out.Column("geometry")
	bin := geomCol.col.Data().Chunks()[0].(*array.Binary)
	if !bin.IsNull(4) {
		t.Fatalf("null geometry row should have been kept as null; got %v", bin.Value(4))
	}
}

func TestExplode_ComponentTypesCorrect(t *testing.T) {
	f := mixedGeomFrame(t)
	out, _ := f.Explode("geometry")
	geomCol, _ := out.Column("geometry")

	// Row 0: original Point.
	g, err := geomCol.Geometry(0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Type() != geometry.TypePoint {
		t.Fatalf("row 0 type = %s, want Point", g.Type())
	}
	// Rows 1-3: exploded multipoint components (individual Points).
	for i := 1; i <= 3; i++ {
		g, _ := geomCol.Geometry(i)
		if g.Type() != geometry.TypePoint {
			t.Fatalf("row %d type = %s, want Point", i, g.Type())
		}
	}
	// Row 5: first collection component (Point).
	g, _ = geomCol.Geometry(5)
	if g.Type() != geometry.TypePoint {
		t.Fatalf("row 5 type = %s, want Point", g.Type())
	}
	// Row 6: second collection component (LineString).
	g, _ = geomCol.Geometry(6)
	if g.Type() != geometry.TypeLineString {
		t.Fatalf("row 6 type = %s, want LineString", g.Type())
	}
}

func TestExplode_NonGeometryColumnErrors(t *testing.T) {
	f := mixedGeomFrame(t)
	_, err := f.Explode("name")
	if !errors.Is(err, ErrNotGeometry) {
		t.Fatalf("expected ErrNotGeometry, got %v", err)
	}
}

// listExplodeFrame builds a small frame with an id column and a
// List<Int64> tags column mixing non-empty, empty, and null lists.
func listExplodeFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3, 4}, nil)

	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.Int64Builder)
	// Row 0: [10, 20, 30]
	lb.Append(true)
	vb.AppendValues([]int64{10, 20, 30}, nil)
	// Row 1: [40]
	lb.Append(true)
	vb.Append(40)
	// Row 2: null list
	lb.AppendNull()
	// Row 3: [] (empty non-null)
	lb.Append(true)

	idArr := idB.NewArray()
	defer idArr.Release()
	tagsArr := lb.NewArray()
	defer tagsArr.Release()

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "tags", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(tagsArr.DataType(), []arrow.Array{tagsArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExplode_ListColumn(t *testing.T) {
	f := listExplodeFrame(t)
	out, err := f.Explode("tags")
	if err != nil {
		t.Fatal(err)
	}
	// Row 0 → 3, row 1 → 1, row 2 (null) → 1 null, row 3 ([]) → 1 null.
	if got := out.NumRows(); got != 6 {
		t.Fatalf("row count = %d, want 6", got)
	}
	tagsS, _ := out.Column("tags")
	if tagsS.DataType().ID() != arrow.INT64 {
		t.Fatalf("exploded tags type = %s, want INT64", tagsS.DataType())
	}
	tagsArr := tagsS.col.Data().Chunks()[0].(*array.Int64)
	// Values: 10, 20, 30, 40, null, null
	wantVal := []int64{10, 20, 30, 40}
	for i, w := range wantVal {
		if tagsArr.IsNull(i) || tagsArr.Value(i) != w {
			t.Fatalf("tags row %d = %d (null=%v), want %d", i, tagsArr.Value(i), tagsArr.IsNull(i), w)
		}
	}
	if !tagsArr.IsNull(4) || !tagsArr.IsNull(5) {
		t.Fatalf("tags rows 4,5 should be null: %v %v", tagsArr.IsNull(4), tagsArr.IsNull(5))
	}
	// id column duplicated: 1, 1, 1, 2, 3, 4
	idArr := out.series[0].col.Data().Chunks()[0].(*array.Int64)
	wantID := []int64{1, 1, 1, 2, 3, 4}
	for i, w := range wantID {
		if idArr.Value(i) != w {
			t.Fatalf("id row %d = %d, want %d", i, idArr.Value(i), w)
		}
	}
}

func TestExplode_MissingColumnErrors(t *testing.T) {
	f := mixedGeomFrame(t)
	_, err := f.Explode("nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("expected ErrColumnNotFound, got %v", err)
	}
}

func TestLazyExplode_GeometryMatchesEager(t *testing.T) {
	f := mixedGeomFrame(t)
	eager, err := f.Explode("geometry")
	if err != nil {
		t.Fatal(err)
	}
	lazy, err := f.Lazy().Explode("geometry").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if eager.NumRows() != lazy.NumRows() {
		t.Fatalf("row count mismatch: eager=%d lazy=%d", eager.NumRows(), lazy.NumRows())
	}
	eName, _ := eager.Column("name")
	lName, _ := lazy.Column("name")
	eArr := eName.col.Data().Chunks()[0].(*array.String)
	lArr := lName.col.Data().Chunks()[0].(*array.String)
	for i := 0; i < eager.NumRows(); i++ {
		if eArr.Value(i) != lArr.Value(i) {
			t.Fatalf("row %d name eager=%q lazy=%q", i, eArr.Value(i), lArr.Value(i))
		}
	}
}

func TestLazyExplode_ListSchemaChanges(t *testing.T) {
	f := listExplodeFrame(t)
	lf := f.Lazy().Explode("tags")
	// Plan-time schema should already reflect the element type, not the
	// list-of type. This is what makes downstream WithColumn / Filter
	// operate on tags-as-Int64 without materializing.
	tagsField, ok := lf.Schema().FieldsByName("tags")
	if !ok || len(tagsField) == 0 {
		t.Fatalf("tags field missing from lazy explode schema: %s", lf.Schema())
	}
	if tagsField[0].Type.ID() != arrow.INT64 {
		t.Fatalf("lazy explode tags field type = %s, want INT64", tagsField[0].Type)
	}
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 6 {
		t.Fatalf("row count = %d, want 6", out.NumRows())
	}
}

func TestLazyExplode_ComposesWithFilterAndSelect(t *testing.T) {
	// Explode a list, then filter on the exploded element, then select
	// two columns. Verifies the lazy chain preserves per-row semantics
	// through the row-cardinality change.
	f := listExplodeFrame(t)
	out, err := f.Lazy().
		Explode("tags").
		Filter(Col("tags").Gt(Lit(int64(15)))).
		Select(Col("id"), Col("tags")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// tags after explode: 10, 20, 30, 40, null, null. Filter keeps
	// non-null > 15 → 20, 30, 40, giving 3 rows.
	if out.NumRows() != 3 {
		t.Fatalf("row count = %d, want 3", out.NumRows())
	}
	tagsS, _ := out.Column("tags")
	tagsArr := tagsS.col.Data().Chunks()[0].(*array.Int64)
	want := []int64{20, 30, 40}
	for i, w := range want {
		if tagsArr.Value(i) != w {
			t.Fatalf("tags row %d = %d, want %d", i, tagsArr.Value(i), w)
		}
	}
}

func TestLazyExplode_MissingColumnSurfacesAtCollect(t *testing.T) {
	f := mixedGeomFrame(t)
	// Plan-build should succeed (schema is best-effort); the error only
	// surfaces once Collect asks the eager engine to actually explode.
	lf := f.Lazy().Explode("nope")
	if _, err := lf.Collect(); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("expected ErrColumnNotFound, got %v", err)
	}
}

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

package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// castFixtureFrame builds a small frame with one column per source
// numeric type. Used to exercise every cast source in the matrix.
func castFixtureFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	f64B := array.NewFloat64Builder(pool)
	defer f64B.Release()
	f64B.AppendValues([]float64{1.5, 2.7, 3.9}, nil)

	f32B := array.NewFloat32Builder(pool)
	defer f32B.Release()
	f32B.AppendValues([]float32{1.5, 2.7, 3.9}, nil)

	i64B := array.NewInt64Builder(pool)
	defer i64B.Release()
	i64B.AppendValues([]int64{1, 2, 3}, nil)

	i32B := array.NewInt32Builder(pool)
	defer i32B.Release()
	i32B.AppendValues([]int32{1, 2, 3}, nil)

	u64B := array.NewUint64Builder(pool)
	defer u64B.Release()
	u64B.AppendValues([]uint64{1, 2, 3}, nil)

	u32B := array.NewUint32Builder(pool)
	defer u32B.Release()
	u32B.AppendValues([]uint32{1, 2, 3}, nil)

	fields := []arrow.Field{
		{Name: "f64", Type: arrow.PrimitiveTypes.Float64},
		{Name: "f32", Type: arrow.PrimitiveTypes.Float32},
		{Name: "i64", Type: arrow.PrimitiveTypes.Int64},
		{Name: "i32", Type: arrow.PrimitiveTypes.Int32},
		{Name: "u64", Type: arrow.PrimitiveTypes.Uint64},
		{Name: "u32", Type: arrow.PrimitiveTypes.Uint32},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{f64B.NewArray(), f32B.NewArray(), i64B.NewArray(), i32B.NewArray(), u64B.NewArray(), u32B.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	fr, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return fr
}

// TestCast_ToFloat64FromAllNumeric — every supported numeric source
// widens correctly to Float64.
func TestCast_ToFloat64FromAllNumeric(t *testing.T) {
	f := castFixtureFrame(t)
	cases := []struct {
		col  string
		want []float64
	}{
		{"f64", []float64{1.5, 2.7, 3.9}},
		{"f32", []float64{1.5, 2.7, 3.9}}, // float32→float64 loses no precision here
		{"i64", []float64{1, 2, 3}},
		{"i32", []float64{1, 2, 3}},
		{"u64", []float64{1, 2, 3}},
		{"u32", []float64{1, 2, 3}},
	}
	for _, tc := range cases {
		out, err := f.WithColumnExpr("out", Col(tc.col).Cast(arrow.PrimitiveTypes.Float64))
		if err != nil {
			t.Fatalf("Cast %s→Float64: %v", tc.col, err)
		}
		col, _ := out.Column("out")
		if col.DataType().ID() != arrow.FLOAT64 {
			t.Errorf("%s: out type = %s, want FLOAT64", tc.col, col.DataType())
			continue
		}
		arr := col.col.Data().Chunks()[0].(*array.Float64)
		for i, want := range tc.want {
			// f32→f64 might not be exact for arbitrary values; use
			// tolerance for the float32 source.
			if tc.col == "f32" {
				if diff := float32(arr.Value(i)) - float32(want); diff*diff > 1e-6 {
					t.Errorf("%s row %d = %v, want ~%v", tc.col, i, arr.Value(i), want)
				}
				continue
			}
			if arr.Value(i) != want {
				t.Errorf("%s row %d = %v, want %v", tc.col, i, arr.Value(i), want)
			}
		}
	}
}

// TestCast_ToInt64TruncatesFloats — Float64→Int64 truncates
// fractional part (Go's numeric conversion semantics).
func TestCast_ToInt64TruncatesFloats(t *testing.T) {
	f := castFixtureFrame(t)
	out, err := f.WithColumnExpr("out", Col("f64").Cast(arrow.PrimitiveTypes.Int64))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("out")
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	// f64 = [1.5, 2.7, 3.9] → truncated = [1, 2, 3]
	want := []int64{1, 2, 3}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

// TestCast_SameTypeIsNoop — casting to the same type returns the
// input Series unchanged.
func TestCast_SameTypeIsNoop(t *testing.T) {
	f := castFixtureFrame(t)
	out, err := f.WithColumnExpr("out", Col("f64").Cast(arrow.PrimitiveTypes.Float64))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("out")
	arr := col.col.Data().Chunks()[0].(*array.Float64)
	want := []float64{1.5, 2.7, 3.9}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

// TestCast_NullPropagation — null rows in the source produce null
// rows in the output.
func TestCast_NullPropagation(t *testing.T) {
	pool := memory.DefaultAllocator
	iB := array.NewInt64Builder(pool)
	defer iB.Release()
	iB.Append(10)
	iB.AppendNull()
	iB.Append(30)
	arr := iB.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	cols := []arrow.Column{*arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.WithColumnExpr("out", Col("x").Cast(arrow.PrimitiveTypes.Float64))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("out")
	fa := col.col.Data().Chunks()[0].(*array.Float64)
	if fa.IsNull(0) || fa.Value(0) != 10 {
		t.Fatalf("row 0 = %v (null=%v), want 10", fa.Value(0), fa.IsNull(0))
	}
	if !fa.IsNull(1) {
		t.Fatalf("row 1 should be null, got %v", fa.Value(1))
	}
	if fa.IsNull(2) || fa.Value(2) != 30 {
		t.Fatalf("row 2 = %v, want 30", fa.Value(2))
	}
}

// TestCast_UnblocksIfMixedNumeric — the sharp edge Cast was
// designed to fix: If with mixed numeric literals used to error on
// type mismatch. Now works via explicit Cast.
func TestCast_UnblocksIfMixedNumeric(t *testing.T) {
	f := castFixtureFrame(t)
	// Without Cast: If(cond, Lit(int64(1)), Lit(1.5)) errors because
	// then=Int64 and otherwise=Float64. With Cast, the int side
	// widens to Float64 and the branches match.
	out, err := f.WithColumnExpr("mixed",
		If(Col("i64").Gt(Lit(int64(1))),
			Col("i64").Cast(arrow.PrimitiveTypes.Float64),
			Lit(1.5)))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("mixed")
	if col.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("mixed type = %s, want FLOAT64 (Cast widened Int64→Float64)", col.DataType())
	}
	// i64 = [1, 2, 3]. Predicate `i64 > 1`:
	//   row 0: 1 > 1 → false → 1.5
	//   row 1: 2 > 1 → true → 2.0 (cast from int64)
	//   row 2: 3 > 1 → true → 3.0
	arr := col.col.Data().Chunks()[0].(*array.Float64)
	want := []float64{1.5, 2.0, 3.0}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

// TestCast_UnsupportedTargetErrors — casting to a non-numeric target
// (e.g., String) errors with ExprTypeMismatch.
func TestCast_UnsupportedTargetErrors(t *testing.T) {
	f := castFixtureFrame(t)
	_, err := f.WithColumnExpr("bad", Col("i64").Cast(arrow.BinaryTypes.String))
	if err == nil {
		t.Fatal("expected error for unsupported target type String")
	}
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("error should wrap ErrExprTypeMismatch, got %v", err)
	}
}

// TestCast_UnsupportedSourceErrors — casting from a non-numeric
// source (e.g., String) errors.
func TestCast_UnsupportedSourceErrors(t *testing.T) {
	pool := memory.DefaultAllocator
	sB := array.NewStringBuilder(pool)
	defer sB.Release()
	sB.AppendValues([]string{"a", "b"}, nil)
	arr := sB.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "s", Type: arrow.BinaryTypes.String}
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	cols := []arrow.Column{*arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WithColumnExpr("bad", Col("s").Cast(arrow.PrimitiveTypes.Float64))
	if err == nil {
		t.Fatal("expected error for String → Float64 (unsupported source)")
	}
}

// TestCast_TypeInferenceReportsTarget — Cast's Type() returns the
// target type without evaluating.
func TestCast_TypeInferenceReportsTarget(t *testing.T) {
	f := castFixtureFrame(t)
	lf := f.Lazy().WithColumn("out", Col("i32").Cast(arrow.PrimitiveTypes.Float64))
	fields, _ := lf.Schema().FieldsByName("out")
	if len(fields) == 0 || fields[0].Type.ID() != arrow.FLOAT64 {
		t.Fatalf("out field type = %v, want FLOAT64", fields)
	}
}

// TestCast_NoRefcountLeak guards against the arrow.NewChunked
// refcount contract being misread. NewChunked RETAINS each input
// array (does not steal ownership) — callers keep the initial
// ref and must Release it. A CheckedAllocator here would report
// leaked buffers if castSeries ever regresses on this discipline.
// Runs a mix of source types through Cast, then releases the
// output Series and asserts the allocator's checked-size returns
// to zero.
func TestCast_NoRefcountLeak(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	// Build a fixture on the checked pool so every buffer allocated
	// during the test flows through the leak tracker.
	f64B := array.NewFloat64Builder(pool)
	f64B.AppendValues([]float64{1.5, 2.7, 3.9, 4.1, 5.2}, nil)
	f64Arr := f64B.NewArray()
	f64B.Release()

	field := arrow.Field{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	chunked := arrow.NewChunked(f64Arr.DataType(), []arrow.Array{f64Arr})
	f64Arr.Release()
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	frame, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer frame.Release()

	// Cast Float64 → Int64 (goes through arrow.compute.CastArray).
	// If the outChunks = nil bug reappears, CastArray's initial
	// ref leaks and the checked allocator's deferred AssertSize
	// fails.
	out, err := Col("v").Cast(arrow.PrimitiveTypes.Int64).Node().Eval(frame)
	if err != nil {
		t.Fatal(err)
	}
	// Series's underlying column is the Cast output — Release the
	// column (via a Frame wrapper so we can call Release cleanly).
	outCol := out.Column()
	outCol.Release()
}

// TestCast_LazyPipeline — Cast round-trips through Compile + Execute.
func TestCast_LazyPipeline(t *testing.T) {
	f := castFixtureFrame(t)
	out, err := f.Lazy().
		WithColumn("i64_as_f", Col("i64").Cast(arrow.PrimitiveTypes.Float64)).
		Filter(Col("i64_as_f").Gt(Lit(1.5))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("filtered row count = %d, want 2 (i64 in [1,2,3] cast → filter > 1.5)", out.NumRows())
	}
}

package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// medianModeFrame: group column + one numeric column (for Median) +
// one string column (for Mode).
func medianModeFrame(t testing.TB) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	group := array.NewStringBuilder(pool)
	defer group.Release()
	group.AppendValues([]string{"a", "a", "a", "a", "b", "b", "b", "c"}, nil)
	value := array.NewFloat64Builder(pool)
	defer value.Release()
	// group a: [10, 20, 30, 40] — median = (20+30)/2 = 25
	// group b: [5, 15, 25]     — median = 15
	// group c: [100]           — median = 100
	value.AppendValues([]float64{10, 20, 30, 40, 5, 15, 25, 100}, nil)
	label := array.NewStringBuilder(pool)
	defer label.Release()
	// group a: red, red, blue, red → mode = red (count 3)
	// group b: green, blue, blue   → mode = blue (count 2)
	// group c: purple              → mode = purple
	label.AppendValues([]string{"red", "red", "blue", "red", "green", "blue", "blue", "purple"}, nil)

	fields := []arrow.Field{
		{Name: "group", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "label", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{group.NewArray(), value.NewArray(), label.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestGroupBy_Median — per-group Bessel-style median with even/odd
// group sizes and singletons.
func TestGroupBy_Median(t *testing.T) {
	f := medianModeFrame(t)
	gb, err := f.GroupBy("group")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "value", Kind: AggMedian, Alias: "med",
	})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("med")
	if col.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("median dtype = %s, want FLOAT64", col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Float64)
	got := map[string]float64{}
	gCol, _ := out.Column("group")
	gArr := gCol.col.Data().Chunks()[0].(*array.String)
	for i := range gArr.Len() {
		got[gArr.Value(i)] = arr.Value(i)
	}
	want := map[string]float64{"a": 25, "b": 15, "c": 100}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("median[%s] = %v, want %v", k, got[k], w)
		}
	}
}

// TestGroupBy_Mode — most-frequent-value with ties broken by
// first-seen order. Preserves the source column's arrow type (String).
func TestGroupBy_Mode(t *testing.T) {
	f := medianModeFrame(t)
	gb, err := f.GroupBy("group")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "label", Kind: AggMode, Alias: "mode_label",
	})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("mode_label")
	if col.DataType().ID() != arrow.STRING {
		t.Fatalf("mode dtype = %s, want STRING (source-preserving)", col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.String)
	gCol, _ := out.Column("group")
	gArr := gCol.col.Data().Chunks()[0].(*array.String)
	got := map[string]string{}
	for i := range gArr.Len() {
		got[gArr.Value(i)] = arr.Value(i)
	}
	want := map[string]string{"a": "red", "b": "blue", "c": "purple"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("mode[%s] = %q, want %q", k, got[k], w)
		}
	}
}

// TestGroupBy_ModeTieBreak — when two values have the same top count
// within a group, first-seen wins. Order in the input matters.
func TestGroupBy_ModeTieBreak(t *testing.T) {
	pool := memory.DefaultAllocator
	group := array.NewStringBuilder(pool)
	defer group.Release()
	group.AppendValues([]string{"g", "g", "g", "g"}, nil)
	label := array.NewStringBuilder(pool)
	defer label.Release()
	// Two apples then two bananas → both tied at 2; "apple" was
	// seen first, so mode = "apple".
	label.AppendValues([]string{"apple", "apple", "banana", "banana"}, nil)
	fields := []arrow.Field{
		{Name: "group", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "label", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{group.NewArray(), label.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	gb, err := f.GroupBy("group")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "label", Kind: AggMode, Alias: "mode",
	})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("mode")
	arr := col.col.Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "apple" {
		t.Errorf("mode = %q, want %q (first-seen tie-break)", arr.Value(0), "apple")
	}
}

// TestGroupBy_MedianNullPropagation — nulls in the source column are
// skipped; a group with all nulls emits a null median.
func TestGroupBy_MedianNullPropagation(t *testing.T) {
	pool := memory.DefaultAllocator
	group := array.NewStringBuilder(pool)
	defer group.Release()
	group.AppendValues([]string{"a", "a", "a", "b", "b"}, nil)
	value := array.NewFloat64Builder(pool)
	defer value.Release()
	// group a: [1.0, null, 3.0] → non-null values [1.0, 3.0] → median 2
	// group b: [null, null]     → all-null → null median
	value.Append(1.0)
	value.AppendNull()
	value.Append(3.0)
	value.AppendNull()
	value.AppendNull()

	fields := []arrow.Field{
		{Name: "group", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{group.NewArray(), value.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	gb, err := f.GroupBy("group")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "value", Kind: AggMedian, Alias: "med",
	})
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("med")
	arr := col.col.Data().Chunks()[0].(*array.Float64)
	gCol, _ := out.Column("group")
	gArr := gCol.col.Data().Chunks()[0].(*array.String)
	for i := range gArr.Len() {
		switch gArr.Value(i) {
		case "a":
			if arr.IsNull(i) || arr.Value(i) != 2.0 {
				t.Errorf("median[a] = %v (null=%v), want 2.0", arr.Value(i), arr.IsNull(i))
			}
		case "b":
			if !arr.IsNull(i) {
				t.Errorf("median[b] should be null, got %v", arr.Value(i))
			}
		}
	}
}

// TestLazyAgg_MedianAndMode — Median + Mode via the streaming
// aggregate executor (LazyFrame path).
func TestLazyAgg_MedianAndMode(t *testing.T) {
	f := medianModeFrame(t)
	out, err := f.Lazy().
		GroupBy("group").
		Agg(
			Aggregation{Column: "value", Kind: AggMedian, Alias: "med"},
			Aggregation{Column: "label", Kind: AggMode, Alias: "mode_label"},
		).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 3 {
		t.Fatalf("row count = %d, want 3", out.NumRows())
	}
	medCol, _ := out.Column("med")
	if medCol.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("med dtype = %s, want FLOAT64", medCol.DataType())
	}
	modeCol, _ := out.Column("mode_label")
	if modeCol.DataType().ID() != arrow.STRING {
		t.Fatalf("mode_label dtype = %s, want STRING", modeCol.DataType())
	}
	gCol, _ := out.Column("group")
	gArr := gCol.col.Data().Chunks()[0].(*array.String)
	medArr := medCol.col.Data().Chunks()[0].(*array.Float64)
	modeArr := modeCol.col.Data().Chunks()[0].(*array.String)
	wantMed := map[string]float64{"a": 25, "b": 15, "c": 100}
	wantMode := map[string]string{"a": "red", "b": "blue", "c": "purple"}
	for i := range gArr.Len() {
		k := gArr.Value(i)
		if medArr.Value(i) != wantMed[k] {
			t.Errorf("med[%s] = %v, want %v", k, medArr.Value(i), wantMed[k])
		}
		if modeArr.Value(i) != wantMode[k] {
			t.Errorf("mode[%s] = %q, want %q", k, modeArr.Value(i), wantMode[k])
		}
	}
}

// TestExpr_MedianModeOver — Median and Mode chain through Over.
// Median broadcasts a Float64 per-partition median to every input row;
// Mode preserves the source column's arrow type.
func TestExpr_MedianModeOver(t *testing.T) {
	f := medianModeFrame(t)
	out, err := f.WithColumnExpr("med_over", Col("value").Median().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("med_over")
	if col.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("med_over dtype = %s, want FLOAT64", col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Float64)
	// group a rows: [25, 25, 25, 25]; group b: [15, 15, 15]; group c: [100]
	want := []float64{25, 25, 25, 25, 15, 15, 15, 100}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d med_over = %v, want %v", i, arr.Value(i), w)
		}
	}

	out, err = f.WithColumnExpr("mode_over", Col("label").Mode().Over("group"))
	if err != nil {
		t.Fatal(err)
	}
	col, _ = out.Column("mode_over")
	if col.DataType().ID() != arrow.STRING {
		t.Fatalf("mode_over dtype = %s, want STRING", col.DataType())
	}
	mArr := col.col.Data().Chunks()[0].(*array.String)
	wantMode := []string{"red", "red", "red", "red", "blue", "blue", "blue", "purple"}
	for i, w := range wantMode {
		if mArr.Value(i) != w {
			t.Errorf("row %d mode_over = %q, want %q", i, mArr.Value(i), w)
		}
	}
}

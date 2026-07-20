package gobi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// modeAggregator returns the most frequently occurring Int64 value in a
// group. Ties broken by first-seen. Emits nulls when the group is empty.
type modeAggregator struct{}

func (modeAggregator) Aggregate(s Series, rows []int) (any, error) {
	chunk := s.col.Data().Chunks()[0].(*array.Int64)
	counts := make(map[int64]int, len(rows))
	var order []int64
	for _, r := range rows {
		if chunk.IsNull(r) {
			continue
		}
		v := chunk.Value(r)
		if _, seen := counts[v]; !seen {
			order = append(order, v)
		}
		counts[v]++
	}
	if len(order) == 0 {
		return nil, nil
	}
	bestVal, bestCount := order[0], counts[order[0]]
	for _, v := range order[1:] {
		if counts[v] > bestCount {
			bestVal, bestCount = v, counts[v]
		}
	}
	return bestVal, nil
}
func (modeAggregator) Type() arrow.DataType { return arrow.PrimitiveTypes.Int64 }
func (modeAggregator) Name() string         { return "mode" }

// countDistinctAggregator returns the number of distinct non-null values.
type countDistinctAggregator struct{}

func (countDistinctAggregator) Aggregate(s Series, rows []int) (any, error) {
	chunk := s.col.Data().Chunks()[0].(*array.String)
	set := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if chunk.IsNull(r) {
			continue
		}
		set[chunk.Value(r)] = struct{}{}
	}
	return int64(len(set)), nil
}
func (countDistinctAggregator) Type() arrow.DataType { return arrow.PrimitiveTypes.Int64 }
func (countDistinctAggregator) Name() string         { return "ndv" }

// badTypeAggregator declares Uint64 but returns int64 — used to verify
// that Agg surfaces a helpful mismatch error.
type badTypeAggregator struct{}

func (badTypeAggregator) Aggregate(Series, []int) (any, error) { return int64(1), nil }
func (badTypeAggregator) Type() arrow.DataType                 { return arrow.PrimitiveTypes.Uint64 }
func (badTypeAggregator) Name() string                         { return "bad" }

// buildGroupFrame constructs (group string, val int64, tag string)
// with three groups of varying sizes.
func buildGroupFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	gb := array.NewStringBuilder(pool)
	defer gb.Release()
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	tb := array.NewStringBuilder(pool)
	defer tb.Release()
	rows := []struct {
		g   string
		v   int64
		tag string
	}{
		{"A", 1, "x"}, {"A", 1, "y"}, {"A", 2, "x"},
		{"B", 7, "z"}, {"B", 7, "z"}, {"B", 8, "z"}, {"B", 8, "z"},
		{"C", 3, "w"},
	}
	for _, r := range rows {
		gb.Append(r.g)
		vb.Append(r.v)
		tb.Append(r.tag)
	}
	fields := []arrow.Field{
		{Name: "g", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "tag", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{gb.NewArray(), vb.NewArray(), tb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
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

func TestGroupBy_AggCustom_Mode(t *testing.T) {
	f := buildGroupFrame(t)
	g, err := f.GroupBy("g")
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.Agg(Aggregation{Column: "v", Fn: modeAggregator{}})
	if err != nil {
		t.Fatal(err)
	}
	// Expected modes: A→1 (appears twice), B→7 (first-seen wins tie), C→3.
	names := out.ColumnNames()
	if names[1] != "v_mode" {
		t.Fatalf("output col name = %q, want v_mode", names[1])
	}
	modeCol, _ := out.Column("v_mode")
	modes := modeCol.Column().Data().Chunks()[0].(*array.Int64)
	want := []int64{1, 7, 3}
	for i, w := range want {
		if modes.Value(i) != w {
			t.Errorf("group %d mode = %d, want %d", i, modes.Value(i), w)
		}
	}
}

func TestGroupBy_AggCustom_MixedWithBuiltIn(t *testing.T) {
	// One built-in aggregation + one custom in the same call.
	f := buildGroupFrame(t)
	g, _ := f.GroupBy("g")
	out, err := g.Agg(
		Aggregation{Column: "v", Kind: AggSum},
		Aggregation{Column: "tag", Fn: countDistinctAggregator{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if names[1] != "v_sum" || names[2] != "tag_ndv" {
		t.Fatalf("col names = %v", names)
	}
	sums := mustCol(t, out, "v_sum").Column().Data().Chunks()[0].(*array.Float64)
	if sums.Value(0) != 4 { // 1+1+2
		t.Errorf("A sum = %v, want 4", sums.Value(0))
	}
	ndv := mustCol(t, out, "tag_ndv").Column().Data().Chunks()[0].(*array.Int64)
	// A has tags {x, y}, B has {z}, C has {w}.
	want := []int64{2, 1, 1}
	for i, w := range want {
		if ndv.Value(i) != w {
			t.Errorf("group %d ndv = %d, want %d", i, ndv.Value(i), w)
		}
	}
}

func TestGroupBy_AggCustom_Alias(t *testing.T) {
	f := buildGroupFrame(t)
	g, _ := f.GroupBy("g")
	out, err := g.Agg(Aggregation{
		Column: "v", Fn: modeAggregator{}, Alias: "typical",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ColumnNames()[1] != "typical" {
		t.Fatalf("alias not applied: %v", out.ColumnNames())
	}
}

func TestGroupBy_AggCustom_TypeMismatch(t *testing.T) {
	f := buildGroupFrame(t)
	g, _ := f.GroupBy("g")
	_, err := g.Agg(Aggregation{Column: "v", Fn: badTypeAggregator{}})
	if err == nil {
		t.Fatal("expected type-mismatch error")
	}
	if !contains(err.Error(), "declared Uint64") {
		t.Fatalf("mismatch error should name declared type: %v", err)
	}
}

func TestGroupBy_KeysUint64(t *testing.T) {
	// Simulate an H3-cell group key: group by uint64 cells, sum a float
	// value inside each cell.
	pool := memory.DefaultAllocator
	cellB := array.NewUint64Builder(pool)
	defer cellB.Release()
	valB := array.NewFloat64Builder(pool)
	defer valB.Release()
	cells := []uint64{0xdead, 0xbeef, 0xdead, 0xbeef, 0xdead}
	vals := []float64{1, 10, 2, 20, 3}
	cellB.AppendValues(cells, nil)
	valB.AppendValues(vals, nil)
	fields := []arrow.Field{
		{Name: "h3", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{cellB.NewArray(), valB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	g, err := f.GroupBy("h3")
	if err != nil {
		t.Fatalf("uint64 key rejected: %v", err)
	}
	out, err := g.Agg(Aggregation{Column: "v", Kind: AggSum})
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("groups = %d, want 2", out.NumRows())
	}
	// Confirm the key column type is preserved.
	keyCol, _ := out.Column("h3")
	if keyCol.DataType().ID() != arrow.UINT64 {
		t.Fatalf("key type dropped: %s", keyCol.DataType())
	}
}

func TestGroupBy_KeysTimestamp(t *testing.T) {
	pool := memory.DefaultAllocator
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond}
	tsB := array.NewTimestampBuilder(pool, tsType)
	defer tsB.Release()
	valB := array.NewInt64Builder(pool)
	defer valB.Release()
	// Two distinct timestamps, three rows.
	tsB.Append(arrow.Timestamp(1_000_000))
	tsB.Append(arrow.Timestamp(2_000_000))
	tsB.Append(arrow.Timestamp(1_000_000))
	valB.AppendValues([]int64{5, 7, 3}, nil)

	fields := []arrow.Field{
		{Name: "when", Type: tsType, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{tsB.NewArray(), valB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.GroupBy("when")
	if err != nil {
		t.Fatalf("timestamp key rejected: %v", err)
	}
	out, err := g.Agg(Aggregation{Column: "v", Kind: AggSum})
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("groups = %d, want 2", out.NumRows())
	}
	keyCol, _ := out.Column("when")
	if keyCol.DataType().ID() != arrow.TIMESTAMP {
		t.Fatalf("timestamp key type dropped: %s", keyCol.DataType())
	}
}

// -- helpers -----------------------------------------------------------

func mustCol(t *testing.T, f *Frame, name string) Series {
	t.Helper()
	s, err := f.Column(name)
	if err != nil {
		t.Fatalf("missing col %q: %v", name, err)
	}
	return s
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && stringIndex(s, sub) >= 0
}
func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// silence unused import warnings when tests are rearranged.
var _ = errors.New
var _ = fmt.Sprintf

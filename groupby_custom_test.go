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
//
// Pointer receiver + state fields so Merge can combine partial counts
// from a peer that saw disjoint rows for the same group.
type modeAggregator struct {
	counts map[int64]int
	order  []int64
}

func (m *modeAggregator) Aggregate(s Series, rows []int) (any, error) {
	// Reset — the same instance is reused across groups by the eager
	// engine, so per-group state must not leak.
	m.counts = make(map[int64]int, len(rows))
	m.order = m.order[:0]
	chunk := s.col.Data().Chunks()[0].(*array.Int64)
	for _, r := range rows {
		if chunk.IsNull(r) {
			continue
		}
		v := chunk.Value(r)
		if _, seen := m.counts[v]; !seen {
			m.order = append(m.order, v)
		}
		m.counts[v]++
	}
	return m.currentValue(), nil
}

// currentValue returns the mode implied by m.counts, or nil for an
// empty aggregator. Separated so Merge can compute a merged value
// without repeating the tie-break logic.
func (m *modeAggregator) currentValue() any {
	if len(m.order) == 0 {
		return nil
	}
	bestVal, bestCount := m.order[0], m.counts[m.order[0]]
	for _, v := range m.order[1:] {
		if m.counts[v] > bestCount {
			bestVal, bestCount = v, m.counts[v]
		}
	}
	return bestVal
}

// Merge folds other's per-value counts into m, preserving first-seen
// order for tie-breaking.
func (m *modeAggregator) Merge(other Aggregator) error {
	o, ok := other.(*modeAggregator)
	if !ok {
		return fmt.Errorf("modeAggregator.Merge: peer is %T", other)
	}
	if m.counts == nil {
		m.counts = make(map[int64]int, len(o.counts))
	}
	for _, v := range o.order {
		if _, seen := m.counts[v]; !seen {
			m.order = append(m.order, v)
		}
		m.counts[v] += o.counts[v]
	}
	return nil
}
func (m *modeAggregator) Type() arrow.DataType { return arrow.PrimitiveTypes.Int64 }
func (m *modeAggregator) Name() string         { return "mode" }

// countDistinctAggregator returns the number of distinct non-null values.
type countDistinctAggregator struct {
	seen map[string]struct{}
}

func (c *countDistinctAggregator) Aggregate(s Series, rows []int) (any, error) {
	// Reset per group (see Aggregator docs).
	c.seen = make(map[string]struct{}, len(rows))
	chunk := s.col.Data().Chunks()[0].(*array.String)
	for _, r := range rows {
		if chunk.IsNull(r) {
			continue
		}
		c.seen[chunk.Value(r)] = struct{}{}
	}
	return int64(len(c.seen)), nil
}
func (c *countDistinctAggregator) Merge(other Aggregator) error {
	o, ok := other.(*countDistinctAggregator)
	if !ok {
		return fmt.Errorf("countDistinctAggregator.Merge: peer is %T", other)
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{}, len(o.seen))
	}
	for k := range o.seen {
		c.seen[k] = struct{}{}
	}
	return nil
}
func (c *countDistinctAggregator) Type() arrow.DataType { return arrow.PrimitiveTypes.Int64 }
func (c *countDistinctAggregator) Name() string         { return "ndv" }

// badTypeAggregator declares Uint64 but returns int64 — used to verify
// that Agg surfaces a helpful mismatch error.
type badTypeAggregator struct{}

func (badTypeAggregator) Aggregate(Series, []int) (any, error) { return int64(1), nil }
func (badTypeAggregator) Merge(Aggregator) error               { return nil }
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
	out, err := g.Agg(Aggregation{Column: "v", Fn: &modeAggregator{}})
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
		Aggregation{Column: "tag", Fn: &countDistinctAggregator{}},
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
		Column: "v", Fn: &modeAggregator{}, Alias: "typical",
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

// TestAggregatorMerge_ModeCombines exercises Aggregator.Merge directly.
// Two peer modeAggregators fed disjoint row subsets of the same group
// must combine (via Merge) into the same result as a single aggregator
// fed the union of rows. After Merge, currentValue() reveals the
// combined value — Aggregate would reset state (per interface docs)
// so peers use their internal accessor.
func TestAggregatorMerge_ModeCombines(t *testing.T) {
	f := buildGroupFrame(t)
	valS, _ := f.Column("v")

	// Serial baseline: rows 3-6 = group B's {7, 7, 8, 8}.
	serial := &modeAggregator{}
	serialResult, err := serial.Aggregate(valS, []int{3, 4, 5, 6})
	if err != nil {
		t.Fatal(err)
	}

	// Parallel: two peers, each sees half of group B, then merge.
	left := &modeAggregator{}
	if _, err := left.Aggregate(valS, []int{3, 4}); err != nil { // {7, 7}
		t.Fatal(err)
	}
	right := &modeAggregator{}
	if _, err := right.Aggregate(valS, []int{5, 6}); err != nil { // {8, 8}
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if merged := left.currentValue(); merged != serialResult {
		t.Fatalf("merge divergence: serial=%v merged=%v", serialResult, merged)
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

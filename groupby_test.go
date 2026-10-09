package gobi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// salesFrame returns a frame:
//
//	region string, product string, revenue float64, units int64
func salesFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	region := array.NewStringBuilder(pool)
	defer region.Release()
	region.AppendValues([]string{"NA", "NA", "EU", "EU", "NA", "APAC"}, nil)
	product := array.NewStringBuilder(pool)
	defer product.Release()
	product.AppendValues([]string{"A", "B", "A", "A", "A", "B"}, nil)
	revenue := array.NewFloat64Builder(pool)
	defer revenue.Release()
	revenue.AppendValues([]float64{100, 200, 300, 150, 50, 400}, nil)
	units := array.NewInt64Builder(pool)
	defer units.Release()
	units.AppendValues([]int64{10, 20, 30, 15, 5, 40}, nil)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "product", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "revenue", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "units", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{region.NewArray(), product.NewArray(), revenue.NewArray(), units.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGroupBy_SingleKey_Sum(t *testing.T) {
	f := salesFrame(t)
	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Kind: AggCount},
		Aggregation{Column: "revenue", Kind: AggSum},
	)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 3 || c != 3 {
		t.Fatalf("shape: (%d, %d) want (3, 3)", r, c)
	}
	// Sorted alphabetically by key: APAC, EU, NA
	regions, _ := out.Column("region")
	arr := regions.col.Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "APAC" || arr.Value(1) != "EU" || arr.Value(2) != "NA" {
		t.Fatalf("group order: %v", []string{arr.Value(0), arr.Value(1), arr.Value(2)})
	}
	// APAC revenue = 400, EU = 450, NA = 350
	rev, _ := out.Column("revenue_sum")
	revArr := rev.col.Data().Chunks()[0].(*array.Float64)
	if revArr.Value(0) != 400 || revArr.Value(1) != 450 || revArr.Value(2) != 350 {
		t.Fatalf("revenue sums: %v %v %v",
			revArr.Value(0), revArr.Value(1), revArr.Value(2))
	}
}

func TestGroupBy_MultipleKeys_MinMaxMean(t *testing.T) {
	f := salesFrame(t)
	gb, err := f.GroupBy("region", "product")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Column: "revenue", Kind: AggMean, Alias: "avg_rev"},
		Aggregation{Column: "units", Kind: AggMin, Alias: "min_units"},
		Aggregation{Column: "units", Kind: AggMax, Alias: "max_units"},
	)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	// Groups: (APAC,B), (EU,A), (NA,A), (NA,B) → 4 rows, 2+3 cols
	if r != 4 || c != 5 {
		t.Fatalf("shape: (%d, %d), want (4, 5)", r, c)
	}
	// (NA, A) has revenue 100 and 50 → mean 75; units 10, 5 → min 5, max 10.
	regions, _ := out.Column("region")
	products, _ := out.Column("product")
	regArr := regions.col.Data().Chunks()[0].(*array.String)
	prodArr := products.col.Data().Chunks()[0].(*array.String)
	naA := -1
	for i := range r {
		if regArr.Value(i) == "NA" && prodArr.Value(i) == "A" {
			naA = i
			break
		}
	}
	if naA < 0 {
		t.Fatalf("no (NA, A) group")
	}
	avg, _ := out.Column("avg_rev")
	avgV := avg.col.Data().Chunks()[0].(*array.Float64).Value(naA)
	if avgV != 75 {
		t.Fatalf("(NA,A) avg_rev = %v, want 75", avgV)
	}
	minU, _ := out.Column("min_units")
	if v := minU.col.Data().Chunks()[0].(*array.Float64).Value(naA); v != 5 {
		t.Fatalf("(NA,A) min_units = %v, want 5", v)
	}
	maxU, _ := out.Column("max_units")
	if v := maxU.col.Data().Chunks()[0].(*array.Float64).Value(naA); v != 10 {
		t.Fatalf("(NA,A) max_units = %v, want 10", v)
	}
}

// timestampGroupsFrame returns a frame:
//
//	group string, t timestamp[ns, tz=UTC]
//
// with two groups so min/max on the timestamp column has non-trivial
// output. Includes a null t in group A to check null-skipping.
func timestampGroupsFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	groupB := array.NewStringBuilder(pool)
	defer groupB.Release()
	groupB.AppendValues([]string{"A", "A", "A", "B", "B"}, nil)

	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond, TimeZone: "UTC"}
	tsB := array.NewTimestampBuilder(pool, tsType)
	defer tsB.Release()
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	tsB.Append(arrow.Timestamp(base.Add(3 * time.Hour).UnixNano()))
	tsB.Append(arrow.Timestamp(base.Add(1 * time.Hour).UnixNano()))
	tsB.AppendNull()
	tsB.Append(arrow.Timestamp(base.Add(5 * time.Hour).UnixNano()))
	tsB.Append(arrow.Timestamp(base.Add(7 * time.Hour).UnixNano()))

	fields := []arrow.Field{
		{Name: "group", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "t", Type: tsType, Nullable: true},
	}
	arrays := []arrow.Array{groupB.NewArray(), tsB.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestGroupBy_Timestamp_MinMax verifies that Min/Max on a Timestamp
// column preserves the source's TimestampType (unit + timezone) in
// the output schema and returns Timestamp values (not float64
// nanoseconds). Nulls are skipped.
func TestGroupBy_Timestamp_MinMax(t *testing.T) {
	f := timestampGroupsFrame(t)
	defer f.Release()

	gb, err := f.GroupBy("group")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Column: "t", Kind: AggMin, Alias: "first_t"},
		Aggregation{Column: "t", Kind: AggMax, Alias: "last_t"},
	)
	if err != nil {
		t.Fatalf("Agg: %v", err)
	}
	defer out.Release()

	// Schema check: min/max columns should be TimestampType, not
	// Float64. TimeZone should survive the round trip.
	firstF, ok := out.Schema().FieldsByName("first_t")
	if !ok || len(firstF) == 0 {
		t.Fatalf("no first_t column")
	}
	tsType, isTS := firstF[0].Type.(*arrow.TimestampType)
	if !isTS {
		t.Fatalf("first_t type = %s, want TimestampType", firstF[0].Type)
	}
	if tsType.TimeZone != "UTC" {
		t.Errorf("first_t timezone = %q, want UTC", tsType.TimeZone)
	}
	if tsType.Unit != arrow.Nanosecond {
		t.Errorf("first_t unit = %s, want Nanosecond", tsType.Unit)
	}

	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	// Groups sorted by string key: A, B.
	firstCol, _ := out.Column("first_t")
	firstArr := firstCol.col.Data().Chunks()[0].(*array.Timestamp)
	lastCol, _ := out.Column("last_t")
	lastArr := lastCol.col.Data().Chunks()[0].(*array.Timestamp)

	// Group A: non-null values are +3h, +1h → min +1h, max +3h.
	wantMinA := arrow.Timestamp(base.Add(1 * time.Hour).UnixNano())
	wantMaxA := arrow.Timestamp(base.Add(3 * time.Hour).UnixNano())
	if firstArr.Value(0) != wantMinA {
		t.Errorf("A min = %d, want %d", firstArr.Value(0), wantMinA)
	}
	if lastArr.Value(0) != wantMaxA {
		t.Errorf("A max = %d, want %d", lastArr.Value(0), wantMaxA)
	}
	// Group B: +5h, +7h → min +5h, max +7h.
	wantMinB := arrow.Timestamp(base.Add(5 * time.Hour).UnixNano())
	wantMaxB := arrow.Timestamp(base.Add(7 * time.Hour).UnixNano())
	if firstArr.Value(1) != wantMinB {
		t.Errorf("B min = %d, want %d", firstArr.Value(1), wantMinB)
	}
	if lastArr.Value(1) != wantMaxB {
		t.Errorf("B max = %d, want %d", lastArr.Value(1), wantMaxB)
	}
}

// TestLazyFrame_Timestamp_MinMax exercises the streaming aggregate
// (LazyFrame.GroupBy.Agg) rather than the eager path — the two share
// almost no code, so both need coverage.
func TestLazyFrame_Timestamp_MinMax(t *testing.T) {
	f := timestampGroupsFrame(t)
	defer f.Release()

	out, err := f.Lazy().
		GroupBy("group").
		Agg(
			Aggregation{Column: "t", Kind: AggMin, Alias: "first_t"},
			Aggregation{Column: "t", Kind: AggMax, Alias: "last_t"},
		).
		Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	defer out.Release()

	firstF, ok := out.Schema().FieldsByName("first_t")
	if !ok || len(firstF) == 0 {
		t.Fatalf("no first_t column")
	}
	if _, isTS := firstF[0].Type.(*arrow.TimestampType); !isTS {
		t.Fatalf("first_t type = %s, want TimestampType", firstF[0].Type)
	}

	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	firstCol, _ := out.Column("first_t")
	firstArr := firstCol.col.Data().Chunks()[0].(*array.Timestamp)
	wantMinA := arrow.Timestamp(base.Add(1 * time.Hour).UnixNano())
	wantMinB := arrow.Timestamp(base.Add(5 * time.Hour).UnixNano())
	// Groups may be in first-seen order (A, B) from the streaming
	// path — either ordering is valid, so check that both expected
	// values are present in the two output rows.
	got := []arrow.Timestamp{firstArr.Value(0), firstArr.Value(1)}
	has := func(want arrow.Timestamp) bool {
		return got[0] == want || got[1] == want
	}
	if !has(wantMinA) || !has(wantMinB) {
		t.Errorf("first_t values = %v, want both %d and %d present",
			got, wantMinA, wantMinB)
	}
}

func TestGroupBy_MissingKey(t *testing.T) {
	f := salesFrame(t)
	_, err := f.GroupBy("nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestGroupBy_NonHashableKey(t *testing.T) {
	f := smallFrame(t) // has a Binary geometry column
	_, err := f.GroupBy("geom")
	if err == nil {
		t.Fatalf("expected error grouping by Binary column")
	}
}

func TestGroupBy_NoAggregations(t *testing.T) {
	f := salesFrame(t)
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg()
	if err != nil {
		t.Fatal(err)
	}
	if r, c := out.Shape(); r != 3 || c != 1 {
		t.Fatalf("shape: (%d, %d), want (3, 1)", r, c)
	}
}

// TestGroupBy_CountOnUint64Column — AggCount on a non-numeric-shortlist
// column (Uint64 here) must count non-null rows without erroring. The
// eager engine used to call numericAt for the per-row check, which
// rejects UINT64; the fix routes through isNullAtSeries so any
// hashable column type works.
func TestGroupBy_CountOnUint64Column(t *testing.T) {
	pool := memory.DefaultAllocator
	region := array.NewStringBuilder(pool)
	defer region.Release()
	region.AppendValues([]string{"NA", "NA", "EU", "EU", "NA"}, nil)

	gridB := array.NewUint64Builder(pool)
	defer gridB.Release()
	// One null in the middle to prove non-null counting works.
	gridB.Append(100)
	gridB.Append(200)
	gridB.AppendNull()
	gridB.Append(400)
	gridB.Append(500)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "grid_path", Type: arrow.PrimitiveTypes.Uint64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{region.NewArray(), gridB.NewArray()}
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

	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{Column: "grid_path", Kind: AggCount, Alias: "n"})
	if err != nil {
		t.Fatalf("AggCount on Uint64 failed: %v", err)
	}

	// regions: NA, NA, EU, EU, NA. grid_path: 100, 200, null, 400, 500.
	// EU rows have one non-null (400) → count = 1. NA rows are all
	// non-null (100, 200, 500) → count = 3.
	regions, _ := out.Column("region")
	rArr := regions.col.Data().Chunks()[0].(*array.String)
	counts, _ := out.Column("n")
	cArr := counts.col.Data().Chunks()[0].(*array.Int64)
	// Sorted alphabetically: EU, NA
	if rArr.Value(0) != "EU" || cArr.Value(0) != 1 {
		t.Fatalf("EU count = %d, want 1 (one non-null)", cArr.Value(0))
	}
	if rArr.Value(1) != "NA" || cArr.Value(1) != 3 {
		t.Fatalf("NA count = %d, want 3", cArr.Value(1))
	}
}

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

// providersSetAggregator collects the distinct non-null string values
// seen per group into a List<String>. Mirrors the shape the user hit
// friction with — appendCustomValue used to reject *array.ListBuilder
// with "unhandled builder type", parking any real List<T>-emitting
// aggregator. The regression exercises the new dispatch arm.
type providersSetAggregator struct {
	seen  map[string]struct{}
	order []string
}

func (p *providersSetAggregator) Aggregate(s Series, rows []int) (any, error) {
	// Reset per group — the eager engine reuses one instance across groups.
	p.seen = make(map[string]struct{}, len(rows))
	p.order = p.order[:0]
	chunk := s.col.Data().Chunks()[0].(*array.String)
	for _, r := range rows {
		if chunk.IsNull(r) {
			continue
		}
		v := chunk.Value(r)
		if _, ok := p.seen[v]; ok {
			continue
		}
		p.seen[v] = struct{}{}
		p.order = append(p.order, v)
	}
	// Copy to avoid callers observing later resets on the same slice
	// backing array.
	out := make([]string, len(p.order))
	copy(out, p.order)
	return out, nil
}

func (p *providersSetAggregator) Merge(other Aggregator) error {
	o, ok := other.(*providersSetAggregator)
	if !ok {
		return fmt.Errorf("providersSetAggregator.Merge: peer is %T", other)
	}
	if p.seen == nil {
		p.seen = make(map[string]struct{}, len(o.seen))
	}
	for _, v := range o.order {
		if _, ok := p.seen[v]; ok {
			continue
		}
		p.seen[v] = struct{}{}
		p.order = append(p.order, v)
	}
	return nil
}

func (p *providersSetAggregator) Type() arrow.DataType {
	return arrow.ListOf(arrow.BinaryTypes.String)
}
func (p *providersSetAggregator) Name() string { return "providers" }

func TestCustomAggregator_ListStringOutput(t *testing.T) {
	pool := memory.DefaultAllocator

	region := array.NewStringBuilder(pool)
	defer region.Release()
	region.AppendValues([]string{"NA", "NA", "NA", "EU", "EU"}, nil)

	provider := array.NewStringBuilder(pool)
	defer provider.Release()
	// NA sees a, b, a again → set {a, b}. EU sees c, null → set {c}.
	provider.Append("a")
	provider.Append("b")
	provider.Append("a")
	provider.Append("c")
	provider.AppendNull()

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "provider", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{region.NewArray(), provider.NewArray()}
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

	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "provider",
		Fn:     &providersSetAggregator{},
		Alias:  "providers",
	})
	if err != nil {
		t.Fatalf("custom List<String> aggregator failed: %v", err)
	}

	providers, err := out.Column("providers")
	if err != nil {
		t.Fatal(err)
	}
	if providers.DataType().ID() != arrow.LIST {
		t.Fatalf("providers column type = %s, want LIST", providers.DataType())
	}
	listArr := providers.col.Data().Chunks()[0].(*array.List)
	values := listArr.ListValues().(*array.String)

	// Sorted alphabetically: EU (row 0), NA (row 1).
	getRow := func(row int) []string {
		start, end := listArr.ValueOffsets(row)
		out := make([]string, end-start)
		for i := start; i < end; i++ {
			out[i-start] = values.Value(int(i))
		}
		return out
	}
	euSet := getRow(0)
	if len(euSet) != 1 || euSet[0] != "c" {
		t.Fatalf("EU providers = %v, want [c]", euSet)
	}
	naSet := getRow(1)
	if len(naSet) != 2 || naSet[0] != "a" || naSet[1] != "b" {
		t.Fatalf("NA providers = %v, want [a b]", naSet)
	}
}

// structRowAggregator emits a two-field Struct{Count Int64, First
// String} — the minimal shape that exercises appendCustomValue's
// new *array.StructBuilder arm.
type structRowAggregator struct {
	count int64
	first string
	seen  bool
}

func (a *structRowAggregator) Aggregate(s Series, rows []int) (any, error) {
	a.count = 0
	a.first = ""
	a.seen = false
	chunk := s.col.Data().Chunks()[0].(*array.String)
	for _, r := range rows {
		if chunk.IsNull(r) {
			continue
		}
		if !a.seen {
			a.first = chunk.Value(r)
			a.seen = true
		}
		a.count++
	}
	if !a.seen {
		return nil, nil
	}
	return []any{a.count, a.first}, nil
}
func (a *structRowAggregator) Merge(Aggregator) error { return nil }
func (a *structRowAggregator) Type() arrow.DataType {
	return arrow.StructOf(
		arrow.Field{Name: "count", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		arrow.Field{Name: "first", Type: arrow.BinaryTypes.String, Nullable: true},
	)
}
func (a *structRowAggregator) Name() string { return "summary" }

func TestCustomAggregator_StructOutput(t *testing.T) {
	pool := memory.DefaultAllocator
	region := array.NewStringBuilder(pool)
	defer region.Release()
	region.AppendValues([]string{"NA", "NA", "EU"}, nil)
	name := array.NewStringBuilder(pool)
	defer name.Release()
	name.Append("alpha")
	name.Append("bravo")
	name.Append("charlie")

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{region.NewArray(), name.NewArray()}
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

	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "name",
		Fn:     &structRowAggregator{},
		Alias:  "summary",
	})
	if err != nil {
		t.Fatalf("custom Struct aggregator failed: %v", err)
	}
	summary, _ := out.Column("summary")
	if summary.DataType().ID() != arrow.STRUCT {
		t.Fatalf("summary type = %s, want STRUCT", summary.DataType())
	}
	// Sorted alphabetically: EU (row 0), NA (row 1).
	structArr := summary.col.Data().Chunks()[0].(*array.Struct)
	countArr := structArr.Field(0).(*array.Int64)
	firstArr := structArr.Field(1).(*array.String)
	if countArr.Value(0) != 1 || firstArr.Value(0) != "charlie" {
		t.Fatalf("EU summary = {%d, %q}, want {1, charlie}", countArr.Value(0), firstArr.Value(0))
	}
	if countArr.Value(1) != 2 || firstArr.Value(1) != "alpha" {
		t.Fatalf("NA summary = {%d, %q}, want {2, alpha}", countArr.Value(1), firstArr.Value(1))
	}
}

// filteredAggFrame builds a small frame with a "source" branch label,
// used to demonstrate the "unified pipeline" pattern:
//
//	region  source   provider    count
//	NA      "seg"    "att"       1
//	NA      "seg"    "verizon"   2
//	NA      "ping"   "att"       10   ← filtered out from seg-set
//	EU      "seg"    "vodafone"  3
//	EU      "ping"   "orange"    20   ← filtered out from seg-set
//	EU      "ping"   null        30
func filteredAggFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"NA", "NA", "NA", "EU", "EU", "EU"}, nil)

	sourceB := array.NewStringBuilder(pool)
	defer sourceB.Release()
	sourceB.AppendValues([]string{"seg", "seg", "ping", "seg", "ping", "ping"}, nil)

	providerB := array.NewStringBuilder(pool)
	defer providerB.Release()
	providerB.Append("att")
	providerB.Append("verizon")
	providerB.Append("att")
	providerB.Append("vodafone")
	providerB.Append("orange")
	providerB.AppendNull()

	countB := array.NewInt64Builder(pool)
	defer countB.Release()
	countB.AppendValues([]int64{1, 2, 10, 3, 20, 30}, nil)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "source", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "provider", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "count", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), sourceB.NewArray(), providerB.NewArray(), countB.NewArray()}
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
	return f
}

func TestFilteredAgg_SumOnMatchingRows(t *testing.T) {
	f := filteredAggFrame(t)
	// Sum of count where source=="seg", per region.
	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "count", Kind: AggSum, Alias: "seg_count",
		Filter: Col("source").Eq(Lit("seg")),
	})
	if err != nil {
		t.Fatalf("filtered agg failed: %v", err)
	}
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	// Sorted region: EU, NA
	//   EU seg-rows: {3}          → 3
	//   NA seg-rows: {1, 2}       → 3
	regions, _ := out.Column("region")
	rArr := regions.col.Data().Chunks()[0].(*array.String)
	sums, _ := out.Column("seg_count")
	sArr := sums.col.Data().Chunks()[0].(*array.Float64)
	if rArr.Value(0) != "EU" || sArr.Value(0) != 3 {
		t.Fatalf("EU seg_count = %v, want 3", sArr.Value(0))
	}
	if rArr.Value(1) != "NA" || sArr.Value(1) != 3 {
		t.Fatalf("NA seg_count = %v, want 3", sArr.Value(1))
	}
}

func TestFilteredAgg_CollectSetSkipsExcluded(t *testing.T) {
	f := filteredAggFrame(t)
	// This is the pipeline the user's workaround was designed for:
	// collect_set(provider) where source == "seg", per region.
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "provider",
		Fn:     NewStringSetAggregator(),
		Alias:  "seg_providers",
		Filter: Col("source").Eq(Lit("seg")),
	})
	if err != nil {
		t.Fatalf("filtered collect_set: %v", err)
	}
	providers, _ := out.Column("seg_providers")
	la := providers.col.Data().Chunks()[0].(*array.List)
	values := la.ListValues().(*array.String)
	getRow := func(row int) []string {
		start, end := la.ValueOffsets(row)
		out := make([]string, end-start)
		for j := start; j < end; j++ {
			out[j-start] = values.Value(int(j))
		}
		return out
	}
	// EU seg-rows have provider "vodafone".
	if got := getRow(0); len(got) != 1 || got[0] != "vodafone" {
		t.Fatalf("EU seg_providers = %v, want [vodafone]", got)
	}
	// NA seg-rows have "att" and "verizon".
	if got := getRow(1); len(got) != 2 || got[0] != "att" || got[1] != "verizon" {
		t.Fatalf("NA seg_providers = %v, want [att verizon]", got)
	}
}

// A single Agg call with two different Filter clauses — each agg
// independently narrows its own row set.
func TestFilteredAgg_MultipleIndependentFilters(t *testing.T) {
	f := filteredAggFrame(t)
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(
		Aggregation{
			Column: "count", Kind: AggSum, Alias: "seg_sum",
			Filter: Col("source").Eq(Lit("seg")),
		},
		Aggregation{
			Column: "count", Kind: AggSum, Alias: "ping_sum",
			Filter: Col("source").Eq(Lit("ping")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// EU: seg_sum=3, ping_sum=20+30=50
	// NA: seg_sum=1+2=3, ping_sum=10
	segArr := out.series[1].col.Data().Chunks()[0].(*array.Float64)
	pingArr := out.series[2].col.Data().Chunks()[0].(*array.Float64)
	if segArr.Value(0) != 3 || pingArr.Value(0) != 50 {
		t.Fatalf("EU: seg=%v ping=%v, want (3, 50)", segArr.Value(0), pingArr.Value(0))
	}
	if segArr.Value(1) != 3 || pingArr.Value(1) != 10 {
		t.Fatalf("NA: seg=%v ping=%v, want (3, 10)", segArr.Value(1), pingArr.Value(1))
	}
}

// Filter that produces a null result should treat null as false
// (SQL FILTER WHERE semantics — the row is excluded).
func TestFilteredAgg_NullFilterTreatedAsFalse(t *testing.T) {
	f := filteredAggFrame(t)
	// Filter: provider IsNotNull. Rows where provider is null are
	// excluded from the aggregation. In EU, row 5 has null provider.
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "count", Kind: AggSum, Alias: "known_sum",
		Filter: Col("provider").IsNotNull(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sumArr := out.series[1].col.Data().Chunks()[0].(*array.Float64)
	// EU non-null provider rows: 3 (seg vodafone) + 20 (ping orange) = 23
	// NA non-null provider rows: 1 + 2 + 10 = 13
	if sumArr.Value(0) != 23 {
		t.Fatalf("EU known_sum = %v, want 23", sumArr.Value(0))
	}
	if sumArr.Value(1) != 13 {
		t.Fatalf("NA known_sum = %v, want 13", sumArr.Value(1))
	}
}

// End-to-end via LazyFrame.Collect — the filter path routes through
// the materializing fallback (allBuiltInAggs rejects filtered aggs).
func TestFilteredAgg_LazyPipeline(t *testing.T) {
	f := filteredAggFrame(t)
	out, err := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{
			Column: "count", Kind: AggSum, Alias: "seg_sum",
			Filter: Col("source").Eq(Lit("seg")),
		}).
		Collect()
	if err != nil {
		t.Fatalf("lazy filtered agg: %v", err)
	}
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
}

func TestFilteredAgg_NonBooleanFilterErrors(t *testing.T) {
	f := filteredAggFrame(t)
	gb, _ := f.GroupBy("region")
	// Filter that produces a non-Boolean column.
	_, err := gb.Agg(Aggregation{
		Column: "count", Kind: AggSum,
		Filter: Col("count"), // Int64, not Boolean
	})
	if err == nil {
		t.Fatal("expected error for non-Boolean filter")
	}
}

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

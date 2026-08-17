package gobi

import (
	"fmt"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// sortedGroupByFrame builds a fixture with rows already sorted by
// (region, id) — a two-column key that bypasses aggFast (which
// handles single-column primitive keys only), so the aligned fast
// path in aggAligned is what actually differs from the general
// hash-map path.
//
// nRegions × nIDs × rowsPerGroup rows. Region + id both String;
// value is Int64.
func sortedGroupByFrame(t testing.TB, nRegions, nIDs, rowsPerGroup int) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	rb := array.NewStringBuilder(pool)
	defer rb.Release()
	ib := array.NewStringBuilder(pool)
	defer ib.Release()
	vb := array.NewInt64Builder(pool)
	defer vb.Release()

	for reg := range nRegions {
		rn := fmt.Sprintf("r%03d", reg)
		for id := range nIDs {
			in := fmt.Sprintf("id%06d", id)
			for r := range rowsPerGroup {
				rb.Append(rn)
				ib.Append(in)
				vb.Append(int64(reg*nIDs*rowsPerGroup + id*rowsPerGroup + r))
			}
		}
	}
	rArr := rb.NewArray()
	defer rArr.Release()
	iArr := ib.NewArray()
	defer iArr.Release()
	vArr := vb.NewArray()
	defer vArr.Release()

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "id", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(rArr.DataType(), []arrow.Array{rArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(iArr.DataType(), []arrow.Array{iArr})),
		*arrow.NewColumn(fields[2], arrow.NewChunked(vArr.DataType(), []arrow.Array{vArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// alignedGroupByMeta is the PartitionMetadata claim
// groupByFastPathApplicable requires — partitioned on (region, id),
// sorted on (region, id), SortEnforced=true.
func alignedGroupByMeta() *PartitionMetadata {
	return &PartitionMetadata{
		Columns:      []string{"region", "id"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "region"}, {Column: "id"}},
		SortEnforced: true,
	}
}

// TestGroupByAligned_MatchesSlowPath is the correctness oracle for
// step 8: the aligned+sorted fast path must produce identical
// output rows (values + order) to the general hash-map path. Uses
// a two-column key to bypass aggFast — the single-key fast path
// short-circuits before aggAligned runs on single-primitive-key
// workloads, so the multi-key shape is what actually exercises the
// step-8 code.
func TestGroupByAligned_MatchesSlowPath(t *testing.T) {
	// 4 regions × 5 ids × 10 rows = 200 rows, 20 unique (region, id)
	// groups.
	f := sortedGroupByFrame(t, 4, 5, 10)

	// Slow-path baseline: no metadata → hash-map path.
	slow, err := runGroupBySum(f)
	if err != nil {
		t.Fatalf("slow path: %v", err)
	}

	// Fast path: attach aligned+sorted metadata; groupByFastPathApplicable
	// returns true, aggAligned runs.
	f.WithPartitionMeta(alignedGroupByMeta())
	fast, err := runGroupBySum(f)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}

	if slow.NumRows() != fast.NumRows() {
		t.Fatalf("row-count divergence: slow=%d fast=%d",
			slow.NumRows(), fast.NumRows())
	}

	// Row-by-row parity. Slow path sorts unique group keys; fast
	// path emits in input order (which happens to be sorted). So
	// row indices should match 1:1.
	slowRegion := slow.series[0].col.Data().Chunks()[0].(*array.String)
	slowID := slow.series[1].col.Data().Chunks()[0].(*array.String)
	slowSum := slow.series[2].col.Data().Chunks()[0].(*array.Float64)
	fastRegion := fast.series[0].col.Data().Chunks()[0].(*array.String)
	fastID := fast.series[1].col.Data().Chunks()[0].(*array.String)
	fastSum := fast.series[2].col.Data().Chunks()[0].(*array.Float64)

	for i := 0; i < slow.NumRows(); i++ {
		if slowRegion.Value(i) != fastRegion.Value(i) || slowID.Value(i) != fastID.Value(i) {
			t.Fatalf("row %d key: slow=(%q,%q) fast=(%q,%q)",
				i, slowRegion.Value(i), slowID.Value(i),
				fastRegion.Value(i), fastID.Value(i))
		}
		if slowSum.Value(i) != fastSum.Value(i) {
			t.Fatalf("row %d sum: slow=%v fast=%v", i, slowSum.Value(i), fastSum.Value(i))
		}
	}
}

func runGroupBySum(f *Frame) (*Frame, error) {
	gb, err := f.GroupBy("region", "id")
	if err != nil {
		return nil, err
	}
	return gb.Agg(Aggregation{Column: "v", Kind: AggSum})
}

// TestGroupByAligned_FastPathDetection covers the applicability
// helper — mirrors the shape of TestOver_FastPath* tests.
func TestGroupByAligned_FastPathDetection(t *testing.T) {
	keys := []string{"region", "id"}
	// aligned + sorted + enforced → fires.
	if !groupByFastPathApplicable(alignedGroupByMeta(), keys) {
		t.Error("aligned + sorted + enforced should activate the fast path")
	}
	// SortEnforced = false → refused (hint-only sort could lie).
	m := alignedGroupByMeta()
	m.SortEnforced = false
	if groupByFastPathApplicable(m, keys) {
		t.Error("hint-only sort must not activate the fast path")
	}
	// Sort starts with a non-group column → refused (group rows may
	// not be contiguous).
	m = &PartitionMetadata{
		Columns:      keys,
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "v"}, {Column: "region"}}, // wrong prefix
		SortEnforced: true,
	}
	if groupByFastPathApplicable(m, keys) {
		t.Error("sort on non-group column must not activate the fast path")
	}
	// Nil metadata → refused.
	if groupByFastPathApplicable(nil, keys) {
		t.Error("nil metadata must not activate the fast path")
	}
	// Partition column mismatch → refused.
	m = &PartitionMetadata{
		Columns:      []string{"user_id"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "user_id"}},
		SortEnforced: true,
	}
	if groupByFastPathApplicable(m, keys) {
		t.Error("partition-column mismatch must not activate the fast path")
	}
	// Partition-column ordering reversed → refused (Aligned requires
	// ordered equality — hash(region, id) ≠ hash(id, region)).
	m = &PartitionMetadata{
		Columns:      []string{"id", "region"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "id"}, {Column: "region"}},
		SortEnforced: true,
	}
	if groupByFastPathApplicable(m, keys) {
		t.Error("partition-column reordering must not activate the fast path")
	}
}

// TestGroupByAligned_MixedAggregations exercises the fast path with
// multiple aggregation kinds in a single call — Sum, Mean, Count.
// Confirms buildAggBuilders picks the right output type for each.
func TestGroupByAligned_MixedAggregations(t *testing.T) {
	// 2 regions × 3 ids × 10 rows = 60 rows, 6 unique (region, id)
	// groups.
	f := sortedGroupByFrame(t, 2, 3, 10)
	f.WithPartitionMeta(alignedGroupByMeta())

	gb, err := f.GroupBy("region", "id")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Column: "v", Kind: AggSum},
		Aggregation{Column: "v", Kind: AggMean},
		Aggregation{Column: "v", Kind: AggCount},
	)
	if err != nil {
		t.Fatalf("Agg with mixed kinds: %v", err)
	}
	// Output = 2 key cols + 3 aggs = 5 cols; 6 unique groups.
	if r, c := out.Shape(); r != 6 || c != 5 {
		t.Fatalf("shape = (%d, %d), want (6, 5) — 2 keys + 3 aggs", r, c)
	}
	names := out.ColumnNames()
	want := []string{"region", "id", "v_sum", "v_mean", "v_count"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("col %d = %q, want %q", i, names[i], n)
		}
	}
	// Each group has 10 rows.
	countArr := out.series[4].col.Data().Chunks()[0].(*array.Int64)
	for i := 0; i < out.NumRows(); i++ {
		if countArr.Value(i) != 10 {
			t.Errorf("group %d count = %d, want 10", i, countArr.Value(i))
		}
	}
}

// TestGroupByAligned_EmptyFrame confirms the fast path handles a
// zero-row input gracefully — assembleOutput must produce a valid
// zero-row Frame with the right schema.
func TestGroupByAligned_EmptyFrame(t *testing.T) {
	f := sortedGroupByFrame(t, 0, 0, 0)
	f.WithPartitionMeta(alignedGroupByMeta())
	gb, err := f.GroupBy("region", "id")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{Column: "v", Kind: AggSum})
	if err != nil {
		t.Fatalf("aggAligned on empty frame: %v", err)
	}
	// 2 key cols + 1 agg = 3 cols.
	if r, c := out.Shape(); r != 0 || c != 3 {
		t.Errorf("shape = (%d, %d), want (0, 3)", r, c)
	}
}

// BenchmarkGroupByAligned measures the aligned+sorted fast path
// against the general hash-map path. Uses a two-column key so aggFast
// (single-primitive-key hot path) doesn't short-circuit — the shape
// that actually exercises aggAligned. 10 regions × 100 ids × 100 rows
// = 100k rows / 1000 unique groups.
func BenchmarkGroupByAligned(b *testing.B) {
	f := sortedGroupByFrame(b, 10, 100, 100)
	f.WithPartitionMeta(alignedGroupByMeta())

	for b.Loop() {
		gb, err := f.GroupBy("region", "id")
		if err != nil {
			b.Fatal(err)
		}
		out, err := gb.Agg(Aggregation{Column: "v", Kind: AggSum})
		if err != nil {
			b.Fatal(err)
		}
		_ = out.NumRows()
	}
}

// BenchmarkGroupByUnaligned is the paired baseline — same fixture
// but no metadata attached, so the general hash-map path runs.
// sortedGroupByFrameWithTimestamp builds an aligned+sorted 2-key
// (region, id) fixture with a Timestamp column added. Used to
// exercise Min/Max on Timestamp under the aligned fast path —
// tryEmitContiguousSIMD refuses (Timestamp outputs via
// TimestampBuilder, not Float64Builder), so the general path must
// still emit. Guards the "silent drop" bug where the general-path
// second loop skipped kind-eligible aggs even when SIMD had
// refused them.
func sortedGroupByFrameWithTimestamp(t testing.TB, nRegions, nIDs, rowsPerGroup int) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	rb := array.NewStringBuilder(pool)
	defer rb.Release()
	ib := array.NewStringBuilder(pool)
	defer ib.Release()
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond, TimeZone: "UTC"}
	tsB := array.NewTimestampBuilder(pool, tsType)
	defer tsB.Release()

	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	for reg := range nRegions {
		rn := fmt.Sprintf("r%03d", reg)
		for id := range nIDs {
			in := fmt.Sprintf("id%06d", id)
			for r := range rowsPerGroup {
				rb.Append(rn)
				ib.Append(in)
				tsB.Append(arrow.Timestamp(base.Add(time.Duration(reg*nIDs*rowsPerGroup+id*rowsPerGroup+r) * time.Second).UnixNano()))
			}
		}
	}
	rArr := rb.NewArray()
	defer rArr.Release()
	iArr := ib.NewArray()
	defer iArr.Release()
	tsArr := tsB.NewArray()
	defer tsArr.Release()

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "id", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "ts", Type: tsType, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(rArr.DataType(), []arrow.Array{rArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(iArr.DataType(), []arrow.Array{iArr})),
		*arrow.NewColumn(fields[2], arrow.NewChunked(tsArr.DataType(), []arrow.Array{tsArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestGroupByAligned_TimestampMinMax_NotSilentlyDropped is a
// regression test for the bug caught in code review: the aligned
// GroupBy's second loop used to skip aggs whose kind was
// SIMD-eligible (Sum/Min/Max/Mean) even when the SIMD attempt
// refused (e.g., because the output builder was TimestampBuilder,
// not Float64Builder). Result: Min/Max on Timestamp under the
// aligned path silently produced no output for those aggs,
// leaving per-column length mismatches (or worse — misaligned
// data across groups).
func TestGroupByAligned_TimestampMinMax_NotSilentlyDropped(t *testing.T) {
	f := sortedGroupByFrameWithTimestamp(t, 3, 4, 10)
	defer f.Release()
	f.WithPartitionMeta(alignedGroupByMeta())

	gb, err := f.GroupBy("region", "id")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Column: "ts", Kind: AggMin, Alias: "first_ts"},
		Aggregation{Column: "ts", Kind: AggMax, Alias: "last_ts"},
	)
	if err != nil {
		t.Fatalf("Agg: %v", err)
	}
	defer out.Release()

	if got := out.NumRows(); got != 12 {
		t.Fatalf("row count = %d, want 12 groups", got)
	}
	// Every output column must have length 12 — the silent-drop
	// bug would leave first_ts / last_ts short.
	for _, name := range []string{"region", "id", "first_ts", "last_ts"} {
		col, err := out.Column(name)
		if err != nil {
			t.Fatalf("Column(%q): %v", name, err)
		}
		if col.Len() != 12 {
			t.Errorf("column %q length = %d, want 12", name, col.Len())
		}
	}
	// Timestamp min/max preserved the source type (not widened to
	// Float64) — regression check on the buildAggBuilders path.
	fields, _ := out.Schema().FieldsByName("first_ts")
	if _, isTS := fields[0].Type.(*arrow.TimestampType); !isTS {
		t.Errorf("first_ts type = %s, want TimestampType (silently misrouted?)", fields[0].Type)
	}
}

// TestGroupByAligned_SumWithNulls_NotSilentlyDropped is the second
// half of the silent-drop regression coverage: Sum on a Float64
// column that carries nulls. tryEmitContiguousSIMD refuses on
// NullN() != 0 (SIMD reduce doesn't propagate nulls the same way
// the general Welford loop does), so the general path must emit.
func TestGroupByAligned_SumWithNulls_NotSilentlyDropped(t *testing.T) {
	pool := memory.DefaultAllocator
	rb := array.NewStringBuilder(pool)
	defer rb.Release()
	ib := array.NewStringBuilder(pool)
	defer ib.Release()
	vb := array.NewFloat64Builder(pool)
	defer vb.Release()

	// 2 regions × 2 ids × 3 rows/group = 12 rows. One null per
	// group so NullN() > 0 on the source column.
	for reg := 0; reg < 2; reg++ {
		rn := fmt.Sprintf("r%03d", reg)
		for id := 0; id < 2; id++ {
			in := fmt.Sprintf("id%06d", id)
			rb.Append(rn)
			ib.Append(in)
			vb.Append(1.0)
			rb.Append(rn)
			ib.Append(in)
			vb.Append(2.0)
			rb.Append(rn)
			ib.Append(in)
			vb.AppendNull()
		}
	}
	rArr := rb.NewArray()
	defer rArr.Release()
	iArr := ib.NewArray()
	defer iArr.Release()
	vArr := vb.NewArray()
	defer vArr.Release()

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "id", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(rArr.DataType(), []arrow.Array{rArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(iArr.DataType(), []arrow.Array{iArr})),
		*arrow.NewColumn(fields[2], arrow.NewChunked(vArr.DataType(), []arrow.Array{vArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	f.WithPartitionMeta(alignedGroupByMeta())

	gb, err := f.GroupBy("region", "id")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{Column: "v", Kind: AggSum, Alias: "s"})
	if err != nil {
		t.Fatalf("Agg: %v", err)
	}
	defer out.Release()

	if got := out.NumRows(); got != 4 {
		t.Fatalf("row count = %d, want 4 groups", got)
	}
	sumCol, err := out.Column("s")
	if err != nil {
		t.Fatalf("Column(s): %v", err)
	}
	if sumCol.Len() != 4 {
		t.Errorf("sum column length = %d, want 4 (silent-drop regression would leave it short)", sumCol.Len())
	}
	sumArr := sumCol.Column().Data().Chunks()[0].(*array.Float64)
	for i := 0; i < sumArr.Len(); i++ {
		if sumArr.IsNull(i) {
			t.Errorf("group %d Sum is null, want 3.0 (nulls should skip, not propagate)", i)
			continue
		}
		if sumArr.Value(i) != 3.0 {
			t.Errorf("group %d Sum = %v, want 3.0 (1+2, nulls skipped)", i, sumArr.Value(i))
		}
	}
}

func BenchmarkGroupByUnaligned(b *testing.B) {
	f := sortedGroupByFrame(b, 10, 100, 100)
	// Deliberately no WithPartitionMeta call — hash-map path runs.

	for b.Loop() {
		gb, err := f.GroupBy("region", "id")
		if err != nil {
			b.Fatal(err)
		}
		out, err := gb.Agg(Aggregation{Column: "v", Kind: AggSum})
		if err != nil {
			b.Fatal(err)
		}
		_ = out.NumRows()
	}
}

package gobi

import (
	"fmt"
	"testing"

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

	for reg := 0; reg < nRegions; reg++ {
		rn := fmt.Sprintf("r%03d", reg)
		for id := 0; id < nIDs; id++ {
			in := fmt.Sprintf("id%06d", id)
			for r := 0; r < rowsPerGroup; r++ {
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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
func BenchmarkGroupByUnaligned(b *testing.B) {
	f := sortedGroupByFrame(b, 10, 100, 100)
	// Deliberately no WithPartitionMeta call — hash-map path runs.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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

package gobi

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// sortedJoinFrame builds a (key, val) frame with rows already sorted
// by key. Feeds the sort-merge parity + benchmark tests as the shape
// athenaio's Iceberg CTAS produces.
func sortedJoinFrame(t testing.TB, nRows int, valColName string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	keyB := array.NewInt64Builder(pool)
	defer keyB.Release()
	valB := array.NewFloat64Builder(pool)
	defer valB.Release()

	// Each key appears twice (2 rows per key → tests the cross-product
	// path). Keys ascending so rows are contiguous by key.
	for i := 0; i < nRows; i++ {
		keyB.Append(int64(i / 2))
		valB.Append(float64(i) * 1.5)
	}
	keyArr := keyB.NewArray()
	defer keyArr.Release()
	valArr := valB.NewArray()
	defer valArr.Release()

	fields := []arrow.Field{
		{Name: "key", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: valColName, Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(keyArr.DataType(), []arrow.Array{keyArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(valArr.DataType(), []arrow.Array{valArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// alignedIcebergMeta constructs the PartitionMetadata claim
// canMergeJoin requires: hash-bucketed on the join key, sorted on
// the join key, SortEnforced=true. Matches what athenaio emits from
// an Iceberg CTAS.
func alignedIcebergMeta() *PartitionMetadata {
	return &PartitionMetadata{
		Columns:      []string{"key"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "key"}},
		SortEnforced: true,
	}
}

// TestMergeJoin_MatchesHashJoin is the correctness oracle for
// step 7: the sort-merge fast path must produce identical rows to
// the streaming hash join on the same inputs (up to row order).
func TestMergeJoin_MatchesHashJoin(t *testing.T) {
	left := sortedJoinFrame(t, 20, "l_val")
	right := sortedJoinFrame(t, 20, "r_val")

	// Baseline: hash join via LazyFrame.Join → no metadata → falls
	// through to streamingJoinExec.
	baseline, err := left.Lazy().Join(right.Lazy(), "key", "key", JoinInner).Collect()
	if err != nil {
		t.Fatalf("hash join baseline: %v", err)
	}

	// Fast path: attach aligned+sorted metadata via assertions,
	// forcing Compile to pick sortMergeJoinExec.
	lLazy, err := left.Lazy().WithPartitionAssertion(alignedIcebergMeta())
	if err != nil {
		t.Fatal(err)
	}
	rLazy, err := right.Lazy().WithPartitionAssertion(alignedIcebergMeta())
	if err != nil {
		t.Fatal(err)
	}
	fast, err := lLazy.Join(rLazy, "key", "key", JoinInner).Collect()
	if err != nil {
		t.Fatalf("sort-merge join: %v", err)
	}

	// Row counts must match. Cross-product with 10 keys × 2 left ×
	// 2 right = 40 output rows.
	if baseline.NumRows() != fast.NumRows() {
		t.Fatalf("row-count divergence: hash=%d merge=%d",
			baseline.NumRows(), fast.NumRows())
	}
	if baseline.NumRows() != 40 {
		t.Fatalf("baseline row count = %d, want 40 (10 keys × 2×2 cross-product)",
			baseline.NumRows())
	}

	// Schemas must match — buildTwoSidedOutput is shared, so column
	// order should be identical.
	if baseline.NumCols() != fast.NumCols() {
		t.Fatalf("col-count divergence: hash=%d merge=%d",
			baseline.NumCols(), fast.NumCols())
	}

	// Content-equality up to row order. Serialize each row to a
	// string, sort both, compare — the join has a defined output
	// order (per the hash path) but sort-merge may produce a
	// different order; we only care that the SET of output rows
	// matches.
	baselineRows := frameRowsAsStrings(t, baseline)
	fastRows := frameRowsAsStrings(t, fast)
	sort.Strings(baselineRows)
	sort.Strings(fastRows)
	for i, br := range baselineRows {
		if br != fastRows[i] {
			t.Fatalf("row %d divergence:\n hash:  %s\n merge: %s", i, br, fastRows[i])
		}
	}
}

// frameRowsAsStrings serializes a Frame's rows to strings for set-
// equality comparisons. Format: "col1_val|col2_val|...".
func frameRowsAsStrings(t *testing.T, f *Frame) []string {
	t.Helper()
	rows := make([]string, f.NumRows())
	names := f.ColumnNames()
	series := make([]Series, len(names))
	for i, n := range names {
		s, err := f.Column(n)
		if err != nil {
			t.Fatal(err)
		}
		series[i] = s
	}
	for row := 0; row < f.NumRows(); row++ {
		var parts []string
		for _, s := range series {
			v, err := readScalarAt(s, row)
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, fmt.Sprintf("%v", v))
		}
		rows[row] = joinStrings(parts, "|")
	}
	return rows
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	total += len(sep) * (len(parts) - 1)
	out := make([]byte, 0, total)
	for i, p := range parts {
		if i > 0 {
			out = append(out, sep...)
		}
		out = append(out, p...)
	}
	return string(out)
}

// TestMergeJoin_CompileTimeSelection confirms canMergeJoin gates
// the fast-path pick correctly: aligned+sorted+enforced picks
// sortMergeJoinExec; missing any of those falls through to
// streamingJoinExec.
func TestMergeJoin_CompileTimeSelection(t *testing.T) {
	left := sortedJoinFrame(t, 4, "l_val")
	right := sortedJoinFrame(t, 4, "r_val")

	pick := func(t *testing.T, lMeta, rMeta *PartitionMetadata, wantMerge bool) {
		t.Helper()
		lLazy := left.Lazy()
		rLazy := right.Lazy()
		if lMeta != nil {
			asserted, err := lLazy.WithPartitionAssertion(lMeta)
			if err != nil {
				t.Fatal(err)
			}
			lLazy = asserted
		}
		if rMeta != nil {
			asserted, err := rLazy.WithPartitionAssertion(rMeta)
			if err != nil {
				t.Fatal(err)
			}
			rLazy = asserted
		}
		plan := lLazy.Join(rLazy, "key", "key", JoinInner).Plan()
		op, err := Compile(Optimize(plan))
		if err != nil {
			t.Fatal(err)
		}
		defer op.Close()

		_, isMerge := op.(*sortMergeJoinExec)
		if isMerge != wantMerge {
			t.Errorf("wantMerge=%v, got sortMergeJoinExec=%v (%T)", wantMerge, isMerge, op)
		}
	}

	// Aligned + sorted + enforced on both sides: fast path.
	t.Run("aligned+sorted+enforced fires", func(t *testing.T) {
		pick(t, alignedIcebergMeta(), alignedIcebergMeta(), true)
	})

	// No metadata on either side: hash path.
	t.Run("no metadata falls through", func(t *testing.T) {
		pick(t, nil, nil, false)
	})

	// Left metadata only: hash path (both sides must claim).
	t.Run("one-sided metadata falls through", func(t *testing.T) {
		pick(t, alignedIcebergMeta(), nil, false)
	})

	// SortEnforced=false: hash path (hint-only can lie about order).
	t.Run("hint-only sort falls through", func(t *testing.T) {
		hintOnly := alignedIcebergMeta()
		hintOnly.SortEnforced = false
		pick(t, hintOnly, hintOnly, false)
	})

	// Different HashFn on the two sides: hash path (AlignedWith
	// refuses cross-tag matches).
	t.Run("cross-hash claims fall through", func(t *testing.T) {
		altHash := alignedIcebergMeta()
		altHash.HashFn = "athenaio/hive/bucket/v1"
		pick(t, alignedIcebergMeta(), altHash, false)
	})
}

// unalignedJoinFrame builds an unsorted (shuffled) frame to force
// the hash join path in the benchmark. Rows are still same schema
// as sortedJoinFrame — only the input row order differs.
func unalignedJoinFrame(t testing.TB, nRows int, valColName string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	keyB := array.NewInt64Builder(pool)
	defer keyB.Release()
	valB := array.NewFloat64Builder(pool)
	defer valB.Release()

	// Same (key, val) set as sortedJoinFrame but with keys arranged
	// so no valid alignment claim would hold. Simple bit-reverse-ish
	// shuffle: each key at position i × prime mod nRows.
	prime := int64(7919)
	for i := 0; i < nRows; i++ {
		pos := (int64(i) * prime) % int64(nRows)
		keyB.Append(pos / 2)
		valB.Append(float64(pos) * 1.5)
	}
	keyArr := keyB.NewArray()
	defer keyArr.Release()
	valArr := valB.NewArray()
	defer valArr.Release()

	fields := []arrow.Field{
		{Name: "key", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: valColName, Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(keyArr.DataType(), []arrow.Array{keyArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(valArr.DataType(), []arrow.Array{valArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// BenchmarkJoin_HashUnaligned measures the current streaming hash
// join on unaligned inputs — the baseline the sort-merge fast path
// is compared against. 10k × 10k Inner join over Int64 keys with
// ~2× duplicates per key on each side.
func BenchmarkJoin_HashUnaligned(b *testing.B) {
	left := unalignedJoinFrame(b, 10_000, "l_val")
	right := unalignedJoinFrame(b, 10_000, "r_val")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lf := left.Lazy().Join(right.Lazy(), "key", "key", JoinInner)
		out, err := lf.Collect()
		if err != nil {
			b.Fatal(err)
		}
		_ = out.NumRows()
	}
}

// BenchmarkJoin_MergeAligned measures the sort-merge fast path on
// aligned+sorted inputs. Same shape as BenchmarkJoin_HashUnaligned;
// only difference is the WithPartitionAssertion + sorted input
// order that activates canMergeJoin.
func BenchmarkJoin_MergeAligned(b *testing.B) {
	left := sortedJoinFrame(b, 10_000, "l_val")
	right := sortedJoinFrame(b, 10_000, "r_val")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lLazy, _ := left.Lazy().WithPartitionAssertion(alignedIcebergMeta())
		rLazy, _ := right.Lazy().WithPartitionAssertion(alignedIcebergMeta())
		lf := lLazy.Join(rLazy, "key", "key", JoinInner)
		out, err := lf.Collect()
		if err != nil {
			b.Fatal(err)
		}
		_ = out.NumRows()
	}
}

// BenchmarkJoin_HashMultiProbeBatch measures the streaming hash join
// on a probe side that spans multiple batches. defaultBatchRows is
// 64k, so a 200k-row probe splits into 4 batches — each of which used
// to rebuild the right-side hash index before the buildIfNeeded
// caching landed. A smaller right side keeps the total time reasonable
// while still making the per-batch rebuild dominant relative to
// per-row probe work.
//
// Unaligned inputs deliberately used so the sort-merge fast path
// stays out — this measures the hash path in isolation.
func BenchmarkJoin_HashMultiProbeBatch(b *testing.B) {
	left := unalignedJoinFrame(b, 200_000, "l_val")
	right := unalignedJoinFrame(b, 100_000, "r_val")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lf := left.Lazy().Join(right.Lazy(), "key", "key", JoinInner)
		out, err := lf.Collect()
		if err != nil {
			b.Fatal(err)
		}
		_ = out.NumRows()
	}
}

var _ = context.Background // silence unused-import if refactored later

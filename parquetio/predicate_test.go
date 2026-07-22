package parquetio_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// buildMultiRowGroupFixture writes a parquet file with n rows split
// into rowGroupRows-sized groups. The id column is monotonic 0..n-1,
// so each row-group's [min_id, max_id] describes an obvious interval
// used by the predicate-pushdown tests.
//
//	rowGroupRows=100 + n=400 →  4 row-groups: [0..99] [100..199] [200..299] [300..399]
func buildMultiRowGroupFixture(t *testing.T, n, rowGroupRows int, name string) string {
	t.Helper()
	pool := memory.DefaultAllocator
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	valueB := array.NewFloat64Builder(pool)
	defer valueB.Release()
	for i := range n {
		idB.Append(int64(i))
		valueB.Append(float64(i) * 0.5)
	}
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), valueB.NewArray()}
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
	df, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: int64(rowGroupRows),
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPredicate_ReadFileSkipsRowGroups(t *testing.T) {
	// 400 rows in 4 row-groups. Predicate id > 250 can only match
	// rows in the last two row-groups (ids 250..299 and 300..399);
	// row-groups 0 (ids 0..99) and 1 (100..199) should be skipped.
	path := buildMultiRowGroupFixture(t, 400, 100, "multi_rg.parquet")

	// With predicate: only 200 rows should reach the reader.
	// (The row-level Filter that would normally apply is not part
	// of this test — we're validating the raw skipping.)
	pred := gobi.Col("id").Gt(gobi.Lit(int64(250)))
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Predicate: pred})
	if err != nil {
		t.Fatal(err)
	}
	// Row-groups 2 and 3 survive (200 rows). Row-group 2 still contains
	// ids 200..249 which don't match the predicate; the reader-level
	// skip is coarse (whole rowgroup or not), so those come through
	// and would be dropped by a downstream Filter. We assert on the
	// coarse skip behavior here.
	if out.NumRows() != 200 {
		t.Fatalf("read rows = %d, want 200 (row-groups 2 and 3)", out.NumRows())
	}
	// First surviving row is id=200 (start of row-group 2).
	ids, _ := out.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != 200 {
		t.Fatalf("first surviving id = %d, want 200", arr.Value(0))
	}
}

func TestPredicate_ReadFileNoPruningWithoutPredicate(t *testing.T) {
	// Sanity: without a predicate, every row is read.
	path := buildMultiRowGroupFixture(t, 400, 100, "no_pred.parquet")
	out, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 400 {
		t.Fatalf("read rows = %d, want 400", out.NumRows())
	}
}

func TestPredicate_ReadFileNoPruningWithNonPruningPredicate(t *testing.T) {
	// Predicate that every row-group's stats agree with — no
	// pruning possible, all 400 rows come through.
	path := buildMultiRowGroupFixture(t, 400, 100, "no_prune.parquet")
	pred := gobi.Col("id").Ge(gobi.Lit(int64(0)))
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Predicate: pred})
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 400 {
		t.Fatalf("read rows = %d, want 400 (no pruning possible)", out.NumRows())
	}
}

func TestScanFile_PredicatePushdown_AppliedByOptimizer(t *testing.T) {
	// End-to-end: Filter above ScanFile should get pushed into the
	// scan (Explain shows "pred="), and the resulting scan skips
	// row-groups the predicate can't satisfy. The row-level Filter
	// above stays and finalizes correctness.
	path := buildMultiRowGroupFixture(t, 400, 100, "scan_pushdown.parquet")

	lf := parquetio.ScanFile(path, nil).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(250))))

	// Explain should reflect the pushed predicate.
	explain := lf.ExplainOptimized()
	if !strings.Contains(explain, "pred=") {
		t.Fatalf("optimizer didn't push predicate into scan:\n%s", explain)
	}

	// Result correctness: 149 rows survive (ids 251..399).
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 149 {
		t.Fatalf("result rows = %d, want 149", out.NumRows())
	}
}

func TestScanFile_PredicatePushdown_CombinedFilters(t *testing.T) {
	// Two adjacent Filters get combined by CombineFilters, then the
	// merged predicate is pushed into the scan. Verifies rule
	// composition through the fixed-point loop.
	path := buildMultiRowGroupFixture(t, 400, 100, "scan_combined.parquet")

	lf := parquetio.ScanFile(path, nil).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(50)))).
		Filter(gobi.Col("id").Lt(gobi.Lit(int64(150))))

	explain := lf.ExplainOptimized()
	if !strings.Contains(explain, "pred=") {
		t.Fatalf("no predicate on scan:\n%s", explain)
	}
	// Filter above should now be single (combined).
	if strings.Count(explain, "Filter(") != 1 {
		t.Fatalf("expected exactly 1 Filter in optimized plan:\n%s", explain)
	}

	// Result: ids 51..149 → 99 rows.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 99 {
		t.Fatalf("rows = %d, want 99", out.NumRows())
	}
}

func TestScanFile_PredicatePushdown_PrunesAllRowGroups(t *testing.T) {
	// A predicate no row-group can satisfy should prune everything,
	// and Collect should return an empty (but well-formed) Frame.
	path := buildMultiRowGroupFixture(t, 400, 100, "scan_all_pruned.parquet")

	lf := parquetio.ScanFile(path, nil).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(10_000))))

	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("rows = %d, want 0 (all row-groups pruned)", out.NumRows())
	}
	// Schema preserved.
	if out.NumCols() != 2 {
		t.Fatalf("cols = %d, want 2", out.NumCols())
	}
}

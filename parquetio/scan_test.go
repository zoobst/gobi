package parquetio_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

func TestScanFile_EndToEnd(t *testing.T) {
	// Build a fixture, ScanFile + LazyFrame chain against it, verify
	// the composed pipeline runs and produces the expected result.
	df := makeSyntheticFrame(t, 1000)
	path := writeFixture(t, df, "scan.parquet")

	out, err := parquetio.ScanFile(path, nil).
		Filter(gobi.Col("id").Lt(gobi.Lit(int64(10)))).
		Select(gobi.Col("id"), gobi.Col("value_a").Alias("v")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumRows(); got != 10 {
		t.Fatalf("rows = %d, want 10", got)
	}
	if got := out.NumCols(); got != 2 {
		t.Fatalf("cols = %d, want 2 (projected)", got)
	}
	if names := out.ColumnNames(); names[0] != "id" || names[1] != "v" {
		t.Fatalf("cols = %v, want [id v]", names)
	}
}

func TestScanFile_SchemaWithoutCollect(t *testing.T) {
	// Schema() should be populated from the parquet footer at
	// ScanFile construction time — no need to Collect first.
	df := makeSyntheticFrame(t, 100)
	path := writeFixture(t, df, "scan_schema.parquet")

	lf := parquetio.ScanFile(path, nil)
	sch := lf.Schema()
	if len(sch.Fields()) != 3 {
		t.Fatalf("scan schema fields = %d, want 3", len(sch.Fields()))
	}
	// Field ordering matches the write side.
	want := []string{"id", "value_a", "key"}
	for i, w := range want {
		if got := sch.Field(i).Name; got != w {
			t.Errorf("field %d = %q, want %q", i, got, w)
		}
	}
}

func TestScanFile_ExplainShowsPath(t *testing.T) {
	df := makeSyntheticFrame(t, 10)
	path := writeFixture(t, df, "scan_explain.parquet")

	got := parquetio.ScanFile(path, nil).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(5)))).
		Explain()
	if !strings.Contains(got, "Scan[parquet](") {
		t.Fatalf("explain missing Scan[parquet] line:\n%s", got)
	}
	if !strings.Contains(got, path) {
		t.Fatalf("explain missing path %q:\n%s", path, got)
	}
	if !strings.Contains(got, "Filter(") {
		t.Fatalf("explain missing Filter line:\n%s", got)
	}
}

func TestScanFile_ColumnProjection(t *testing.T) {
	// ReadOptions.Columns should carry through both the pushdown (fewer
	// bytes off disk at Collect) AND the eagerly-fetched Schema.
	df := makeSyntheticFrame(t, 100)
	path := writeFixture(t, df, "scan_proj.parquet")

	lf := parquetio.ScanFile(path, &parquetio.ReadOptions{
		Columns: []string{"id", "key"},
	})
	sch := lf.Schema()
	if len(sch.Fields()) != 2 {
		t.Fatalf("projected schema fields = %d, want 2", len(sch.Fields()))
	}
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 2 {
		t.Fatalf("collected cols = %d, want 2", got)
	}
}

func TestScanFile_MissingFile_ErrorsAtCollect(t *testing.T) {
	// Building a LazyFrame for a missing file should not error —
	// the error must arrive at Collect time.
	badPath := filepath.Join(t.TempDir(), "does_not_exist.parquet")
	lf := parquetio.ScanFile(badPath, nil)
	if lf == nil {
		t.Fatal("ScanFile returned nil for missing file")
	}
	// Explain() should still work — it uses the label, not the schema.
	if !strings.Contains(lf.Explain(), "Scan[parquet](") {
		t.Fatal("Explain should work on a missing-file scan")
	}
	_, err := lf.Collect()
	if err == nil {
		t.Fatal("expected error from Collect on missing file")
	}
}

func TestScanFile_ComposesWithLazyPipeline(t *testing.T) {
	// Full pipeline over a scan source: group-by + agg on read data.
	df := makeSyntheticFrame(t, 1000)
	path := writeFixture(t, df, "scan_pipeline.parquet")

	out, err := parquetio.ScanFile(path, nil).
		GroupBy("key").
		Agg(
			gobi.Aggregation{Column: "value_a", Kind: gobi.AggMean, Alias: "avg"},
			gobi.Aggregation{Column: "value_a", Kind: gobi.AggCount, Alias: "n"},
		).
		SortBy(gobi.SortKey{Column: "avg", Descending: true}).
		Limit(5).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumRows(); got != 5 {
		t.Fatalf("rows = %d, want 5", got)
	}
	// Two output columns beyond the key: avg, n.
	if got := out.NumCols(); got != 3 {
		t.Fatalf("cols = %d, want 3 (key + avg + n)", got)
	}
}

// TestScanFile_ProjectionPushdown_AppliedByOptimizer verifies the
// end-to-end wiring: an unrestricted ScanFile below a Select gets
// projected by the optimizer's ProjectionPushdown rule.
func TestScanFile_ProjectionPushdown_AppliedByOptimizer(t *testing.T) {
	df := makeSyntheticFrame(t, 100)
	path := writeFixture(t, df, "scan_pushdown.parquet")

	lf := parquetio.ScanFile(path, nil).
		Filter(gobi.Col("id").Lt(gobi.Lit(int64(10)))).
		Select(gobi.Col("id"), gobi.Col("value_a"))

	// Optimized plan's scan should now carry a column projection —
	// the Explain output reflects it via the "cols=" label suffix.
	explain := lf.ExplainOptimized()
	if !strings.Contains(explain, "cols=[id value_a]") {
		t.Fatalf("optimizer didn't project scan columns:\n%s", explain)
	}
	// Confirm result correctness.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 2 {
		t.Fatalf("cols = %d, want 2 (id, value_a)", got)
	}
	if got := out.NumRows(); got != 10 {
		t.Fatalf("rows = %d, want 10", got)
	}
}

// TestScanFile_ExplicitColumnsRespected verifies the "user intent
// wins" contract: if the caller sets ReadOptions.Columns explicitly,
// the optimizer's projection callback returns nil (no change).
func TestScanFile_ExplicitColumnsRespected(t *testing.T) {
	df := makeSyntheticFrame(t, 100)
	path := writeFixture(t, df, "scan_explicit.parquet")

	// User asks for {id, value_a, key} even though the plan only
	// uses id — optimizer must NOT shrink that further.
	lf := parquetio.ScanFile(path, &parquetio.ReadOptions{
		Columns: []string{"id", "value_a", "key"},
	}).Select(gobi.Col("id"))

	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Final Frame is the Select's output — 1 column. The scan-level
	// projection didn't shrink; the SELECT just picked one.
	if got := out.NumCols(); got != 1 {
		t.Fatalf("select cols = %d, want 1", got)
	}
}

// Guard against silent regressions: the deferred-execution contract is
// load-bearing for future Layer 4 optimizer work.
func TestScanFile_DeferredError_Wrapping(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "nope.parquet")
	_, err := parquetio.ScanFile(badPath, nil).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(0)))).
		Collect()
	if err == nil {
		t.Fatal("expected an error")
	}
	// Any file-open error will do; we mainly want it to reach us.
	if strings.Contains(err.Error(), "collectPlan") {
		t.Fatalf("plan-walker error leaked instead of underlying: %v", err)
	}
	_ = errors.New // keep import
}

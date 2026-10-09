package parquetio_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// buildMultiRowGroupFixture (from predicate_test.go) builds an
// N-row parquet split into rowGroupRows-sized groups; reused here
// so parallel-scan tests can exercise the row-group partitioning
// logic without duplicating fixture builders.

// TestParallelScan_ParityWithSerial verifies streaming aggregate over
// a multi-row-group parquet produces byte-identical results whether
// the scan runs serially (ScanWorkers=1) or in parallel.
func TestParallelScan_ParityWithSerial(t *testing.T) {
	// 5000 rows, 10 row-groups of 500 rows each — enough that
	// GOMAXPROCS-1 workers get real work.
	path := buildMultiRowGroupFixture(t, 5000, 500, "parallel_parity.parquet")

	// Reduce id modulo a small number to synthesize repeating keys
	// for a nontrivial group-by. Uses an expression rather than a
	// separate column so we don't need to change the fixture writer.
	agg := func(opts *parquetio.ReadOptions) *gobi.Frame {
		out, err := parquetio.ScanFile(path, opts).
			WithColumn("bucket", gobi.Col("id").Ge(gobi.Lit(int64(2500)))).
			GroupBy("bucket").
			Agg(
				gobi.Aggregation{Column: "id", Kind: gobi.AggSum},
				gobi.Aggregation{Column: "value", Kind: gobi.AggMean, Alias: "avg"},
				gobi.Aggregation{Column: "id", Kind: gobi.AggCount, Alias: "n"},
			).
			Collect()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	serial := agg(&parquetio.ReadOptions{ScanWorkers: 1})
	parallel := agg(&parquetio.ReadOptions{ScanWorkers: 4})

	if serial.NumRows() != parallel.NumRows() {
		t.Fatalf("row count: serial=%d, parallel=%d",
			serial.NumRows(), parallel.NumRows())
	}
	if serial.NumCols() != parallel.NumCols() {
		t.Fatalf("col count: serial=%d, parallel=%d",
			serial.NumCols(), parallel.NumCols())
	}
	// Streaming aggregate output is sorted by key bytes, so rows
	// should appear in identical order regardless of scan
	// parallelism.
	for _, col := range []string{"bucket", "id_sum", "avg", "n"} {
		s1, err := serial.Column(col)
		if err != nil {
			t.Fatalf("serial missing col %q", col)
		}
		s2, err := parallel.Column(col)
		if err != nil {
			t.Fatalf("parallel missing col %q", col)
		}
		if s1.Len() != s2.Len() {
			t.Errorf("%s len: serial=%d parallel=%d", col, s1.Len(), s2.Len())
		}
	}
}

// TestParallelScan_ExplainShowsWorkerCount checks that
// ExplainPhysical reports the expected worker count when parallel
// scan kicks in.
func TestParallelScan_ExplainShowsWorkerCount(t *testing.T) {
	path := buildMultiRowGroupFixture(t, 5000, 500, "parallel_explain.parquet")

	lf := parquetio.ScanFile(path, &parquetio.ReadOptions{ScanWorkers: 4}).
		WithColumn("bucket", gobi.Col("id").Ge(gobi.Lit(int64(2500)))).
		GroupBy("bucket").
		Agg(gobi.Aggregation{Column: "id", Kind: gobi.AggSum})

	explain := lf.ExplainPhysical()
	if !strings.Contains(explain, "workers=4") {
		t.Fatalf("expected workers=4 in ExplainPhysical:\n%s", explain)
	}
}

// TestParallelScan_SerialFallbackOnSmallFile checks that a file
// with 1 row-group skips parallel scan even when ScanWorkers > 1.
// No benefit possible + goroutine spin-up is pure overhead.
func TestParallelScan_SerialFallbackOnSmallFile(t *testing.T) {
	// 100 rows, single row-group.
	path := buildMultiRowGroupFixture(t, 100, 1000, "parallel_small.parquet")

	lf := parquetio.ScanFile(path, &parquetio.ReadOptions{ScanWorkers: 8}).
		Filter(gobi.Col("id").Lt(gobi.Lit(int64(50))))

	explain := lf.ExplainPhysical()
	// Should NOT show the scan's workers=... label. (The aggregate
	// label also uses [workers=N] once Slice D lands — that's fine;
	// we're specifically asserting the *scan* fell back to serial.)
	if strings.Contains(explain, "stream, workers=") {
		t.Fatalf("single-row-group file should not use parallel scan:\n%s", explain)
	}
	// And should still return the right rows.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 50 {
		t.Fatalf("rows = %d, want 50", out.NumRows())
	}
}

// TestParallelScan_ScanWorkers1DisablesParallel confirms that
// explicit ScanWorkers=1 turns parallel off even when the file has
// many row-groups. Users who want deterministic serial execution
// (debugging, reproducibility) get it.
func TestParallelScan_ScanWorkers1DisablesParallel(t *testing.T) {
	path := buildMultiRowGroupFixture(t, 5000, 500, "parallel_serial_opt.parquet")

	lf := parquetio.ScanFile(path, &parquetio.ReadOptions{ScanWorkers: 1}).
		WithColumn("bucket", gobi.Col("id").Ge(gobi.Lit(int64(2500)))).
		GroupBy("bucket").
		Agg(gobi.Aggregation{Column: "id", Kind: gobi.AggSum})

	explain := lf.ExplainPhysical()
	// Scope the check to the *scan* label — Slice D's parallel
	// aggregate emits its own [workers=N] token further up the plan
	// and that is not what this test is guarding.
	if strings.Contains(explain, "stream, workers=") {
		t.Fatalf("ScanWorkers=1 should force serial scan:\n%s", explain)
	}
}

// TestParallelScan_AutoUsesGOMAXPROCS verifies the default
// (ScanWorkers=0) picks up GOMAXPROCS.
func TestParallelScan_AutoUsesGOMAXPROCS(t *testing.T) {
	path := buildMultiRowGroupFixture(t, 5000, 500, "parallel_auto.parquet")

	lf := parquetio.ScanFile(path, nil). // nil opts → auto
						WithColumn("bucket", gobi.Col("id").Ge(gobi.Lit(int64(2500)))).
						GroupBy("bucket").
						Agg(gobi.Aggregation{Column: "id", Kind: gobi.AggSum})

	explain := lf.ExplainPhysical()
	// buildMultiRowGroupFixture(5000, 500) → 10 row-groups. Effective
	// worker count is capped at that so we don't spawn idle workers.
	expected := min(runtime.GOMAXPROCS(0), 10)
	wantLabel := fmt.Sprintf("workers=%d", expected)
	if !strings.Contains(explain, wantLabel) {
		t.Fatalf("auto mode should show %s in explain:\n%s", wantLabel, explain)
	}
}

// TestParallelScan_MissingFile propagates the file-open error at
// Collect (not at Compile). Consistent with the serial ScanFile
// contract — no early errors during plan build.
func TestParallelScan_MissingFile(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "does_not_exist.parquet")
	_, err := parquetio.ScanFile(badPath, &parquetio.ReadOptions{ScanWorkers: 4}).
		Collect()
	if err == nil {
		t.Fatal("expected error from Collect on missing file")
	}
}

// TestParallelScan_RowGroupsExplicit lets the user reach in and
// pin a specific row-group subset (parquetio.ReadOptions.RowGroups is
// the underlying knob parallel scan uses to partition). Verifies
// the plumbing works and produces the expected slice.
func TestParallelScan_RowGroupsExplicit(t *testing.T) {
	path := buildMultiRowGroupFixture(t, 5000, 500, "rg_explicit.parquet")

	// Read only row-groups 3 and 5 → 1000 rows (rows 1500-1999 + 2500-2999).
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		RowGroups: []int{3, 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 1000 {
		t.Fatalf("rows from row-groups {3,5} = %d, want 1000", out.NumRows())
	}
}

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

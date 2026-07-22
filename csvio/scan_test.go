package csvio_test

import (
	"strings"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
)

// TestScanFile_ReturnsLazyFrame verifies csvio.ScanFile returns a
// LazyFrame that produces the same result as the eager ReadFile
// path when Collect is called. Uses the shared cities fixture.
func TestScanFile_ReturnsLazyFrame(t *testing.T) {
	lf := csvio.ScanFile[city]("../testdata/cities.csv", &csvio.ReadOptions{CRSHint: 4326})
	out, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	rows, cols := out.Shape()
	// The shared cities.csv fixture has 5 data rows.
	if rows != 5 || cols != 3 {
		t.Fatalf("shape = (%d, %d), want (5, 3)", rows, cols)
	}
	names := out.ColumnNames()
	if names[0] != "name" || names[1] != "population" || names[2] != "geometry" {
		t.Fatalf("names = %v, want [name population geometry]", names)
	}
}

// TestScanFile_SelectComposesAboveScan verifies that a Select above
// the ScanFile drops the unpicked columns from the final Frame.
// Since csvio's parser doesn't skip columns (arrow-csv would panic
// if IncludeColumns is mixed with an explicit schema), the parse
// itself still reads everything — but Select above the LazyFrame
// projects at collect time, so users see the columns they asked
// for.
func TestScanFile_SelectComposesAboveScan(t *testing.T) {
	out, err := csvio.ScanFile[city]("../testdata/cities.csv", &csvio.ReadOptions{CRSHint: 4326}).
		Select(gobi.Col("name"), gobi.Col("population")).
		Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	names := out.ColumnNames()
	if len(names) != 2 || names[0] != "name" || names[1] != "population" {
		t.Fatalf("cols = %v, want [name population]", names)
	}
}

// TestScanFile_FilterAboveScan verifies a Frame.Filter above the
// ScanFile applies at collect time — CSV can't skip rows during
// parse (no random access), but the predicate still shapes the
// output correctly. Population > 3M keeps NYC + LA.
func TestScanFile_FilterAboveScan(t *testing.T) {
	out, err := csvio.ScanFile[city]("../testdata/cities.csv", &csvio.ReadOptions{CRSHint: 4326}).
		Filter(gobi.Col("population").Gt(gobi.Lit(int64(3_000_000)))).
		Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 (population > 3M keeps NYC + LA)", out.NumRows())
	}
}

// TestScanFile_ExplainShowsPath makes sure the Explain output
// identifies the scan as a csv scan with the source path in it —
// useful when debugging plan shapes.
func TestScanFile_ExplainShowsPath(t *testing.T) {
	lf := csvio.ScanFile[city]("../testdata/cities.csv", nil).
		Filter(gobi.Col("population").Gt(gobi.Lit(int64(100))))
	explain := lf.ExplainPhysical()
	if !strings.Contains(explain, `Scan[csv]`) || !strings.Contains(explain, `cities.csv`) {
		t.Fatalf("expected Scan[csv](cities.csv) label in explain:\n%s", explain)
	}
}

// TestScanFile_MissingFile propagates the file-open error to Collect
// rather than to the ScanFile constructor. Consistent with parquetio
// and gpkgio.
func TestScanFile_MissingFile(t *testing.T) {
	_, err := csvio.ScanFile[city]("../testdata/does_not_exist.csv", nil).Collect()
	if err == nil {
		t.Fatal("expected error from Collect on missing file")
	}
}

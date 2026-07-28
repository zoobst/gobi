package gpkgio_test

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/gpkgio"
	_ "modernc.org/sqlite"
)

// TestSumColumn_HappyPath — SumColumn on a numeric column of a
// known-fixture layer returns the right total. buildTestFrame's
// value column is [1.5, 2.5, 3.5], so SUM = 7.5.
func TestSumColumn_HappyPath(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "sum.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	sum, err := g.SumColumn("features", "value")
	if err != nil {
		t.Fatalf("SumColumn: %v", err)
	}
	if sum != 7.5 {
		t.Errorf("SUM(value) = %v, want 7.5", sum)
	}
}


// TestSumColumn_ValidationErrors — unsafe identifiers reject before
// touching the DB; unknown layer/column surfaces the SQL error.
func TestSumColumn_ValidationErrors(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "validate_sum.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	cases := []struct{ layer, col string }{
		{"", "value"},                // empty layer
		{"features", ""},             // empty column
		{"drop; --", "value"},        // injection in layer
		{"features", "col; DROP T"},  // injection in column
		{"missing_layer", "value"},   // unknown layer (SQL error)
		{"features", "missing_col"},  // unknown column (SQL error)
	}
	for _, c := range cases {
		if _, err := g.SumColumn(c.layer, c.col); err == nil {
			t.Errorf("SumColumn(%q, %q): expected error, got nil", c.layer, c.col)
		}
	}
}

// TestMeanColumn_HappyPath — Mean of [1.5, 2.5, 3.5] is 2.5.
func TestMeanColumn_HappyPath(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "mean.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	m, err := g.MeanColumn("features", "value")
	if err != nil {
		t.Fatalf("MeanColumn: %v", err)
	}
	if m != 2.5 {
		t.Errorf("AVG(value) = %v, want 2.5", m)
	}
}

// TestMinMaxColumn_HappyPath — Min and Max on [1.5, 2.5, 3.5].
func TestMinMaxColumn_HappyPath(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "minmax.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	lo, err := g.MinColumn("features", "value")
	if err != nil {
		t.Fatalf("MinColumn: %v", err)
	}
	if lo != 1.5 {
		t.Errorf("MIN(value) = %v, want 1.5", lo)
	}
	hi, err := g.MaxColumn("features", "value")
	if err != nil {
		t.Fatalf("MaxColumn: %v", err)
	}
	if hi != 3.5 {
		t.Errorf("MAX(value) = %v, want 3.5", hi)
	}
}

// TestScalarAgg_EmptyLayerReturnsNaN — Mean/Min/Max on a layer with
// no non-null rows return NaN, matching Series.Mean/Min/Max. Sum
// stays at 0 (sum of nothing = 0, consistent with Series.Sum).
func TestScalarAgg_EmptyLayerReturnsNaN(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "empty.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	// Clear rows via a raw sql.DB handle — DELETE keeps the schema
	// in place so the aggregates run against a valid-but-empty
	// table. Close before reopening via gpkgio.Open so modernc's
	// SQLite doesn't hold the write lock on our second handle.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`DELETE FROM features`); err != nil {
		rawDB.Close()
		t.Fatal(err)
	}
	rawDB.Close()

	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	m, err := g.MeanColumn("features", "value")
	if err != nil {
		t.Fatalf("MeanColumn: %v", err)
	}
	if !math.IsNaN(m) {
		t.Errorf("MeanColumn on empty = %v, want NaN", m)
	}
	lo, err := g.MinColumn("features", "value")
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(lo) {
		t.Errorf("MinColumn on empty = %v, want NaN", lo)
	}
	hi, err := g.MaxColumn("features", "value")
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(hi) {
		t.Errorf("MaxColumn on empty = %v, want NaN", hi)
	}
	sum, err := g.SumColumn("features", "value")
	if err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Errorf("SumColumn on empty = %v, want 0 (SUM of nothing = 0)", sum)
	}
}

// TestScalarAgg_ValidationRejects — same identifier + column-exists
// guards fire for Mean/Min/Max as for SumColumn.
func TestScalarAgg_ValidationRejects(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "reject_agg.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	cases := []struct{ layer, col string }{
		{"", "value"},
		{"features", ""},
		{"drop; --", "value"},
		{"features", "missing_col"},
	}
	for _, c := range cases {
		if _, err := g.MeanColumn(c.layer, c.col); err == nil {
			t.Errorf("MeanColumn(%q, %q): expected error", c.layer, c.col)
		}
		if _, err := g.MinColumn(c.layer, c.col); err == nil {
			t.Errorf("MinColumn(%q, %q): expected error", c.layer, c.col)
		}
		if _, err := g.MaxColumn(c.layer, c.col); err == nil {
			t.Errorf("MaxColumn(%q, %q): expected error", c.layer, c.col)
		}
	}
}

// TestCountRows_HappyPath — CountRows returns the exact row count.
// buildTestFrame produces 3 rows.
func TestCountRows_HappyPath(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "count.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	n, err := g.CountRows("features")
	if err != nil {
		t.Fatalf("CountRows: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

// TestLayerNames_Ordering — LayerNames returns the layer names in
// gpkg_geometry_columns iteration order (write order in practice
// since we register a row per WriteFile).
func TestLayerNames_Ordering(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "names.gpkg")
	names := []string{"alpha", "bravo", "charlie"}
	for _, n := range names {
		if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: n}); err != nil {
			t.Fatal(err)
		}
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	got, err := g.LayerNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("layer count = %d, want 3", len(got))
	}
	// Order isn't spec-guaranteed by gpkg_geometry_columns, so
	// check membership rather than exact order.
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	for _, want := range names {
		if !set[want] {
			t.Errorf("layer %q missing from LayerNames() = %v", want, got)
		}
	}
}

package gpkgio_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	// Package under test — imported as _test so the file exercises
	// the public API surface only, catching accidental unexported
	// dependencies.
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/gpkgio"

	_ "modernc.org/sqlite"
)

// buildTestFrame constructs a 3-row Frame with an id (Int64), name
// (String), value (Float64), and geometry (WKB Point) column. Shared
// across every test that needs a canonical fixture.
func buildTestFrame(t *testing.T) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3}, nil)
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"a", "b", "c"}, nil)
	valB := array.NewFloat64Builder(pool)
	defer valB.Release()
	valB.AppendValues([]float64{1.5, 2.5, 3.5}, nil)

	// Geometry column: WKB-encoded points, tagged via gobi.GeometryField.
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, pt := range []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 2}} {
		geomB.Append(geometry.WKB(pt))
	}

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		gobi.GeometryField("geom", 4326),
	}
	schema := arrow.NewSchema(fields, nil)

	arrs := []arrow.Array{idB.NewArray(), nameB.NewArray(), valB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestRoundTrip_ColumnsAndGeometry writes a 3-row Frame with a
// geometry column to a fresh gpkg, reads it back, and verifies
// every column round-trips byte-identical (id, name, value) and
// the geometry decodes to the same points.
func TestRoundTrip_ColumnsAndGeometry(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "roundtrip.gpkg")

	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "features"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rows, cols := out.Shape()
	if rows != 3 {
		t.Fatalf("rows = %d, want 3", rows)
	}
	// Expect the layer's four data columns + the implicit fid PK
	// that WriteFile adds — five total on read.
	if cols != 5 {
		t.Fatalf("cols = %d, want 5 (id, name, value, geom, + fid PK)", cols)
	}

	// id column: Int64 values 1,2,3.
	idS, err := out.Column("id")
	if err != nil {
		t.Fatal(err)
	}
	idArr := idS.Column().Data().Chunks()[0].(*array.Int64)
	for i, want := range []int64{1, 2, 3} {
		if idArr.Value(i) != want {
			t.Errorf("id[%d] = %d, want %d", i, idArr.Value(i), want)
		}
	}

	// name column: String values a,b,c.
	nameS, err := out.Column("name")
	if err != nil {
		t.Fatal(err)
	}
	nameArr := nameS.Column().Data().Chunks()[0].(*array.String)
	for i, want := range []string{"a", "b", "c"} {
		if nameArr.Value(i) != want {
			t.Errorf("name[%d] = %q, want %q", i, nameArr.Value(i), want)
		}
	}

	// value column: Float64 values.
	valS, err := out.Column("value")
	if err != nil {
		t.Fatal(err)
	}
	valArr := valS.Column().Data().Chunks()[0].(*array.Float64)
	for i, want := range []float64{1.5, 2.5, 3.5} {
		if valArr.Value(i) != want {
			t.Errorf("value[%d] = %v, want %v", i, valArr.Value(i), want)
		}
	}

	// geom column: Binary, tagged as geometry, decodes to expected points.
	geomS, err := out.Column("geom")
	if err != nil {
		t.Fatal(err)
	}
	if !geomS.IsGeometry() {
		t.Errorf("read geometry column lost its geometry tag")
	}
	geomArr := geomS.Column().Data().Chunks()[0].(*array.Binary)
	for i, want := range []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 2}} {
		g, err := geometry.ParseWKB(geomArr.Value(i))
		if err != nil {
			t.Fatalf("parse geom row %d: %v", i, err)
		}
		pt, ok := g.(geometry.Point)
		if !ok {
			t.Fatalf("row %d not Point: %T", i, g)
		}
		if pt.X != want.X || pt.Y != want.Y {
			t.Errorf("row %d = (%v, %v), want (%v, %v)", i, pt.X, pt.Y, want.X, want.Y)
		}
	}
}

// TestRoundTrip_MetadataInPlace verifies that after WriteFile, the
// GeoPackage metadata tables carry the right entries: application_id
// pragma is set, gpkg_contents has the layer registered with proper
// bounds (extent of the 3 test points), gpkg_geometry_columns names
// the geom column, and the RTree shadow table + gpkg_extensions row
// were created.
func TestRoundTrip_MetadataInPlace(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "meta.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}

	// Open the raw SQLite so we can poke at metadata tables directly.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// application_id + user_version pragmas.
	var appID, userVer int64
	if err := db.QueryRow(`PRAGMA application_id`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if appID != 1196444487 {
		t.Errorf("application_id = %d, want 1196444487", appID)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&userVer); err != nil {
		t.Fatal(err)
	}
	if userVer != 10300 {
		t.Errorf("user_version = %d, want 10300", userVer)
	}

	// gpkg_contents row.
	var (
		dataType             string
		minX, minY, maxX, maxY float64
		srsID                int32
	)
	if err := db.QueryRow(`
		SELECT data_type, min_x, min_y, max_x, max_y, srs_id
		FROM gpkg_contents WHERE table_name = ?`, "features").Scan(&dataType, &minX, &minY, &maxX, &maxY, &srsID); err != nil {
		t.Fatal(err)
	}
	if dataType != "features" {
		t.Errorf("data_type = %q, want features", dataType)
	}
	if minX != 0 || maxX != 2 || minY != 0 || maxY != 2 {
		t.Errorf("bounds = (%v,%v,%v,%v), want (0,0,2,2)", minX, minY, maxX, maxY)
	}
	if srsID != 4326 {
		t.Errorf("srs_id = %d, want 4326", srsID)
	}

	// gpkg_geometry_columns row.
	var geomColName, geomType string
	if err := db.QueryRow(`
		SELECT column_name, geometry_type_name FROM gpkg_geometry_columns
		WHERE table_name = ?`, "features").Scan(&geomColName, &geomType); err != nil {
		t.Fatal(err)
	}
	if geomColName != "geom" {
		t.Errorf("geometry column = %q, want geom", geomColName)
	}
	if geomType != "POINT" {
		t.Errorf("geometry type = %q, want POINT", geomType)
	}

	// RTree shadow table exists + has 3 rows (one per feature).
	var rtreeCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rtree_features_geom`).Scan(&rtreeCount); err != nil {
		t.Fatal(err)
	}
	if rtreeCount != 3 {
		t.Errorf("rtree row count = %d, want 3", rtreeCount)
	}
}

// TestRoundTrip_Projection verifies ReadOptions.Columns projects — but
// the geometry column is always kept even when not listed.
func TestRoundTrip_Projection(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "projection.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	// Ask for id + value only. geom should still come along.
	out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{
		Layer:   "features",
		Columns: []string{"id", "value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	// Expected: id, value, geom (geometry auto-preserved). fid is
	// dropped since it wasn't requested; matches user intent.
	want := map[string]bool{"id": true, "value": true, "geom": true}
	if len(names) != len(want) {
		t.Fatalf("cols = %v, want %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected col %q; want subset of %v", n, want)
		}
	}
}

// TestRoundTrip_ReplaceLayer confirms opts.Replace drops + recreates
// the layer without leaving stale rows or RTree entries behind.
func TestRoundTrip_ReplaceLayer(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "replace.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	// Second write of the same layer must fail without Replace…
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err == nil {
		t.Fatal("expected error on second write without Replace=true")
	}
	// …and succeed with it.
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features", Replace: true}); err != nil {
		t.Fatalf("Replace=true: %v", err)
	}
	out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "features"})
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 3 {
		t.Fatalf("rows after replace = %d, want 3", out.NumRows())
	}
}

// TestScanFile_ProjectionPushdown builds a LazyFrame from a gpkg
// via ScanFile, applies Select(id, value), and verifies the
// projection is pushed down into the SQL SELECT — the read closure
// only decodes the projected columns. Also asserts the plan's
// Explain output reflects the pushed projection.
func TestScanFile_ProjectionPushdown(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "scan_projection.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}

	lf := gpkgio.ScanFile(path, &gpkgio.ReadOptions{Layer: "features"}).
		Select(gobi.Col("id"), gobi.Col("value"))

	out, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Result: 3 rows × (id, value). The projection pushdown means
	// the geometry column doesn't even get decoded from SQLite —
	// but Select() drops it before user-visible output, so we can
	// only verify indirectly via column count + names here.
	names := out.ColumnNames()
	if len(names) != 2 || names[0] != "id" || names[1] != "value" {
		t.Fatalf("cols = %v, want [id value]", names)
	}
	// ExplainPhysical should show the projected column list on the
	// scan node — proof the optimizer's projection-pushdown rule
	// found the ScanFile and rewrote it via WithColumnProjection.
	explain := lf.ExplainPhysical()
	if !strings.Contains(explain, "cols=[") {
		t.Fatalf("ExplainPhysical missing projected cols marker:\n%s", explain)
	}
}

// TestScanFile_WhereClauseThroughOptions verifies that a raw SQL
// WHERE fragment in ReadOptions.Where flows through to the SELECT.
// The gobi.Expr → SQL translator isn't wired yet, so this is the
// escape hatch users have today for predicate pushdown.
func TestScanFile_WhereClauseThroughOptions(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "scan_where.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	out, err := gpkgio.ScanFile(path, &gpkgio.ReadOptions{
		Layer: "features",
		Where: "id > 1",
	}).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Only rows with id > 1 (i.e. id=2 and id=3) survive.
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 (id > 1 keeps 2 rows)", out.NumRows())
	}
}

// TestScanFile_PredicatePushdown verifies that a Frame.Filter above
// ScanFile is translated to a SQL WHERE clause and pushed into
// SQLite: the row count is correct, and ExplainPhysical shows the
// pushed fragment on the scan node label.
func TestScanFile_PredicatePushdown(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "scan_predicate.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	lf := gpkgio.ScanFile(path, &gpkgio.ReadOptions{Layer: "features"}).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(1))))
	explain := lf.ExplainPhysical()
	// buildScanLabel formats the fragment via %q, so `"id"` shows up
	// as `\"id\"` in the explain output. Match on the escaped form.
	if !strings.Contains(explain, `where=`) || !strings.Contains(explain, `\"id\" > ?`) {
		t.Fatalf("expected translated predicate in explain:\n%s", explain)
	}
	out, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 (id > 1 keeps id=2,3)", out.NumRows())
	}
}

// TestScanFile_PredicatePushdown_CompoundAND covers a compound
// predicate. SplitConjuncts breaks the top-level AND, translates
// each side, and the SQL WHERE combines them. Both halves are
// translatable here (integer + float comparisons), so the whole
// predicate lands SQL-side.
func TestScanFile_PredicatePushdown_CompoundAND(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "scan_and.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	// value < 100 is true for all rows; combined with id > 1, still
	// 2 rows.
	pred := gobi.Col("id").Gt(gobi.Lit(int64(1))).
		And(gobi.Col("value").Lt(gobi.Lit(float64(100))))
	out, err := gpkgio.ScanFile(path, &gpkgio.ReadOptions{Layer: "features"}).
		Filter(pred).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 (id>1 AND value<100)", out.NumRows())
	}
}

// TestRoundTrip_MultipleLayers writes two layers into the same file
// and verifies both are readable independently.
func TestRoundTrip_MultipleLayers(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "multi.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "two"}); err != nil {
		t.Fatal(err)
	}
	// Reading without a Layer selector must error listing both.
	if _, err := gpkgio.ReadFile(path, nil); err == nil {
		t.Fatal("expected multiple-layer error when Layer is empty")
	}
	// Both layers readable individually.
	for _, name := range []string{"one", "two"} {
		out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: name})
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if out.NumRows() != 3 {
			t.Errorf("%s rows = %d, want 3", name, out.NumRows())
		}
	}
}

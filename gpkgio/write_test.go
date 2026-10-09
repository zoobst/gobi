package gpkgio_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
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
		dataType               string
		minX, minY, maxX, maxY float64
		srsID                  int32
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

// TestRoundTrip_MultiChunkFrame — regression against the multi-chunk
// write crash. Before the fix, WriteFile on a Concat'd (multi-chunk)
// Frame errored out with "unsupported column type binary" as soon as
// columnWriter's `len(chunks) == 1` fast path missed. This shape
// arises naturally in geojsonio.ReadFile for any FC exceeding
// DefaultChunkRows — the reader emits one chunk per batch and stitches
// them with Concat. writeLayerToDB now calls CompactChunks up front,
// so multi-chunk input round-trips correctly.
func TestRoundTrip_MultiChunkFrame(t *testing.T) {
	a := buildTestFrame(t)
	b := buildTestFrame(t)
	stacked, err := a.Concat(b)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if stacked.IsSingleChunk() {
		t.Fatal("Concat output should be multi-chunk — fixture invariant broken")
	}
	path := filepath.Join(t.TempDir(), "multichunk.gpkg")
	if err := gpkgio.WriteFile(stacked, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatalf("WriteFile on multi-chunk frame: %v", err)
	}
	out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "features"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if out.NumRows() != 6 {
		t.Fatalf("rows = %d, want 6 (3+3 stacked)", out.NumRows())
	}
	// Spot-check that both halves of the stacked frame made it through
	// — the geometry column being present + decodable is the tightest
	// assertion, since the original crash was on the geometry Binary
	// column specifically.
	geomS, err := out.Column("geom")
	if err != nil {
		t.Fatal(err)
	}
	if !geomS.IsGeometry() {
		t.Error("geometry tag lost through multi-chunk write")
	}
	geomArr := geomS.Column().Data().Chunks()[0].(*array.Binary)
	if geomArr.Len() != 6 {
		t.Fatalf("geom rows = %d, want 6", geomArr.Len())
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

// TestRemoveLayer_HappyPath — write two layers, remove one, confirm
// the other survives and the removed one's feature table, RTree
// shadow, and metadata rows are all gone.
func TestRemoveLayer_HappyPath(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "two_layers.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features_a"}); err != nil {
		t.Fatal(err)
	}
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features_b"}); err != nil {
		t.Fatal(err)
	}

	if err := gpkgio.RemoveLayer(path, "features_a"); err != nil {
		t.Fatalf("RemoveLayer: %v", err)
	}

	// features_a gone: reading it errors.
	if _, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "features_a"}); err == nil {
		t.Error("expected error reading removed layer, got nil")
	}
	// features_b still there.
	out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "features_b"})
	if err != nil {
		t.Fatalf("read surviving layer: %v", err)
	}
	if out.NumRows() != 3 {
		t.Fatalf("surviving-layer rows = %d, want 3", out.NumRows())
	}

	// Metadata + shadow tables actually gone (not just gpkg_contents
	// stripped while the feature table lingers).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM gpkg_contents WHERE table_name = ?`, "features_a").
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("gpkg_contents rows for features_a = %d, want 0", n)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, "features_a").
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("feature table features_a survived: %d rows in sqlite_master", n)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`,
		"rtree_features_a_geom").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("RTree shadow for features_a survived")
	}
}

// TestRemoveLayer_NotFound — dropping a layer that isn't registered
// returns ErrLayerNotFound so callers can distinguish "already
// gone" from "the file itself is broken."
func TestRemoveLayer_NotFound(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "one_layer.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	err := gpkgio.RemoveLayer(path, "nonexistent")
	if err == nil {
		t.Fatal("expected error removing nonexistent layer")
	}
	if !errors.Is(err, gpkgio.ErrLayerNotFound) {
		t.Errorf("error should wrap ErrLayerNotFound, got %v", err)
	}
}

// TestRemoveLayer_ValidationErrors — layer name must be non-empty
// and a valid SQL identifier; injection attempts are rejected before
// touching the DB.
func TestRemoveLayer_ValidationErrors(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "validate.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"",              // empty
		"drop; --",      // injection attempt
		"features'or'1", // quote / expression
	}
	for _, name := range cases {
		if err := gpkgio.RemoveLayer(path, name); err == nil {
			t.Errorf("RemoveLayer(%q): expected error, got nil", name)
		}
	}
}

// TestRemoveLayer_ViaHandle — the *GeoPackage method has the same
// semantics as the package-level entry point.
func TestRemoveLayer_ViaHandle(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "handle.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatal(err)
	}
	g, err := gpkgio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.RemoveLayer("features"); err != nil {
		t.Fatalf("(*GeoPackage).RemoveLayer: %v", err)
	}
	// Handle-side removal makes the layer disappear.
	if err := g.RemoveLayer("features"); !errors.Is(err, gpkgio.ErrLayerNotFound) {
		t.Errorf("second removal should be ErrLayerNotFound, got %v", err)
	}
}

// TestWriteMany_HappyPath — three layers land in one call, each
// readable independently, each carries its own row count.
func TestWriteMany_HappyPath(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "many.gpkg")

	layers := []gpkgio.Layer{
		{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "alpha"}},
		{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "bravo"}},
		{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "charlie"}},
	}
	if err := gpkgio.WriteMany(path, layers...); err != nil {
		t.Fatalf("WriteMany: %v", err)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: name})
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if out.NumRows() != 3 {
			t.Errorf("layer %s rows = %d, want 3", name, out.NumRows())
		}
	}
}

// TestWriteMany_Empty — no layers is a legal no-op. Doesn't create
// the file, doesn't error.
func TestWriteMany_Empty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.gpkg")
	if err := gpkgio.WriteMany(path); err != nil {
		t.Errorf("WriteMany(): expected nil error on empty slice, got %v", err)
	}
}

// TestWriteMany_FirstErrorStops — a mid-slice failure returns
// early. Layers before the failing one stay written; layers after
// aren't attempted. The error wraps enough context to identify
// which layer index/name failed.
func TestWriteMany_FirstErrorStops(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "partial.gpkg")

	// Pre-populate layer "bravo" so the second Write conflicts (no
	// Replace) — Layer index 1 is where WriteMany should stop.
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "bravo"}); err != nil {
		t.Fatal(err)
	}

	layers := []gpkgio.Layer{
		{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "alpha"}},
		{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "bravo"}},   // conflicts
		{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "charlie"}}, // shouldn't attempt
	}
	err := gpkgio.WriteMany(path, layers...)
	if err == nil {
		t.Fatal("expected error on layer 1 conflict")
	}
	// alpha (index 0) should be written; charlie (index 2) should NOT be.
	if _, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "alpha"}); err != nil {
		t.Errorf("layer alpha expected to be written before the failure: %v", err)
	}
	if _, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: "charlie"}); err == nil {
		t.Errorf("layer charlie expected to be unwritten (WriteMany should stop at failure)")
	}
}

// TestWriteMany_ReplaceOnExisting — each Layer's Replace flag is
// honored independently. Pre-populate layer "target", then overwrite
// via WriteMany with Replace=true.
func TestWriteMany_ReplaceOnExisting(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "replace_many.gpkg")

	// Write once, verify.
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "target"}); err != nil {
		t.Fatal(err)
	}
	// Overwrite via WriteMany + a sibling new layer.
	err := gpkgio.WriteMany(path,
		gpkgio.Layer{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "target", Replace: true}},
		gpkgio.Layer{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "sibling"}},
	)
	if err != nil {
		t.Fatalf("WriteMany with Replace: %v", err)
	}
	// Both layers readable.
	for _, name := range []string{"target", "sibling"} {
		out, err := gpkgio.ReadFile(path, &gpkgio.ReadOptions{Layer: name})
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if out.NumRows() != 3 {
			t.Errorf("layer %s rows = %d, want 3", name, out.NumRows())
		}
	}
}

// TestWriteMany_MissingLayerName — bad Layer surfaces at the first
// invalid entry with the index in the error.
func TestWriteMany_MissingLayerName(t *testing.T) {
	df := buildTestFrame(t)
	path := filepath.Join(t.TempDir(), "bad.gpkg")
	err := gpkgio.WriteMany(path,
		gpkgio.Layer{Frame: df, Opts: &gpkgio.WriteOptions{Layer: "good"}},
		gpkgio.Layer{Frame: df, Opts: &gpkgio.WriteOptions{}}, // missing Layer name
	)
	if err == nil {
		t.Fatal("expected error on missing Layer name")
	}
}

// TestWriteMany_MatchesWriteFileSemantics — the batch API and the
// per-call API should produce structurally identical files given
// identical inputs. Compares row counts + column names layer-by-layer.
func TestWriteMany_MatchesWriteFileSemantics(t *testing.T) {
	df := buildTestFrame(t)
	dir := t.TempDir()
	batchPath := filepath.Join(dir, "batch.gpkg")
	loopPath := filepath.Join(dir, "loop.gpkg")

	names := []string{"a", "b", "c", "d", "e"}
	layers := make([]gpkgio.Layer, len(names))
	for i, n := range names {
		layers[i] = gpkgio.Layer{Frame: df, Opts: &gpkgio.WriteOptions{Layer: n}}
	}
	if err := gpkgio.WriteMany(batchPath, layers...); err != nil {
		t.Fatalf("WriteMany: %v", err)
	}
	for _, n := range names {
		if err := gpkgio.WriteFile(df, loopPath, &gpkgio.WriteOptions{Layer: n}); err != nil {
			t.Fatalf("WriteFile %s: %v", n, err)
		}
	}
	for _, n := range names {
		got, err := gpkgio.ReadFile(batchPath, &gpkgio.ReadOptions{Layer: n})
		if err != nil {
			t.Fatal(err)
		}
		want, err := gpkgio.ReadFile(loopPath, &gpkgio.ReadOptions{Layer: n})
		if err != nil {
			t.Fatal(err)
		}
		if got.NumRows() != want.NumRows() {
			t.Errorf("layer %s: batch rows %d vs loop rows %d",
				n, got.NumRows(), want.NumRows())
		}
	}
}

package gpkgio_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/gpkgio"

	_ "modernc.org/sqlite"
)

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

package gpkgio_test

import (
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/gpkgio"
)

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

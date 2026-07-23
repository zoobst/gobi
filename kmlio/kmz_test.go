package kmlio_test

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/kmlio"
)

// buildKMZTestFrame constructs a small Frame with the shape kmlio's
// writer expects: name + description + geometry columns. Reused
// across the KMZ tests so each one focuses on the compression /
// archive concern.
func buildKMZTestFrame(t *testing.T) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B"}, nil)
	descB := array.NewStringBuilder(pool)
	defer descB.Release()
	descB.AppendValues([]string{"first", "second"}, nil)
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, pt := range []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}} {
		geomB.Append(geometry.WKB(pt))
	}
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "description", Type: arrow.BinaryTypes.String, Nullable: false},
		gobi.GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), descB.NewArray(), geomB.NewArray()}
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

// TestKMZ_WriteThenReadFile writes a KMZ via WriteFile (extension
// auto-detects) and reads it back the same way. Verifies the file
// really is a zip archive containing a doc.kml entry, and the
// round-tripped Frame matches the input shape.
func TestKMZ_WriteThenReadFile(t *testing.T) {
	df := buildKMZTestFrame(t)
	path := filepath.Join(t.TempDir(), "cities.kmz")
	if err := kmlio.WriteFile(df, path, nil); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Sanity: verify the file is a valid zip with a doc.kml entry.
	// Reads the archive directly to check the structural expectation,
	// independent of kmlio's reader.
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()
	var haveDoc bool
	for _, entry := range zr.File {
		if entry.Name == "doc.kml" {
			haveDoc = true
		}
	}
	if !haveDoc {
		t.Fatal("KMZ missing doc.kml entry")
	}

	// Round-trip.
	back, err := kmlio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if back.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2", back.NumRows())
	}
	nameCol, _ := back.Column("name")
	names := nameCol.Column().Data().Chunks()[0].(*array.String)
	if names.Value(0) != "A" || names.Value(1) != "B" {
		t.Errorf("name round-trip lost data: %q %q", names.Value(0), names.Value(1))
	}
}

// TestKMZ_ExplicitFormatOverride exercises the FormatKMZ opt on
// Write / Read (io.Writer / io.Reader paths where there's no
// extension to auto-detect from).
func TestKMZ_ExplicitFormatOverride(t *testing.T) {
	df := buildKMZTestFrame(t)
	var buf bytes.Buffer
	if err := kmlio.Write(df, &buf, &kmlio.WriteOptions{Format: kmlio.FormatKMZ}); err != nil {
		t.Fatalf("Write KMZ: %v", err)
	}
	// The output should be a zip archive (starts with "PK\x03\x04").
	if len(buf.Bytes()) < 4 || string(buf.Bytes()[:2]) != "PK" {
		t.Fatalf("output doesn't look like a zip archive: %x", buf.Bytes()[:min(8, len(buf.Bytes()))])
	}
	back, err := kmlio.Read(bytes.NewReader(buf.Bytes()), &kmlio.ReadOptions{Format: kmlio.FormatKMZ})
	if err != nil {
		t.Fatalf("Read KMZ: %v", err)
	}
	if back.NumRows() != 2 {
		t.Errorf("rows = %d, want 2", back.NumRows())
	}
}

// TestKMZ_FallbackToNonDocKMLEntry checks the reader's fallback
// behavior when the archive uses a non-standard entry name. Some
// tools emit KMZ archives with the KML at "layer.kml" or similar
// instead of "doc.kml"; gobi should still read them.
func TestKMZ_FallbackToNonDocKMLEntry(t *testing.T) {
	// Build a KMZ with a non-doc.kml entry name manually.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("layer.kml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(citiesKML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	// Read it through the KMZ path — should locate layer.kml via
	// the fallback rule.
	path := filepath.Join(t.TempDir(), "non_doc.kmz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	df, err := kmlio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if df.NumRows() != 3 {
		t.Fatalf("rows = %d, want 3 (citiesKML has 3 placemarks)", df.NumRows())
	}
}

// TestKMZ_ErrorOnNonZipInput verifies the KMZ reader rejects a
// plain KML file when Format is set to KMZ (which triggers zip
// parsing). Catches accidental misuse where the format doesn't
// match the input.
func TestKMZ_ErrorOnNonZipInput(t *testing.T) {
	_, err := kmlio.Read(strings.NewReader(citiesKML), &kmlio.ReadOptions{Format: kmlio.FormatKMZ})
	if err == nil {
		t.Fatal("expected KMZ read of plain KML to error, got nil")
	}
}

package parquetio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/parquetio"
)

type structTestRow struct {
	ID      int64   `parquet:"id"`
	Name    string  `parquet:"name" gobi:"name_fallback"`
	Score   float64 `gobi:"score"` // no parquet tag; gobi: is used
	Ignored string  `parquet:"-"`  // skipped
}

func TestWriteReadStructs_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "structs.parquet")

	rows := []structTestRow{
		{ID: 1, Name: "alice", Score: 3.14, Ignored: "skipme"},
		{ID: 2, Name: "bob", Score: 2.71, Ignored: "gone"},
	}
	if err := parquetio.WriteStructs(rows, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}

	got, err := parquetio.ReadStructs[structTestRow](path, nil)
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("row count = %d, want %d", len(got), len(rows))
	}
	for i, r := range got {
		if r.ID != rows[i].ID {
			t.Errorf("row %d ID = %v, want %v", i, r.ID, rows[i].ID)
		}
		if r.Name != rows[i].Name {
			t.Errorf("row %d Name = %q, want %q", i, r.Name, rows[i].Name)
		}
		if r.Score != rows[i].Score {
			t.Errorf("row %d Score = %v, want %v", i, r.Score, rows[i].Score)
		}
		// Ignored was omitted from the file — after read it's the zero value.
		if r.Ignored != "" {
			t.Errorf("row %d Ignored = %q, want empty (field was parquet:\"-\")", i, r.Ignored)
		}
	}
}

// TestWriteStructs_UsesParquetTagOverCsv verifies the parquet: tag
// wins over a csv: tag on the same field. This is the whole point of
// per-io tag namespaces.
func TestWriteStructs_UsesParquetTagOverCsv(t *testing.T) {
	type Row struct {
		Val int64 `parquet:"pq_col" csv:"csv_col"`
	}
	rows := []Row{{Val: 42}}
	dir := t.TempDir()
	path := filepath.Join(dir, "tag_priority.parquet")

	if err := parquetio.WriteStructs(rows, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}

	// Peek at the schema — column should be "pq_col", not "csv_col".
	schema, err := parquetio.ReadSchema(path, nil)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	if _, ok := schema.FieldsByName("pq_col"); !ok {
		t.Errorf("column pq_col not found; parquet: tag should win over csv:")
	}
	if _, ok := schema.FieldsByName("csv_col"); ok {
		t.Errorf("column csv_col present; csv: tag should have been shadowed by parquet:")
	}
}

func TestReadStructs_MissingFile(t *testing.T) {
	_, err := parquetio.ReadStructs[structTestRow]("/does/not/exist.parquet", nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	// Sanity: the file we referenced really doesn't exist.
	if _, statErr := os.Stat("/does/not/exist.parquet"); statErr == nil {
		t.Fatalf("test precondition failed: file exists")
	}
}

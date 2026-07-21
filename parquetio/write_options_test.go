package parquetio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/parquet/file"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// numRowGroups opens path and returns the parquet file's row-group
// count. Used to verify RowGroupRows had the intended effect on write.
func numRowGroups(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pf, err := file.NewParquetReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	return pf.NumRowGroups()
}

func TestWriteOptions_RowGroupRows_Splits(t *testing.T) {
	// 5,000 rows with RowGroupRows=1000 should produce 5 row groups.
	df := makeSyntheticFrame(t, 5_000)
	path := filepath.Join(t.TempDir(), "split.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	got := numRowGroups(t, path)
	if got != 5 {
		t.Fatalf("row groups = %d, want 5 (5000 rows / 1000 per group)", got)
	}
}

func TestWriteOptions_RowGroupRows_UnsetSingleGroup(t *testing.T) {
	// Without RowGroupRows set, a small frame lands in a single row
	// group (parquet-arrow's default is ~1M rows per group).
	df := makeSyntheticFrame(t, 5_000)
	path := filepath.Join(t.TempDir(), "default.parquet")
	if err := parquetio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	if got := numRowGroups(t, path); got != 1 {
		t.Fatalf("row groups = %d, want 1 (default sizing on 5k rows)", got)
	}
}

func TestWriteOptions_NilUsesSnappyDefault(t *testing.T) {
	// A nil WriteOptions round-trips cleanly — codec defaults to Snappy.
	df := makeSyntheticFrame(t, 100)
	path := filepath.Join(t.TempDir(), "nil.parquet")
	if err := parquetio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	// Confirm the file reads back with the same shape.
	loaded, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if loaded.NumRows() != 100 {
		t.Fatalf("rows = %d, want 100", loaded.NumRows())
	}
}

func TestWriteOptions_StreamsInSplitFile(t *testing.T) {
	// End-to-end: writing with small row groups produces a file the
	// streaming reader can process one row group at a time. Verifies
	// the round-trip works and yields the expected total rows.
	const nRows = 4_000
	const rowsPerGroup = 800
	df := makeSyntheticFrame(t, nRows)
	path := filepath.Join(t.TempDir(), "stream_split.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: rowsPerGroup,
	}); err != nil {
		t.Fatal(err)
	}

	if got := numRowGroups(t, path); got != nRows/rowsPerGroup {
		t.Fatalf("row groups = %d, want %d", got, nRows/rowsPerGroup)
	}

	var total int
	err := parquetio.ReadFileChunksFunc(path, nil, func(f *gobi.Frame) error {
		total += f.NumRows()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != nRows {
		t.Fatalf("streamed %d rows, want %d", total, nRows)
	}
}

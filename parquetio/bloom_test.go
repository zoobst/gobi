package parquetio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"

	"github.com/zoobst/gobi/parquetio"
)

// bloomFilterFor opens path and returns the bloom filter for the named
// column in the first row group. Returns nil if the column has no
// bloom filter attached (which is what we expect when the caller
// didn't ask for one).
func bloomFilterFor(t *testing.T, path, colName string) metadata.BloomFilter {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	pf, err := file.NewParquetReader(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Close() })

	// Look up the column index by name from the file schema.
	schema := pf.MetaData().Schema
	colIdx := -1
	for i := range schema.NumColumns() {
		if schema.Column(i).Name() == colName {
			colIdx = i
			break
		}
	}
	if colIdx < 0 {
		t.Fatalf("column %q not in schema", colName)
	}

	rgBF, err := pf.GetBloomFilterReader().RowGroup(0)
	if err != nil {
		t.Fatal(err)
	}
	bf, err := rgBF.GetColumnBloomFilter(colIdx)
	if err != nil {
		t.Fatalf("read bloom filter for %q: %v", colName, err)
	}
	return bf
}

func TestWriteOptions_BloomFilter_WrittenForRequestedColumn(t *testing.T) {
	// Ask for a bloom filter on "key" (string column) and confirm the
	// filter shows up in the written file.
	df := makeSyntheticFrame(t, 5_000)
	path := filepath.Join(t.TempDir(), "bloom.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:              parquetio.CodecSnappy,
		BloomFilterColumns: []string{"key"},
	}); err != nil {
		t.Fatal(err)
	}
	bf := bloomFilterFor(t, path, "key")
	if bf == nil {
		t.Fatal("bloom filter missing from key column")
	}
	if bf.Size() <= 0 {
		t.Fatalf("bloom filter size = %d, want > 0 (empty filter)", bf.Size())
	}
}

func TestWriteOptions_BloomFilter_NotWrittenForOtherColumns(t *testing.T) {
	// Bloom filter requested only for "key" — "id" should have none.
	df := makeSyntheticFrame(t, 5_000)
	path := filepath.Join(t.TempDir(), "bloom_selective.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:              parquetio.CodecSnappy,
		BloomFilterColumns: []string{"key"},
	}); err != nil {
		t.Fatal(err)
	}
	if bf := bloomFilterFor(t, path, "id"); bf != nil {
		t.Fatalf("unrequested column has bloom filter (size=%d)", bf.Size())
	}
	if bf := bloomFilterFor(t, path, "value_a"); bf != nil {
		t.Fatalf("unrequested column has bloom filter (size=%d)", bf.Size())
	}
}

func TestWriteOptions_BloomFilter_NoneByDefault(t *testing.T) {
	// Writing without BloomFilterColumns must not emit any bloom
	// filters — the default should leave file size untouched vs old
	// behavior for callers who never asked for them.
	df := makeSyntheticFrame(t, 5_000)
	path := filepath.Join(t.TempDir(), "no_bloom.parquet")
	if err := parquetio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"id", "value_a", "key"} {
		if bf := bloomFilterFor(t, path, col); bf != nil {
			t.Errorf("column %q got unexpected bloom filter (size=%d)", col, bf.Size())
		}
	}
}

func TestWriteOptions_BloomFilter_MultipleColumns(t *testing.T) {
	df := makeSyntheticFrame(t, 5_000)
	path := filepath.Join(t.TempDir(), "bloom_multi.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:              parquetio.CodecSnappy,
		BloomFilterColumns: []string{"id", "key"},
		BloomFilterFPP:     0.01,
	}); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"id", "key"} {
		bf := bloomFilterFor(t, path, col)
		if bf == nil {
			t.Errorf("column %q missing bloom filter", col)
			continue
		}
		if bf.Size() <= 0 {
			t.Errorf("column %q bloom filter size = %d, want > 0", col, bf.Size())
		}
	}
	// A column not on the list should still have none.
	if bf := bloomFilterFor(t, path, "value_a"); bf != nil {
		t.Errorf("value_a got unexpected bloom filter (size=%d)", bf.Size())
	}
}

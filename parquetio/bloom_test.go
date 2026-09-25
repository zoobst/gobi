package parquetio_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"

	"github.com/zoobst/gobi/parquetio"
)

// openColumn opens path and resolves colName (a top-level column
// path) to its leaf index. The reader closes on test cleanup.
func openColumn(t *testing.T, path, colName string) (*file.Reader, int) {
	t.Helper()
	pf, err := file.OpenParquetFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Close() })
	colIdx := pf.MetaData().Schema.ColumnIndexByName(colName)
	if colIdx < 0 {
		t.Fatalf("column %q not in schema", colName)
	}
	return pf, colIdx
}

// bloomFilterAt returns colIdx's bloom filter in row group rg, or nil
// if the column has none.
func bloomFilterAt(t *testing.T, pf *file.Reader, colIdx, rg int) metadata.BloomFilter {
	t.Helper()
	rgBF, err := pf.GetBloomFilterReader().RowGroup(rg)
	if err != nil {
		t.Fatal(err)
	}
	bf, err := rgBF.GetColumnBloomFilter(colIdx)
	if err != nil {
		t.Fatalf("read bloom filter (col %d, rg %d): %v", colIdx, rg, err)
	}
	return bf
}

// bloomFilterFor returns the bloom filter for colName in the first
// row group, or nil if the column has none (what we expect when the
// caller didn't ask for one).
func bloomFilterFor(t *testing.T, path, colName string) metadata.BloomFilter {
	t.Helper()
	pf, colIdx := openColumn(t, path, colName)
	return bloomFilterAt(t, pf, colIdx, 0)
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

// bloomLengths returns the on-disk bloom filter length (bytes,
// including a ~16 B serialized header) for colName in every row
// group of path. 0 means no filter.
func bloomLengths(t *testing.T, path, colName string) []int32 {
	t.Helper()
	pf, colIdx := openColumn(t, path, colName)
	out := make([]int32, pf.NumRowGroups())
	for rg := range pf.NumRowGroups() {
		cc, err := pf.MetaData().RowGroup(rg).ColumnChunk(colIdx)
		if err != nil {
			t.Fatal(err)
		}
		out[rg] = cc.BloomFilterLength()
	}
	return out
}

// TestWriteOptions_BloomFilter_SizedToCardinality — without adaptive
// sizing arrow-go writes every filter at its 1 MiB max. A 100-NDV
// row group must get a small filter, and a 20k-NDV one a
// proportionate one, with no false negatives.
func TestWriteOptions_BloomFilter_SizedToCardinality(t *testing.T) {
	df := makeSyntheticFrame(t, 200_000) // key: 100 distinct; id: unique
	path := filepath.Join(t.TempDir(), "bloom_sized.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		RowGroupRows:       20_000,
		BloomFilterColumns: []string{"key", "id"},
		SkipBboxCovering:   true,
	}); err != nil {
		t.Fatal(err)
	}
	keyLens := bloomLengths(t, path, "key")
	idLens := bloomLengths(t, path, "id")
	if len(keyLens) != 10 {
		t.Fatalf("row groups = %d, want 10", len(keyLens))
	}
	for rg := range keyLens {
		// Lengths include a small (~16 B) serialized header.
		// 100 NDV @ 1% → the 2 KiB floor candidate.
		if keyLens[rg] <= 0 || keyLens[rg] > 2<<10+64 {
			t.Errorf("rg %d key bloom = %d B, want (0, 2 KiB]", rg, keyLens[rg])
		}
		// 20k NDV @ 1% needs ~24 KB; arrow-go rates the 32 KiB
		// candidate at ~13.5k NDV, so 64 KiB wins.
		if idLens[rg] <= 16<<10 || idLens[rg] > 64<<10+64 {
			t.Errorf("rg %d id bloom = %d B, want (16 KiB, 64 KiB]", rg, idLens[rg])
		}
	}
	t.Logf("per-row-group bloom bytes: key=%d id=%d (was 1 MiB each)", keyLens[0], idLens[0])

	// No false negatives in any row group, including id — the
	// high-NDV column, where adaptive candidates are dropped
	// mid-row-group and the survivor must still hold every insert.
	pfKey, keyIdx := openColumn(t, path, "key")
	pfID, idIdx := openColumn(t, path, "id")
	for rg := range 10 {
		kbf := metadata.TypedBloomFilter[parquet.ByteArray]{BloomFilter: bloomFilterAt(t, pfKey, keyIdx, rg)}
		for i := range 100 {
			if v := fmt.Sprintf("k%d", i); !kbf.Check(parquet.ByteArray(v)) {
				t.Errorf("rg %d: key false negative for %q", rg, v)
			}
		}
		ibf := metadata.TypedBloomFilter[int64]{BloomFilter: bloomFilterAt(t, pfID, idIdx, rg)}
		misses := 0
		for id := int64(rg * 20_000); id < int64((rg+1)*20_000); id++ {
			if !ibf.Check(id) {
				misses++
			}
		}
		if misses > 0 {
			t.Errorf("rg %d: %d id false negatives", rg, misses)
		}
	}
}

// TestWriteOptions_BloomFilter_NDVAndMaxBytes — an explicit NDV sizes
// the filter from it; BloomFilterMaxBytes caps adaptive candidates.
func TestWriteOptions_BloomFilter_NDVAndMaxBytes(t *testing.T) {
	df := makeSyntheticFrame(t, 50_000)
	path := filepath.Join(t.TempDir(), "bloom_ndv.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		BloomFilterColumns:  []string{"key", "id"},
		BloomFilterNDV:      map[string]int64{"key": 100_000},
		BloomFilterMaxBytes: 16 << 10,
		SkipBboxCovering:    true,
	}); err != nil {
		t.Fatal(err)
	}
	// key: sized for 100k NDV (~120 KB) but capped at 16 KiB.
	if l := bloomLengths(t, path, "key")[0]; l < 8<<10 || l > 17<<10 {
		t.Errorf("key bloom = %d B, want ~16 KiB (NDV sizing, capped)", l)
	}
	// id: 50k NDV needs ~60 KB, over the 16 KiB cap → largest candidate.
	if l := bloomLengths(t, path, "id")[0]; l <= 0 || l > 17<<10 {
		t.Errorf("id bloom = %d B, want (0, 16 KiB]", l)
	}
}

// TestWriteOptions_BloomFilter_MaxBytesRoundsDown — a non-power-of-two
// cap rounds down, so no path writes a filter over the cap and the
// NDV path never gets a size that isn't a whole number of blocks.
func TestWriteOptions_BloomFilter_MaxBytesRoundsDown(t *testing.T) {
	df := makeSyntheticFrame(t, 50_000)
	path := filepath.Join(t.TempDir(), "bloom_cap.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		BloomFilterColumns:  []string{"key", "id"},
		BloomFilterNDV:      map[string]int64{"key": 100_000},
		BloomFilterMaxBytes: 24_000, // → 16 KiB
		SkipBboxCovering:    true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"key", "id"} {
		if l := bloomLengths(t, path, col)[0]; l <= 0 || l > 16<<10+64 {
			t.Errorf("%s bloom = %d B, want (0, 16 KiB] (24000 rounds down)", col, l)
		}
	}
}

// TestWriteOptions_BloomFilter_FloorFollowsFPP — the adaptive floor
// isn't hardcoded to the 1% case: at 10% FPP a 100-NDV row group gets
// a smaller filter than at 1%.
func TestWriteOptions_BloomFilter_FloorFollowsFPP(t *testing.T) {
	df := makeSyntheticFrame(t, 20_000) // key: 100 distinct
	lenAt := func(fpp float64) int32 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("bloom_fpp_%v.parquet", fpp))
		if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
			BloomFilterColumns: []string{"key"},
			BloomFilterFPP:     fpp,
			SkipBboxCovering:   true,
		}); err != nil {
			t.Fatal(err)
		}
		return bloomLengths(t, path, "key")[0]
	}
	tight, loose := lenAt(0.01), lenAt(0.1)
	if loose <= 0 || loose >= tight {
		t.Errorf("key bloom at 10%% FPP = %d B, want smaller than %d B at 1%%", loose, tight)
	}
	t.Logf("100-NDV filter: %d B at 1%% FPP, %d B at 10%%", tight, loose)
}

// TestWriteOptions_BloomFilter_Validation — misconfigurations fail the
// write instead of silently writing no filter or a wrapped size.
func TestWriteOptions_BloomFilter_Validation(t *testing.T) {
	df := makeSyntheticFrame(t, 1_000)
	cases := map[string]*parquetio.WriteOptions{
		"NDV without column": {BloomFilterNDV: map[string]int64{"id": 5000}},
		"NDV for other column": {
			BloomFilterColumns: []string{"key"},
			BloomFilterNDV:     map[string]int64{"id": 5000},
		},
		"negative NDV": {
			BloomFilterColumns: []string{"id"},
			BloomFilterNDV:     map[string]int64{"id": -1},
		},
		"NDV past uint32": {
			BloomFilterColumns: []string{"id"},
			BloomFilterNDV:     map[string]int64{"id": 1 << 32},
		},
		"negative MaxBytes": {BloomFilterColumns: []string{"id"}, BloomFilterMaxBytes: -1},
		"tiny MaxBytes":     {BloomFilterColumns: []string{"id"}, BloomFilterMaxBytes: 16},
		"huge MaxBytes":     {BloomFilterColumns: []string{"id"}, BloomFilterMaxBytes: 1 << 30},
		"FPP 1":             {BloomFilterColumns: []string{"id"}, BloomFilterFPP: 1},
		"negative FPP":      {BloomFilterColumns: []string{"id"}, BloomFilterFPP: -0.1},
	}
	for name, opts := range cases {
		opts.SkipBboxCovering = true
		path := filepath.Join(t.TempDir(), "bad.parquet")
		if err := parquetio.WriteFile(df, path, opts); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

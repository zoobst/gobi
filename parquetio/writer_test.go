package parquetio_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

type wRow struct {
	ID    int64    `parquet:"id"`
	Name  string   `parquet:"name"`
	Big   uint32   `parquet:"big"`
	Empty *float64 `parquet:"empty"` // always nil: all-null column
	Geom  string   `parquet:"geometry" geom:"true"`
}

// wFrame builds n rows starting at id0; points walk along x = id.
func wFrame(t *testing.T, id0, n int) *gobi.Frame {
	t.Helper()
	rows := make([]wRow, n)
	for i := range rows {
		id := id0 + i
		rows[i] = wRow{
			ID:   int64(id),
			Name: fmt.Sprintf("n%03d", id),
			Big:  uint32(id) * 1_000_000_000, // crosses MaxInt32: unsigned order matters
			Geom: fmt.Sprintf("POINT(%d %d)", id, -id),
		}
	}
	f, err := gobi.FromStructs(rows, gobi.StructTagFormat("parquet"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Release)
	return f
}

func rowGroupSizes(t *testing.T, data []byte) []int64 {
	t.Helper()
	pf, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	out := make([]int64, pf.NumRowGroups())
	for i := range out {
		out[i] = pf.MetaData().RowGroup(i).NumRows()
	}
	return out
}

func footerKV(t *testing.T, data []byte, key string) []string {
	t.Helper()
	pf, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var out []string
	for _, kv := range pf.MetaData().KeyValueMetadata() {
		if kv.Key == key {
			out = append(out, kv.GetValue())
		}
	}
	return out
}

// TestWriter_RowGroupBoundaries — EndRowGroup cuts exactly where
// called, several Writes share a group, and stray EndRowGroup calls
// (at the start, doubled, before Close) never produce empty groups.
func TestWriter_RowGroupBoundaries(t *testing.T) {
	var buf bytes.Buffer
	schema := wFrame(t, 0, 1).Schema()
	w, err := parquetio.NewWriter(&buf, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(w.EndRowGroup()) // nothing buffered: no-op
	must(w.Write(wFrame(t, 0, 3)))
	must(w.EndRowGroup())
	must(w.EndRowGroup()) // doubled: no-op
	must(w.Write(wFrame(t, 3, 5)))
	must(w.Write(wFrame(t, 8, 2))) // same group as the previous Write
	must(w.Write(wFrame(t, 10, 0)))
	must(w.EndRowGroup()) // before Close: no-op
	if w.NumRows() != 10 {
		t.Errorf("NumRows = %d, want 10", w.NumRows())
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := rowGroupSizes(t, buf.Bytes()); fmt.Sprint(got) != "[3 7]" {
		t.Errorf("row groups = %v, want [3 7]", got)
	}
	if stats.NumRows != 10 || stats.NumRowGroups != 2 || stats.FileSize != int64(buf.Len()) {
		t.Errorf("stats = rows %d, groups %d, size %d (file %d)", stats.NumRows, stats.NumRowGroups, stats.FileSize, buf.Len())
	}

	// The file reads back whole and in order.
	back, err := parquetio.ReadReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Release()
	rows, err := gobi.ToStructs[wRow](back, gobi.StructTagFormat("parquet"))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if r.ID != int64(i) || r.Name != fmt.Sprintf("n%03d", i) {
			t.Fatalf("row %d = %+v", i, r)
		}
	}
}

// TestWriter_RowGroupRowsSplits — RowGroupRows still caps groups,
// combined with manual boundaries.
func TestWriter_RowGroupRowsSplits(t *testing.T) {
	var buf bytes.Buffer
	w, err := parquetio.NewWriter(&buf, wFrame(t, 0, 1).Schema(), &parquetio.WriteOptions{RowGroupRows: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(wFrame(t, 0, 10)); err != nil {
		t.Fatal(err)
	}
	w.EndRowGroup()
	if err := w.Write(wFrame(t, 10, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := rowGroupSizes(t, buf.Bytes()); fmt.Sprint(got) != "[4 4 2 3]" {
		t.Errorf("row groups = %v, want [4 4 2 3]", got)
	}
}

// TestWriter_MatchesWrite — batches through Writer produce the same
// on-disk schema and the same "geo" footer (bbox and types merged
// across batches) as Write on the concatenated frame.
func TestWriter_MatchesWrite(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("SkipBboxCovering=%v", skip), func(t *testing.T) {
			a, b := wFrame(t, 0, 4), wFrame(t, 50, 3)
			all, err := gobi.Concat(a, b)
			if err != nil {
				t.Fatal(err)
			}
			defer all.Release()
			opts := &parquetio.WriteOptions{SkipBboxCovering: skip}

			var whole bytes.Buffer
			if err := parquetio.Write(all, &whole, opts); err != nil {
				t.Fatal(err)
			}
			var inc bytes.Buffer
			w, err := parquetio.NewWriter(&inc, a.Schema(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Write(a); err != nil {
				t.Fatal(err)
			}
			w.EndRowGroup()
			if err := w.Write(b); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Close(); err != nil {
				t.Fatal(err)
			}

			wg, ig := footerKV(t, whole.Bytes(), gobi.GeoParquetMetadataKey), footerKV(t, inc.Bytes(), gobi.GeoParquetMetadataKey)
			if len(wg) != 1 || len(ig) != 1 || wg[0] != ig[0] {
				t.Errorf("geo footer differs:\nWrite:  %v\nWriter: %v", wg, ig)
			}
			wm, _ := gobi.ParseGeoParquetMetadata(ig[0])
			if bb := wm.Columns["geometry"].Bbox; fmt.Sprint(bb) != "[0 -52 52 0]" {
				t.Errorf("merged bbox = %v, want [0 -52 52 0]", bb)
			}
			ws, err := parquetio.ReadReader(bytes.NewReader(whole.Bytes()), int64(whole.Len()), &parquetio.ReadOptions{IncludeCoveringColumns: true})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Release()
			is, err := parquetio.ReadReader(bytes.NewReader(inc.Bytes()), int64(inc.Len()), &parquetio.ReadOptions{IncludeCoveringColumns: true})
			if err != nil {
				t.Fatal(err)
			}
			defer is.Release()
			if !ws.Schema().Equal(is.Schema()) {
				t.Errorf("schemas differ:\nWrite:  %s\nWriter: %s", ws.Schema(), is.Schema())
			}
			if hasCovering := strings.Contains(is.Schema().String(), "geometry_bbox_xmin"); hasCovering == skip {
				t.Errorf("covering columns present = %v with SkipBboxCovering=%v", hasCovering, skip)
			}
		})
	}
}

// TestWriter_FileStats — bounds merge across row groups in the
// column's sort order; null counts; all-null columns have no bounds;
// MinBytes is the PLAIN encoding of Min.
func TestWriter_FileStats(t *testing.T) {
	var buf bytes.Buffer
	w, err := parquetio.NewWriter(&buf, wFrame(t, 0, 1).Schema(), &parquetio.WriteOptions{SkipBboxCovering: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id0 := range []int{2, 0, 4} { // row groups out of id order
		if err := w.Write(wFrame(t, id0, 1)); err != nil {
			t.Fatal(err)
		}
		w.EndRowGroup()
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]parquetio.ColumnStats{}
	for _, c := range stats.Columns {
		col[c.Path] = c
	}

	id := col["id"]
	if !id.HasMinMax || id.Min != int64(0) || id.Max != int64(4) || id.NumValues != 3 || !id.HasNullCount || id.NullCount != 0 {
		t.Errorf("id stats = %+v", id)
	}
	if got := int64(binary.LittleEndian.Uint64(id.MinBytes)); got != 0 || len(id.MaxBytes) != 8 ||
		int64(binary.LittleEndian.Uint64(id.MaxBytes)) != 4 {
		t.Errorf("id MinBytes/MaxBytes = %x / %x", id.MinBytes, id.MaxBytes)
	}
	name := col["name"]
	if !name.HasMinMax || string(name.Min.([]byte)) != "n000" || string(name.Max.([]byte)) != "n004" ||
		string(name.MinBytes) != "n000" {
		t.Errorf("name stats = %+v", name)
	}
	// uint32 is INT32 physical with an unsigned sort order: 4e9 must be
	// the max even though it is negative as an int32.
	big := col["big"]
	if !big.HasMinMax || uint32(big.Min.(int32)) != 0 || uint32(big.Max.(int32)) != 4_000_000_000 {
		t.Errorf("big (uint32) bounds = %v / %v, want 0 / 4e9", big.Min, big.Max)
	}
	empty := col["empty"]
	if empty.HasMinMax || empty.NullCount != 3 || !empty.HasNullCount {
		t.Errorf("all-null column stats = %+v", empty)
	}
	if stats.Metadata == nil || stats.Metadata.NumRows != 3 {
		t.Error("raw footer missing")
	}

	// StatsFromMetadata on the same footer read back agrees.
	pf, err := file.NewParquetReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	again, err := parquetio.StatsFromMetadata(pf.MetaData(), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again.Columns) != fmt.Sprint(stats.Columns) {
		t.Errorf("StatsFromMetadata(read footer) differs from Close stats")
	}
}

// TestWriter_Errors — option validation up front; a schema mismatch
// is rejected without poisoning the writer; closed writers refuse.
func TestWriter_Errors(t *testing.T) {
	schema := wFrame(t, 0, 1).Schema()
	var sink bytes.Buffer
	if _, err := parquetio.NewWriter(&sink, schema, &parquetio.WriteOptions{HilbertSort: true}); err == nil {
		t.Error("HilbertSort: want error")
	}
	if _, err := parquetio.NewWriter(&sink, schema, &parquetio.WriteOptions{BloomFilterFPP: 2}); err == nil {
		t.Error("bad bloom FPP: want error")
	}
	if _, err := parquetio.NewWriter(&sink, nil, nil); err == nil {
		t.Error("nil schema: want error")
	}

	w, err := parquetio.NewWriter(&sink, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	type other struct{ X int32 }
	wrong, err := gobi.FromStructs([]other{{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Release()
	if err := w.Write(wrong); err == nil {
		t.Error("schema mismatch: want error")
	}
	if err := w.Write(wFrame(t, 0, 2)); err != nil {
		t.Errorf("valid Write after a rejected one: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(wFrame(t, 0, 1)); err == nil {
		t.Error("Write after Close: want error")
	}
	if _, err := w.Close(); err == nil {
		t.Error("second Close: want error")
	}
}

// TestWriter_EmptyFile — no Writes still yields a valid file with the
// schema and a geo entry (no bbox).
func TestWriter_EmptyFile(t *testing.T) {
	var buf bytes.Buffer
	schema := wFrame(t, 0, 1).Schema()
	w, err := parquetio.NewWriter(&buf, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.NumRows != 0 || stats.NumRowGroups != 0 {
		t.Errorf("stats = %+v", stats)
	}
	f, err := parquetio.ReadReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()), nil)
	if err != nil {
		t.Fatalf("read empty file: %v", err)
	}
	defer f.Release()
	if f.NumRows() != 0 {
		t.Errorf("rows = %d", f.NumRows())
	}
	geo := footerKV(t, buf.Bytes(), gobi.GeoParquetMetadataKey)
	if len(geo) != 1 {
		t.Fatalf("geo entries = %d, want 1", len(geo))
	}
	if m, _ := gobi.ParseGeoParquetMetadata(geo[0]); m.Columns["geometry"].Bbox != nil {
		t.Errorf("empty file bbox = %v, want none", m.Columns["geometry"].Bbox)
	}
}

// TestWriter_BloomFiltersPerRowGroup — writer options carry through.
func TestWriter_BloomFiltersPerRowGroup(t *testing.T) {
	path := t.TempDir() + "/bloom.parquet"
	var buf bytes.Buffer
	w, err := parquetio.NewWriter(&buf, wFrame(t, 0, 1).Schema(), &parquetio.WriteOptions{
		BloomFilterColumns: []string{"name"}, SkipBboxCovering: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(wFrame(t, 0, 5)); err != nil {
		t.Fatal(err)
	}
	w.EndRowGroup()
	if err := w.Write(wFrame(t, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	lens := bloomLengths(t, path, "name")
	if len(lens) != 2 || lens[0] <= 0 || lens[1] <= 0 {
		t.Errorf("bloom lengths = %v, want a filter in each of 2 row groups", lens)
	}
}

// TestWriter_InvalidCoerceTimestamps — CoerceTimestamps is validated
// at NewWriter, like every other option.
func TestWriter_InvalidCoerceTimestamps(t *testing.T) {
	type r struct {
		TS int64 `parquet:"ts"`
	}
	f, err := gobi.FromStructs([]r{{1}}, gobi.StructTagFormat("parquet"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	var buf bytes.Buffer
	if _, err := parquetio.NewWriter(&buf, f.Schema(), &parquetio.WriteOptions{CoerceTimestamps: "bogus"}); err == nil {
		t.Error("invalid CoerceTimestamps: want error at NewWriter")
	}
}

// failAfter accepts n bytes, then fails every write.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.n {
		k := f.n
		f.n = 0
		return k, errors.New("disk full")
	}
	f.n -= len(p)
	return len(p), nil
}

// TestWriter_StickyError — once a flush fails, every later call
// reports it and Close still returns an error.
func TestWriter_StickyError(t *testing.T) {
	w, err := parquetio.NewWriter(&failAfter{n: 4}, wFrame(t, 0, 1).Schema(), nil) // room for the magic only
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(wFrame(t, 0, 100)); err != nil {
		t.Fatalf("first Write (buffered, nothing flushed yet): %v", err)
	}
	w.EndRowGroup()
	first := w.Write(wFrame(t, 100, 1)) // closing group 1 flushes → fails
	if first == nil {
		t.Fatal("Write after a failed flush: want error")
	}
	if err := w.Write(wFrame(t, 0, 1)); err == nil || err.Error() != first.Error() {
		t.Errorf("later Write = %v, want the sticky %v", err, first)
	}
	if err := w.EndRowGroup(); err == nil {
		t.Error("EndRowGroup after failure: want error")
	}
	if _, err := w.Close(); err == nil {
		t.Error("Close after failure: want error")
	}
}

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

// TestWriteStructs_RequiredColumnsOnDisk — StructRequiredFields makes
// non-pointer columns REQUIRED in the parquet schema.
func TestWriteStructs_RequiredColumnsOnDisk(t *testing.T) {
	type row struct {
		ID   int64   `parquet:"id"`
		Name string  `parquet:"name"`
		Opt  *string `parquet:"opt"`
	}
	f, err := gobi.FromStructs([]row{{1, "a", nil}}, gobi.StructTagFormat("parquet"), gobi.StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	var buf bytes.Buffer
	if err := parquetio.Write(f, &buf, nil); err != nil {
		t.Fatal(err)
	}
	pf, err := file.NewParquetReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	want := map[string]parquet.Repetition{"id": parquet.Repetitions.Required, "name": parquet.Repetitions.Required, "opt": parquet.Repetitions.Optional}
	sc := pf.MetaData().Schema
	for i := range sc.NumColumns() {
		c := sc.Column(i)
		if got := c.SchemaNode().RepetitionType(); got != want[c.Name()] {
			t.Errorf("%s repetition = %s, want %s", c.Name(), got, want[c.Name()])
		}
	}
}

// textFrame: high-cardinality text-like strings, where the codec (not
// dictionary encoding) does the work.
func textFrame(t *testing.T) *gobi.Frame {
	type row struct{ S string }
	rows := make([]row, 5_000)
	for i := range rows {
		rows[i] = row{fmt.Sprintf("user-%08d/session/%d/event-%d", i*7919%1000003, i%13, i)}
	}
	f, err := gobi.FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Release)
	return f
}

// TestWriteOptions_CompressionLevel — the level reaches the codec (the
// output changes; zstd tiers aren't monotonic in size, so don't
// assert "smaller") and is range-checked per codec.
func TestWriteOptions_CompressionLevel(t *testing.T) {
	f := textFrame(t)
	write := func(opts *parquetio.WriteOptions) []byte {
		var buf bytes.Buffer
		if err := parquetio.Write(f, &buf, opts); err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
		return buf.Bytes()
	}
	def := write(&parquetio.WriteOptions{Codec: parquetio.CodecZstd})
	best := write(&parquetio.WriteOptions{Codec: parquetio.CodecZstd, CompressionLevel: 19})
	if bytes.Equal(def, best) {
		t.Error("zstd level 19 produced the same bytes as the default level; level not applied")
	}
	gz1 := write(&parquetio.WriteOptions{Codec: parquetio.CodecGzip, CompressionLevel: 1})
	gz9 := write(&parquetio.WriteOptions{Codec: parquetio.CodecGzip, CompressionLevel: 9})
	if len(gz9) >= len(gz1) {
		t.Errorf("gzip 9 = %d B, gzip 1 = %d B; want 9 smaller", len(gz9), len(gz1))
	}
	write(&parquetio.WriteOptions{Codec: parquetio.CodecBrotli, CompressionLevel: 4})

	for _, bad := range []*parquetio.WriteOptions{
		{Codec: parquetio.CodecZstd, CompressionLevel: 23},
		{Codec: parquetio.CodecZstd, CompressionLevel: -1},
		{Codec: parquetio.CodecGzip, CompressionLevel: 10},
		{Codec: parquetio.CodecSnappy, CompressionLevel: 3},
		{CompressionLevel: 3}, // default codec is snappy
	} {
		if err := parquetio.Write(f, &bytes.Buffer{}, bad); err == nil {
			t.Errorf("%+v: want error", bad)
		}
		if _, err := parquetio.NewWriter(&bytes.Buffer{}, f.Schema(), bad); err == nil {
			t.Errorf("NewWriter %+v: want error", bad)
		}
	}
}

// countingAlloc counts allocations through an inner allocator.
type countingAlloc struct {
	*memory.CheckedAllocator
	n int
}

func (c *countingAlloc) Allocate(size int) []byte {
	c.n++
	return c.CheckedAllocator.Allocate(size)
}

// TestWriteOptions_Allocator — the writer allocates from the given
// allocator, including generated bbox covering columns, and frees it
// all by the time Write returns.
func TestWriteOptions_Allocator(t *testing.T) {
	type row struct {
		ID   int64  `parquet:"id"`
		Geom string `parquet:"geometry" geom:"true"`
	}
	f, err := gobi.FromStructs([]row{{1, "POINT(1 2)"}, {2, "POINT(3 4)"}}, gobi.StructTagFormat("parquet"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	alloc := &countingAlloc{CheckedAllocator: memory.NewCheckedAllocator(memory.NewGoAllocator())}
	if err := parquetio.Write(f, &bytes.Buffer{}, &parquetio.WriteOptions{Allocator: alloc}); err != nil {
		t.Fatal(err)
	}
	if alloc.n == 0 {
		t.Error("Allocator never used")
	}
	alloc.AssertSize(t, 0)

	alloc2 := &countingAlloc{CheckedAllocator: memory.NewCheckedAllocator(memory.NewGoAllocator())}
	w, err := parquetio.NewWriter(&bytes.Buffer{}, f.Schema(), &parquetio.WriteOptions{Allocator: alloc2})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(f); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if alloc2.n == 0 {
		t.Error("Writer: Allocator never used")
	}
	alloc2.AssertSize(t, 0)
}

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

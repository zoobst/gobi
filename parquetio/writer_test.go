package parquetio_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/parquet/file"

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

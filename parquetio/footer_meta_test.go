package parquetio_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// checkFooterMeta asserts md carries owner=etl and team=geo from the
// footer, a "geo" entry exactly when wantGeo, and no ARROW:schema.
func checkFooterMeta(t *testing.T, path string, md arrow.Metadata, wantGeo bool) {
	t.Helper()
	for k, want := range map[string]string{"owner": "etl", "team": "geo"} {
		if got, ok := md.GetValue(k); !ok || got != want {
			t.Errorf("%s: %q = %q (present %v), want %q", path, k, got, ok, want)
		}
	}
	if _, ok := md.GetValue(gobi.GeoParquetMetadataKey); ok != wantGeo {
		t.Errorf("%s: geo present = %v, want %v", path, ok, wantGeo)
	}
	if _, ok := md.GetValue("ARROW:schema"); ok {
		t.Errorf("%s: ARROW:schema leaked into schema metadata", path)
	}
}

// TestRead_FooterKeyValueMetadata — every read path puts the footer's
// key/value metadata on the schema / Frame it returns, for plain and
// GeoParquet files alike.
func TestRead_FooterKeyValueMetadata(t *testing.T) {
	type plain struct{ X float64 }
	plainDF, err := gobi.FromStructs([]plain{{1}, {2}, {3}})
	if err != nil {
		t.Fatal(err)
	}
	defer plainDF.Release()
	geoDF := pointFrame(t, []float64{1, 2}, []float64{3, 4}, 0)
	defer geoDF.Release()

	kv := map[string]string{"owner": "etl", "team": "geo"}
	for name, tc := range map[string]struct {
		df      *gobi.Frame
		wantGeo bool
	}{"plain": {plainDF, false}, "geo": {geoDF, true}} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kv.parquet")
			if err := parquetio.WriteFile(tc.df, path, &parquetio.WriteOptions{KeyValueMetadata: kv}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			sc, err := parquetio.ReadSchema(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkFooterMeta(t, "ReadSchema", sc.Metadata(), tc.wantGeo)

			sc, err = parquetio.ReadSchemaReader(bytes.NewReader(data), int64(len(data)), nil)
			if err != nil {
				t.Fatal(err)
			}
			checkFooterMeta(t, "ReadSchemaReader", sc.Metadata(), tc.wantGeo)

			df, err := parquetio.ReadFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkFooterMeta(t, "ReadFile", df.Schema().Metadata(), tc.wantGeo)
			df.Release()

			df, err = parquetio.ReadReader(bytes.NewReader(data), int64(len(data)), nil)
			if err != nil {
				t.Fatal(err)
			}
			checkFooterMeta(t, "ReadReader", df.Schema().Metadata(), tc.wantGeo)
			df.Release()

			if err := parquetio.ReadFileChunksFunc(path, nil, func(f *gobi.Frame) error {
				checkFooterMeta(t, "ReadFileChunksFunc", f.Schema().Metadata(), tc.wantGeo)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := parquetio.ReadReaderChunksFunc(bytes.NewReader(data), int64(len(data)), nil, func(f *gobi.Frame) error {
				checkFooterMeta(t, "ReadReaderChunksFunc", f.Schema().Metadata(), tc.wantGeo)
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			df, err = parquetio.ScanFile(path, nil).Collect()
			if err != nil {
				t.Fatal(err)
			}
			checkFooterMeta(t, "ScanFile", df.Schema().Metadata(), tc.wantGeo)
			df.Release()
		})
	}
}

// TestRead_FooterKeyValueMetadata_RoundTrip — a Frame read with
// footer metadata and written back keeps one footer entry per key,
// and an explicit KeyValueMetadata value replaces the carried one.
func TestRead_FooterKeyValueMetadata_RoundTrip(t *testing.T) {
	src := pointFrame(t, []float64{1}, []float64{1}, 0)
	defer src.Release()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.parquet")
	if err := parquetio.WriteFile(src, in, &parquetio.WriteOptions{
		KeyValueMetadata: map[string]string{"owner": "etl", "team": "geo"},
	}); err != nil {
		t.Fatal(err)
	}
	df, err := parquetio.ReadFile(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()

	out := filepath.Join(dir, "out.parquet")
	if err := parquetio.WriteFile(df, out, &parquetio.WriteOptions{
		KeyValueMetadata: map[string]string{"owner": "rewrite"},
	}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"owner": "rewrite", "team": "geo"} {
		if got := footerValues(t, out, key); len(got) != 1 || got[0] != want {
			t.Errorf("footer %q = %v, want [%s]", key, got, want)
		}
	}
	for _, key := range []string{gobi.GeoParquetMetadataKey, "ARROW:schema"} {
		if got := footerValues(t, out, key); len(got) != 1 {
			t.Errorf("footer %q entries = %d, want 1", key, len(got))
		}
	}
}

// TestRead_FooterKeyValueMetadata_DuplicateKey — a footer that repeats
// a key (another writer's doing) yields one schema entry holding the
// first value, matching KeyValueMetadata.FindValue.
func TestRead_FooterKeyValueMetadata_DuplicateKey(t *testing.T) {
	pool := memory.NewGoAllocator()
	schema := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}}, nil)
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues([]int64{1, 2}, nil)
	arr := b.NewArray()
	defer arr.Release()
	rec := array.NewRecordBatch(schema, []arrow.Array{arr}, 2)
	defer rec.Release()

	var buf bytes.Buffer
	w, err := pqarrow.NewFileWriter(schema, &buf, parquet.NewWriterProperties(), pqarrow.NewArrowWriterProperties())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	for _, v := range []string{"first", "second"} {
		if err := w.AppendKeyValueMetadata("dup", v); err != nil {
			_ = w.Close()
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	df, err := parquetio.ReadReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	md := df.Schema().Metadata()
	n := 0
	for _, k := range md.Keys() {
		if k == "dup" {
			n++
		}
	}
	if got, _ := md.GetValue("dup"); n != 1 || got != "first" {
		t.Errorf("dup: %d entries, value %q; want 1 entry, \"first\"", n, got)
	}
}

// TestWrite_DropsColumnDescribingKeys — keys other writers use to
// describe the columns ("pandas", Spark row metadata, …) aren't
// copied from a Frame's schema metadata into a new footer, since they
// may no longer match its columns; other keys are. An explicit
// KeyValueMetadata entry is still written.
func TestWrite_DropsColumnDescribingKeys(t *testing.T) {
	src := pointFrame(t, []float64{1}, []float64{1}, 0)
	defer src.Release()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.parquet")
	if err := parquetio.WriteFile(src, in, &parquetio.WriteOptions{KeyValueMetadata: map[string]string{
		"pandas": `{"columns":["old"]}`, "org.apache.spark.sql.parquet.row.metadata": "{}", "owner": "etl",
	}}); err != nil {
		t.Fatal(err)
	}
	df, err := parquetio.ReadFile(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	// Readers still see them.
	if _, ok := df.Schema().Metadata().GetValue("pandas"); !ok {
		t.Fatal("read: pandas key missing")
	}

	out := filepath.Join(dir, "out.parquet")
	if err := parquetio.WriteFile(df, out, nil); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"pandas": 0, "org.apache.spark.sql.parquet.row.metadata": 0, "owner": 1} {
		if got := footerValues(t, out, key); len(got) != want {
			t.Errorf("footer %q = %v, want %d entries", key, got, want)
		}
	}

	if err := parquetio.WriteFile(df, out, &parquetio.WriteOptions{KeyValueMetadata: map[string]string{"pandas": "new"}}); err != nil {
		t.Fatal(err)
	}
	if got := footerValues(t, out, "pandas"); len(got) != 1 || got[0] != "new" {
		t.Errorf("explicit pandas = %v, want [new]", got)
	}
}

// TestWrite_TimestampUnitIsUTC — NewTimestampSeriesUnit columns are
// written isAdjustedToUTC=true (pyarrow: datetime64[unit, UTC]);
// NewTimestampSeries columns stay zone-naive.
func TestWrite_TimestampUnitIsUTC(t *testing.T) {
	ts := []time.Time{time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)}
	us, err := gobi.NewTimestampSeriesUnit("us", ts, nil, arrow.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	ns := gobi.NewTimestampSeries("ns", []time.Time{time.Unix(0, 0)}, nil)
	df, err := gobi.NewFrameFromSeries(us, ns)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	path := filepath.Join(t.TempDir(), "ts.parquet")
	if err := parquetio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	pf, err := file.OpenParquetFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	for i, want := range []bool{true, false} {
		lt, ok := pf.MetaData().Schema.Column(i).LogicalType().(interface{ IsAdjustedToUTC() bool })
		if !ok || lt.IsAdjustedToUTC() != want {
			t.Errorf("column %d: logical type %v, want isAdjustedToUTC=%v", i, pf.MetaData().Schema.Column(i).LogicalType(), want)
		}
	}
	back, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Release()
	col, _ := back.Column("us")
	if got, _, _ := col.AsTimes(); !got[0].Equal(ts[0]) {
		t.Errorf("year 2300 round trip: %v", got[0])
	}
}

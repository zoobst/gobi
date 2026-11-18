package parquetio_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/metadata"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// footerReader is an in-memory file that counts reads of its footer
// (the Thrift metadata, its length and the closing magic).
type footerReader struct {
	*bytes.Reader
	footerStart int64
	mu          sync.Mutex
	footerReads int
}

func newFooterReader(t testing.TB, b []byte) *footerReader {
	t.Helper()
	n := int64(len(b))
	footerLen := int64(binary.LittleEndian.Uint32(b[n-8 : n-4]))
	return &footerReader{Reader: bytes.NewReader(b), footerStart: n - 8 - footerLen}
}

func (r *footerReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > r.footerStart {
		r.mu.Lock()
		r.footerReads++
		r.mu.Unlock()
	}
	return r.Reader.ReadAt(p, off)
}

// groupedFile writes df in row groups of 1,000.
func groupedFile(t testing.TB, df *gobi.Frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := parquetio.Write(df, &buf, &parquetio.WriteOptions{RowGroupRows: 1_000}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func footerOf(t testing.TB, b []byte) *metadata.FileMetaData {
	t.Helper()
	md, err := parquetio.ReadMetadata(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func sameFrames(t *testing.T, got, want *gobi.Frame) {
	t.Helper()
	gt, wt := got.Table(), want.Table()
	defer gt.Release()
	defer wt.Release()
	if !gt.Schema().Equal(wt.Schema()) || gt.NumRows() != wt.NumRows() {
		t.Fatalf("got %v (%d rows), want %v (%d rows)", gt.Schema(), gt.NumRows(), wt.Schema(), wt.NumRows())
	}
	for i := range int(wt.NumCols()) {
		if !array.ChunkedEqual(gt.Column(i).Data(), wt.Column(i).Data()) {
			t.Errorf("column %s differs", wt.Column(i).Name())
		}
	}
}

// idSum is the row count and the sum of the id column: a check that
// doesn't depend on row order (parallel scans don't keep it).
func idSum(t *testing.T, f *gobi.Frame) (rows int, sum int64) {
	t.Helper()
	tbl := f.Table()
	defer tbl.Release()
	for i := range int(tbl.NumCols()) {
		if tbl.Column(i).Name() != "id" {
			continue
		}
		for _, c := range tbl.Column(i).Data().Chunks() {
			for _, v := range c.(*array.Int64).Int64Values() {
				sum += v
			}
		}
	}
	return f.NumRows(), sum
}

func TestReadReader_Metadata(t *testing.T) {
	df := makeSyntheticFrame(t, 5_000)
	defer df.Release()
	b := groupedFile(t, df)
	md := footerOf(t, b)
	first := df.Schema().Field(0).Name

	for _, opts := range []parquetio.ReadOptions{
		{RowGroups: []int{1, 3}},
		{RowGroups: []int{4}, Columns: []string{first}},
		{Predicate: gobi.Col("id").Gt(gobi.Lit(int64(3_500)))}, // prunes groups 0-2 by stats
		{},
	} {
		plain := newFooterReader(t, b)
		want, err := parquetio.ReadReader(plain, int64(len(b)), &opts)
		if err != nil {
			t.Fatal(err)
		}
		if plain.footerReads == 0 {
			t.Fatal("a read without Metadata didn't read the footer")
		}

		reuse := newFooterReader(t, b)
		withMD := opts
		withMD.Metadata = md
		got, err := parquetio.ReadReader(reuse, int64(len(b)), &withMD)
		if err != nil {
			t.Fatal(err)
		}
		if reuse.footerReads != 0 {
			t.Errorf("%+v: read the footer %d times despite Metadata", opts, reuse.footerReads)
		}
		sameFrames(t, got, want)
		got.Release()
		want.Release()
	}

	// The schema, and the chunked reader.
	r := newFooterReader(t, b)
	schema, err := parquetio.ReadSchemaReader(r, int64(len(b)), &parquetio.ReadOptions{Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	if schema.NumFields() != df.NumCols() || schema.Field(0).Name != first {
		t.Errorf("schema %v", schema)
	}
	rows := 0
	err = parquetio.ReadReaderChunksFunc(r, int64(len(b)), &parquetio.ReadOptions{Metadata: md, RowGroups: []int{2, 4}},
		func(f *gobi.Frame) error {
			rows += f.NumRows()
			return nil
		})
	if err != nil || rows != 2_000 {
		t.Errorf("ReadReaderChunksFunc: %d rows, %v", rows, err)
	}
	if r.footerReads != 0 {
		t.Errorf("ReadSchemaReader / ReadReaderChunksFunc read the footer %d times despite Metadata", r.footerReads)
	}
}

func TestReadFile_Metadata(t *testing.T) {
	df := makeSyntheticFrame(t, 5_000)
	defer df.Release()
	b := groupedFile(t, df)
	md := footerOf(t, b)
	path := filepath.Join(t.TempDir(), "grouped.parquet")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	want, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer want.Release()
	got, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	sameFrames(t, got, want)
	got.Release()

	rows := 0
	err = parquetio.ReadFileChunksFunc(path, &parquetio.ReadOptions{Metadata: md, RowGroups: []int{0}}, func(f *gobi.Frame) error {
		rows += f.NumRows()
		return nil
	})
	if err != nil || rows != 1_000 {
		t.Errorf("ReadFileChunksFunc: %d rows, %v", rows, err)
	}

	// A parallel scan with predicate pruning: the same rows (in any
	// order) as without Metadata.
	scan := func(md *metadata.FileMetaData) *gobi.Frame {
		t.Helper()
		f, err := parquetio.ScanFile(path, &parquetio.ReadOptions{Metadata: md, ScanWorkers: 3}).
			Filter(gobi.Col("id").Ge(gobi.Lit(int64(1_500)))).Collect()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	plain, reused := scan(nil), scan(md)
	defer plain.Release()
	defer reused.Release()
	pr, ps := idSum(t, plain)
	rr, rs := idSum(t, reused)
	if pr != 3_500 || pr != rr || ps != rs {
		t.Errorf("ScanFile: %d rows (id sum %d) with Metadata, %d (%d) without; want 3500", rr, rs, pr, ps)
	}
}

// TestReadReader_MetadataSharedAcrossGoroutines reads row groups on many
// goroutines with one Metadata, the way a map loads its view. Run with
// -race. arrow-go fills the writer version in on first use when it's
// unset: parsing a footer sets it, but metadata assembled from a parsed
// footer's fields (as below) doesn't, so reads sharing it would write it
// at once if they didn't each work on their own copy.
func TestReadReader_MetadataSharedAcrossGoroutines(t *testing.T) {
	df := makeSyntheticFrame(t, 5_000)
	defer df.Release()
	b := groupedFile(t, df)
	parsed := footerOf(t, b)
	for _, md := range []*metadata.FileMetaData{
		parsed,
		{FileMetaData: parsed.FileMetaData, Schema: parsed.Schema}, // writer version unset
	} {
		readConcurrently(t, b, md)
	}
}

func readConcurrently(t *testing.T, b []byte, md *metadata.FileMetaData) {
	t.Helper()
	r := bytes.NewReader(b)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := parquetio.ReadReader(r, int64(len(b)), &parquetio.ReadOptions{RowGroups: []int{i % 5}, Metadata: md})
			if err != nil {
				t.Error(err)
				return
			}
			if f.NumRows() != 1_000 {
				t.Errorf("group %d: %d rows", i%5, f.NumRows())
			}
			f.Release()
		}()
	}
	wg.Wait()
}

func TestReadReader_MetadataFromAnotherFile(t *testing.T) {
	big := makeSyntheticFrame(t, 5_000)
	defer big.Release()
	small := makeSyntheticFrame(t, 1_500)
	defer small.Release()
	bigFile, smallFile := groupedFile(t, big), groupedFile(t, small)
	md := footerOf(t, bigFile)

	// Groups that lie past the end of the smaller file are refused,
	// before anything is read.
	r := newFooterReader(t, smallFile)
	_, err := parquetio.ReadReader(r, int64(len(smallFile)), &parquetio.ReadOptions{RowGroups: []int{3}, Metadata: md})
	if err == nil || !strings.Contains(err.Error(), "doesn't match this file") {
		t.Errorf("err = %v, want a mismatch error", err)
	}
	// So is the whole file truncated.
	cut := bigFile[:len(bigFile)/2]
	_, err = parquetio.ReadReader(bytes.NewReader(cut), int64(len(cut)), &parquetio.ReadOptions{Metadata: md})
	if err == nil || !strings.Contains(err.Error(), "doesn't match this file") {
		t.Errorf("truncated file: err = %v, want a mismatch error", err)
	}
}

func TestReadReader_MetadataRefusesEncryptedFiles(t *testing.T) {
	df := makeSyntheticFrame(t, 2_000)
	defer df.Release()
	tbl := df.Table()
	defer tbl.Release()
	var buf bytes.Buffer
	enc := parquet.NewFileEncryptionProperties("0123456789012345", parquet.WithPlaintextFooter())
	props := parquet.NewWriterProperties(parquet.WithEncryptionProperties(enc))
	if err := pqarrow.WriteTable(tbl, &buf, 1_000, props, pqarrow.DefaultWriterProps()); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	md, err := parquetio.ReadMetadata(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = parquetio.ReadReader(bytes.NewReader(b), int64(len(b)), &parquetio.ReadOptions{Metadata: md})
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("err = %v, want encrypted files refused", err)
	}
}

// BenchmarkReadReader_rowGroupFooter reads one row group of a file with
// many row groups (a big footer), parsing the footer each time or not.
func BenchmarkReadReader_rowGroupFooter(b *testing.B) {
	df := wideFrame(b, 200_000, 40)
	defer df.Release()
	data := groupedFile(b, df)
	md := footerOf(b, data)
	b.Logf("%d row groups, footer %d KB", md.NumRowGroups(), md.Size()>>10)
	for _, bc := range []struct {
		name string
		md   *metadata.FileMetaData
	}{{"parse", nil}, {"reuse", md}} {
		b.Run(bc.name, func(b *testing.B) {
			r := bytes.NewReader(data)
			for i := range b.N {
				f, err := parquetio.ReadReader(r, int64(len(data)), &parquetio.ReadOptions{
					RowGroups: []int{i % md.NumRowGroups()}, Columns: []string{df.Schema().Field(0).Name}, Metadata: bc.md,
				})
				if err != nil {
					b.Fatal(err)
				}
				f.Release()
			}
		})
	}
}

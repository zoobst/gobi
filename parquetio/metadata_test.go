package parquetio_test

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// footerReader is an in-memory file that records reads of its footer
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

// groupedFile writes n synthetic rows in row groups of 1,000.
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
	pf, err := file.NewParquetReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	return pf.MetaData()
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

func TestReadReader_Metadata(t *testing.T) {
	df := makeSyntheticFrame(t, 5_000)
	defer df.Release()
	b := groupedFile(t, df)
	md := footerOf(t, b)

	for _, opts := range []parquetio.ReadOptions{
		{RowGroups: []int{1, 3}},
		{RowGroups: []int{4}, Columns: []string{df.Schema().Field(0).Name}},
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

	// The schema too.
	r := newFooterReader(t, b)
	schema, err := parquetio.ReadSchemaReader(r, int64(len(b)), &parquetio.ReadOptions{Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	if schema.NumFields() != df.NumCols() || schema.Field(0).Name != df.Schema().Field(0).Name {
		t.Errorf("schema %v", schema)
	}
	if r.footerReads != 0 {
		t.Errorf("ReadSchemaReader read the footer %d times despite Metadata", r.footerReads)
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

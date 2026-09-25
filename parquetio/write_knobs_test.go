package parquetio_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

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

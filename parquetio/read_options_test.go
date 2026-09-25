package parquetio_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// wideFrame: nCols Float64 columns, value = row*1000 + col.
func wideFrame(t testing.TB, rows, nCols int) *gobi.Frame {
	t.Helper()
	fields := make([]arrow.Field, nCols)
	cols := make([]arrow.Column, nCols)
	for c := range nCols {
		b := array.NewFloat64Builder(memory.DefaultAllocator)
		for r := range rows {
			b.Append(float64(r*1000 + c))
		}
		a := b.NewArray()
		b.Release()
		fields[c] = arrow.Field{Name: fmt.Sprintf("c%02d", c), Type: arrow.PrimitiveTypes.Float64}
		ch := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		a.Release()
		cols[c] = *arrow.NewColumn(fields[c], ch)
		ch.Release()
	}
	f, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestReadOptions_DecodeModesAgree — serial column decode and buffered
// streaming return exactly what the default read does.
func TestReadOptions_DecodeModesAgree(t *testing.T) {
	const rows, nCols = 20_000, 24
	df := wideFrame(t, rows, nCols)
	defer df.Release()
	path := filepath.Join(t.TempDir(), "wide.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{RowGroupRows: 5_000}); err != nil {
		t.Fatal(err)
	}
	modes := map[string]*parquetio.ReadOptions{
		"default":  nil,
		"serial":   {SerialColumnDecode: true},
		"buffered": {BufferedStreamBytes: 4 << 10},
		"both":     {SerialColumnDecode: true, BufferedStreamBytes: 4 << 10},
	}
	for name, opts := range modes {
		f, err := parquetio.ReadFile(path, opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f.NumRows() != rows || f.NumCols() != nCols {
			t.Fatalf("%s: shape %dx%d", name, f.NumRows(), f.NumCols())
		}
		for c := range nCols {
			s, _ := f.Column(fmt.Sprintf("c%02d", c))
			off := 0
			for _, ch := range s.Column().Data().Chunks() {
				vals := ch.(*array.Float64).Float64Values()
				for i, v := range vals {
					if v != float64((off+i)*1000+c) {
						t.Fatalf("%s: c%02d row %d = %v", name, c, off+i, v)
					}
				}
				off += len(vals)
			}
		}
		f.Release()

		// Streaming path uses the same reader setup.
		n := 0
		if err := parquetio.ReadFileChunksFunc(path, opts, func(fr *gobi.Frame) error {
			n += fr.NumRows()
			return nil
		}); err != nil {
			t.Fatalf("%s chunks: %v", name, err)
		}
		if n != rows {
			t.Errorf("%s chunks: %d rows, want %d", name, n, rows)
		}
	}

	if _, err := parquetio.ReadFile(path, &parquetio.ReadOptions{BufferedStreamBytes: -1}); err == nil {
		t.Error("negative BufferedStreamBytes: want error")
	}
}

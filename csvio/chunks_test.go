package csvio_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
)

// makeSyntheticCSV builds a CSV with n rows and 4 columns (name, i, f, note).
func makeSyntheticCSV(n int) string {
	var b strings.Builder
	b.WriteString("name,i,f,note\n")
	for r := range n {
		fmt.Fprintf(&b, "row-%d,%d,%d.5,note-%d\n", r, r*7, r, r%128)
	}
	return b.String()
}

type basicRow struct {
	Name string  `csv:"name"`
	I    int64   `csv:"i"`
	F    float64 `csv:"f"`
	Note string  `csv:"note"`
}

func TestReadChunksFunc_ChunkSize(t *testing.T) {
	// 25k rows with ChunkRows=10k should yield 3 chunks (10k + 10k + 5k).
	src := makeSyntheticCSV(25_000)
	var chunkCount int
	var totalRows int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.Options{ChunkRows: 10_000},
		func(f *gobi.Frame) error {
			chunkCount++
			totalRows += f.NumRows()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if chunkCount != 3 {
		t.Fatalf("chunk count = %d, want 3", chunkCount)
	}
	if totalRows != 25_000 {
		t.Fatalf("total rows across chunks = %d, want 25_000", totalRows)
	}
}

func TestReadChunksFunc_CallbackErrorAborts(t *testing.T) {
	src := makeSyntheticCSV(10_000)
	sentinel := errors.New("boom")
	var invocations int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.Options{ChunkRows: 1_000},
		func(f *gobi.Frame) error {
			invocations++
			if invocations == 2 {
				return sentinel
			}
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected error from aborted callback")
	}
	if !errors.Is(err, csvio.ErrChunksAborted) {
		t.Fatalf("want ErrChunksAborted in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("original callback error should be wrapped: %v", err)
	}
	if invocations != 2 {
		t.Fatalf("callback invocations = %d, want 2 (stopped after error)", invocations)
	}
}

func TestReadChunksFunc_DataIntegrityAcrossChunks(t *testing.T) {
	// Confirm each row's values are preserved in-order across chunks.
	const n = 5_000
	src := makeSyntheticCSV(n)
	var rowIdx int64
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.Options{ChunkRows: 500},
		func(f *gobi.Frame) error {
			iCol, _ := f.Column("i")
			arr := iCol.Column().Data().Chunks()[0].(*array.Int64)
			for i := 0; i < arr.Len(); i++ {
				want := rowIdx * 7
				if arr.Value(i) != want {
					return fmt.Errorf("row %d i = %d, want %d", rowIdx, arr.Value(i), want)
				}
				rowIdx++
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if rowIdx != n {
		t.Fatalf("saw %d rows, want %d", rowIdx, n)
	}
}

func TestReadChunksFunc_RetainAcrossCallback(t *testing.T) {
	// A callback that retains a Frame must be able to read from it after
	// the callback returns.
	src := makeSyntheticCSV(2_000)
	var kept []*gobi.Frame
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.Options{ChunkRows: 500},
		func(f *gobi.Frame) error {
			f.Retain()
			kept = append(kept, f)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) == 0 {
		t.Fatal("no frames retained")
	}
	// Access the buffers of a retained frame — this should not crash.
	last := kept[len(kept)-1]
	iCol, _ := last.Column("i")
	arr := iCol.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Len() == 0 {
		t.Fatal("retained frame lost its data")
	}
	// Match retains with releases so we don't leak.
	for _, f := range kept {
		f.Release()
	}
}

func TestReadFileChunksFunc_GzipAutoDetect(t *testing.T) {
	// End-to-end: gzipped CSV on disk, streaming callback, geometry column.
	dir := t.TempDir()
	path := filepath.Join(dir, "cities.csv.gz")
	src := `name,population,geometry
New York,8804190,POINT (-74.0060 40.7128)
Los Angeles,3898747,POINT (-118.2437 34.0522)
Chicago,2746388,POINT (-87.6298 41.8781)
`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte(src))
	gw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	var totalRows int
	err := csvio.ReadFileChunksFunc[city](path, &csvio.Options{CRSHint: 4326},
		func(f *gobi.Frame) error {
			totalRows += f.NumRows()
			// Sanity: geometry column is decoded.
			g, err := f.Geometry("geometry", 0)
			if err != nil {
				return err
			}
			if g == nil {
				return errors.New("geometry column decoded as nil")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if totalRows != 3 {
		t.Fatalf("total rows = %d, want 3", totalRows)
	}
}

func TestReadChunksFunc_EmptyInput(t *testing.T) {
	// Only a header, no data rows: fn should never be called, no error.
	var called int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader("name,i,f,note\n"), nil,
		func(f *gobi.Frame) error {
			called++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("fn called %d times on empty input, want 0", called)
	}
}

// bounded-memory smoke test: process a large synthetic CSV in small chunks
// without accumulating.
func TestReadChunksFunc_BoundedMemoryLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large memory test in short mode")
	}
	src := makeSyntheticCSV(200_000)
	var total int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.Options{ChunkRows: 8_192},
		func(f *gobi.Frame) error {
			total += f.NumRows()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if total != 200_000 {
		t.Fatalf("total rows = %d, want 200_000", total)
	}
}

// discardWriter is a small helper used by nothing here, kept to catch
// import drift if we later expand the streaming tests.
var _ = io.Discard

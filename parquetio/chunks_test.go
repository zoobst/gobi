package parquetio_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
	"github.com/zoobst/gobi/parquetio"
)

// makeSyntheticFrame builds an n-row Frame with (id int64, value_a
// float64, key string) columns. Used as write-side input for the
// streaming and projection tests.
func makeSyntheticFrame(t *testing.T, n int) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	aB := array.NewFloat64Builder(pool)
	defer aB.Release()
	keyB := array.NewStringBuilder(pool)
	defer keyB.Release()
	for i := range n {
		idB.Append(int64(i))
		aB.Append(float64(i) * 0.5)
		keyB.Append(fmt.Sprintf("k%d", i%100))
	}
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "value_a", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "key", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), aB.NewArray(), keyB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// writeFixture writes df to a temp file and returns the path.
func writeFixture(t *testing.T, df *gobi.Frame, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := parquetio.WriteFile(df, path, parquetio.CodecSnappy); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestReadFile_ColumnProjection(t *testing.T) {
	df := makeSyntheticFrame(t, 500)
	path := writeFixture(t, df, "projection.parquet")

	loaded, err := parquetio.ReadFile(path, &parquetio.Options{
		Columns: []string{"id", "key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.NumCols(); got != 2 {
		t.Fatalf("num cols = %d, want 2 (projected)", got)
	}
	if got := loaded.NumRows(); got != 500 {
		t.Fatalf("num rows = %d, want 500", got)
	}
	names := loaded.ColumnNames()
	if names[0] != "id" || names[1] != "key" {
		t.Fatalf("projected columns = %v, want [id key]", names)
	}
	// value_a must not have leaked through — asking for it should fail.
	if _, err := loaded.Column("value_a"); err == nil {
		t.Fatalf("value_a should not be present in projected frame")
	}
}

func TestReadFile_ColumnProjection_UnknownColumn(t *testing.T) {
	df := makeSyntheticFrame(t, 10)
	path := writeFixture(t, df, "unknown_col.parquet")

	_, err := parquetio.ReadFile(path, &parquetio.Options{
		Columns: []string{"id", "does_not_exist"},
	})
	if err == nil {
		t.Fatal("expected error for unknown column")
	}
	if !errors.Is(err, parquetio.ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Fatalf("error should name the missing column: %v", err)
	}
}

func TestReadFileChunksFunc_MultipleChunks(t *testing.T) {
	// 5000 rows at ChunkRows=1000 should produce multiple chunks whose
	// total row count matches the source.
	df := makeSyntheticFrame(t, 5000)
	path := writeFixture(t, df, "chunks.parquet")

	var chunkCount, totalRows int
	err := parquetio.ReadFileChunksFunc(path, &parquetio.Options{ChunkRows: 1000},
		func(f *gobi.Frame) error {
			chunkCount++
			totalRows += f.NumRows()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if chunkCount < 2 {
		t.Fatalf("chunkCount = %d, want > 1", chunkCount)
	}
	if totalRows != 5000 {
		t.Fatalf("totalRows = %d, want 5000", totalRows)
	}
}

func TestReadFileChunksFunc_CallbackErrorAborts(t *testing.T) {
	df := makeSyntheticFrame(t, 2000)
	path := writeFixture(t, df, "abort.parquet")

	sentinel := errors.New("stop")
	var invocations int
	err := parquetio.ReadFileChunksFunc(path, &parquetio.Options{ChunkRows: 500},
		func(f *gobi.Frame) error {
			invocations++
			if invocations == 2 {
				return sentinel
			}
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected error from callback abort")
	}
	if !errors.Is(err, parquetio.ErrChunksAborted) {
		t.Fatalf("want ErrChunksAborted in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "stop") {
		t.Fatalf("callback error should be wrapped: %v", err)
	}
	if invocations != 2 {
		t.Fatalf("invocations = %d, want 2", invocations)
	}
}

func TestReadFileChunksFunc_DataIntegrity(t *testing.T) {
	// Values must arrive in-order across chunks.
	const n = 3000
	df := makeSyntheticFrame(t, n)
	path := writeFixture(t, df, "integrity.parquet")

	var rowIdx int64
	err := parquetio.ReadFileChunksFunc(path, &parquetio.Options{ChunkRows: 400},
		func(f *gobi.Frame) error {
			idCol, _ := f.Column("id")
			arr := idCol.Column().Data().Chunks()[0].(*array.Int64)
			for i := 0; i < arr.Len(); i++ {
				if arr.Value(i) != rowIdx {
					return fmt.Errorf("row %d id = %d, want %d", rowIdx, arr.Value(i), rowIdx)
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

func TestReadFileChunksFunc_RetainAcrossCallback(t *testing.T) {
	df := makeSyntheticFrame(t, 1500)
	path := writeFixture(t, df, "retain.parquet")

	var kept []*gobi.Frame
	err := parquetio.ReadFileChunksFunc(path, &parquetio.Options{ChunkRows: 400},
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
	// Access the buffers of a retained frame — should not crash after
	// the streaming loop's own Release.
	last := kept[len(kept)-1]
	idCol, _ := last.Column("id")
	arr := idCol.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Len() == 0 {
		t.Fatal("retained frame lost its data")
	}
	for _, f := range kept {
		f.Release()
	}
}

func TestReadFileChunksFunc_ProjectionApplies(t *testing.T) {
	// Streaming + column projection should compose: each yielded frame
	// carries only the requested columns.
	df := makeSyntheticFrame(t, 2000)
	path := writeFixture(t, df, "stream_proj.parquet")

	var seenCols []string
	err := parquetio.ReadFileChunksFunc(path,
		&parquetio.Options{Columns: []string{"key"}, ChunkRows: 500},
		func(f *gobi.Frame) error {
			if seenCols == nil {
				seenCols = f.ColumnNames()
			}
			if f.NumCols() != 1 {
				return fmt.Errorf("batch has %d cols, want 1", f.NumCols())
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(seenCols) != 1 || seenCols[0] != "key" {
		t.Fatalf("projected cols in stream = %v, want [key]", seenCols)
	}
}

func TestReadFileChunksFunc_GeoMetadataPropagates(t *testing.T) {
	// Streaming a file with geometry columns should attach the "geo"
	// file-level metadata to each yielded frame's schema.
	src, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.Options{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := writeFixture(t, src, "geo.parquet")

	var geoOK bool
	err = parquetio.ReadFileChunksFunc(path, nil, func(f *gobi.Frame) error {
		md := f.Schema().Metadata()
		if v, ok := md.GetValue("geo"); ok && strings.Contains(v, `"primary_column":"geometry"`) {
			geoOK = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !geoOK {
		t.Fatal("geo metadata missing from streamed frame")
	}
}

func TestReadFileChunksFunc_EmptyFile(t *testing.T) {
	// A file with zero rows should complete without invoking fn.
	df := makeSyntheticFrame(t, 0)
	path := writeFixture(t, df, "empty.parquet")

	var called int
	err := parquetio.ReadFileChunksFunc(path, nil, func(*gobi.Frame) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("fn called %d times on empty file, want 0", called)
	}
}

package parquetio_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

type city struct {
	Name       string `csv:"name"`
	Population int64  `csv:"population"`
	Geom       string `csv:"geometry" geom:"true"`
}

const citiesCSV = `name,population,geometry
New York,8804190,POINT (-74.0060 40.7128)
Los Angeles,3898747,POINT (-118.2437 34.0522)
Chicago,2746388,POINT (-87.6298 41.8781)
`

func TestParseCodec(t *testing.T) {
	cases := map[string]parquetio.Codec{
		"":       parquetio.CodecUncompressed,
		"NONE":   parquetio.CodecUncompressed,
		"snappy": parquetio.CodecSnappy,
		"Gzip":   parquetio.CodecGzip,
		"gz":     parquetio.CodecGzip,
		"br":     parquetio.CodecBrotli,
		"zstd":   parquetio.CodecZstd,
		"lz4":    parquetio.CodecLZ4,
	}
	for in, want := range cases {
		got, err := parquetio.ParseCodec(in)
		if err != nil {
			t.Errorf("ParseCodec(%q) err: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCodec(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := parquetio.ParseCodec("bogus"); !errors.Is(err, parquetio.ErrUnknownCodec) {
		t.Errorf("expected ErrUnknownCodec, got %v", err)
	}
}

func TestWriteRead_RoundTrip_Snappy(t *testing.T) {
	testRoundTrip(t, parquetio.CodecSnappy)
}

func TestWriteRead_RoundTrip_Gzip(t *testing.T) {
	testRoundTrip(t, parquetio.CodecGzip)
}

func TestWriteRead_RoundTrip_Uncompressed(t *testing.T) {
	testRoundTrip(t, parquetio.CodecUncompressed)
}

func TestWriteRead_PreservesGeoParquetMetadata(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Look for the "geo" file-level metadata key in the loaded schema.
	md := loaded.Schema().Metadata()
	geoRaw, ok := md.GetValue("geo")
	if !ok {
		t.Fatal("geo metadata key missing after round-trip")
	}
	if !strings.Contains(geoRaw, `"primary_column":"geometry"`) {
		t.Fatalf("primary_column not in metadata: %s", geoRaw)
	}
	if !strings.Contains(geoRaw, `"geometry_types":["Point"]`) {
		t.Fatalf("geometry_types missing: %s", geoRaw)
	}
	if !strings.Contains(geoRaw, `"bbox":`) {
		t.Fatalf("bbox missing: %s", geoRaw)
	}
}

func testRoundTrip(t *testing.T, codec parquetio.Codec) {
	t.Helper()
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: codec}); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rows, cols := loaded.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("round-trip shape got (%d, %d), want (3, 3)", rows, cols)
	}
	g, err := loaded.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(geometry.Point); !ok {
		t.Fatalf("expected Point, got %T", g)
	}
}

// TestWrite_ToBuffer confirms the io.Writer-backed entrypoint
// produces bytes that round-trip through ReadReader identically to
// WriteFile. The buffer stand-in mirrors the shape of an S3 uploader,
// a tar entry writer, or any other non-filesystem sink.
func TestWrite_ToBuffer(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := parquetio.Write(df, &buf, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := parquetio.ReadReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()), nil)
	if err != nil {
		t.Fatalf("ReadReader: %v", err)
	}
	rows, cols := loaded.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("round-trip shape got (%d, %d), want (3, 3)", rows, cols)
	}
	geoRaw, ok := loaded.Schema().Metadata().GetValue("geo")
	if !ok {
		t.Fatal("geo metadata key missing after Write→ReadReader round-trip")
	}
	if !strings.Contains(geoRaw, `"primary_column":"geometry"`) {
		t.Fatalf("primary_column not in metadata: %s", geoRaw)
	}
}

// TestReadReader confirms the io.ReaderAt-backed entrypoint reads a
// Parquet payload identically to ReadFile. Writes to a file, slurps
// the bytes, feeds them back via bytes.Reader (which satisfies
// io.ReaderAt). Same round-trip contract as TestWriteRead_RoundTrip_*.
//
// This is the code path athenaio T1 will exercise via s3.GetObject's
// io.ReaderAt-shaped output — the test uses bytes.Reader as a stand-in
// so it can run in CI without touching AWS.
func TestReadReader(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := parquetio.ReadReader(bytes.NewReader(buf), int64(len(buf)), nil)
	if err != nil {
		t.Fatalf("ReadReader: %v", err)
	}
	if rows, cols := loaded.Shape(); rows != 3 || cols != 3 {
		t.Fatalf("shape got (%d, %d), want (3, 3)", rows, cols)
	}
	// Geo metadata should survive the reader-based path just like the
	// path-based one — the file-level KeyValueMetadata is read from
	// the parquet footer regardless of source.
	g, err := loaded.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(geometry.Point); !ok {
		t.Fatalf("expected Point, got %T", g)
	}
}

// TestReadReaderChunksFunc confirms the streaming reader-based path
// behaves like ReadFileChunksFunc — batches arrive, fn is called per
// batch, error propagation works.
func TestReadReaderChunksFunc(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var totalRows int64
	err = parquetio.ReadReaderChunksFunc(
		bytes.NewReader(buf),
		int64(len(buf)),
		nil,
		func(f *gobi.Frame) error {
			r, _ := f.Shape()
			totalRows += int64(r)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ReadReaderChunksFunc: %v", err)
	}
	if totalRows != 3 {
		t.Errorf("streamed row count = %d, want 3", totalRows)
	}
}

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
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestReadFile_ColumnProjection(t *testing.T) {
	df := makeSyntheticFrame(t, 500)
	path := writeFixture(t, df, "projection.parquet")

	loaded, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
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

	_, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
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
	err := parquetio.ReadFileChunksFunc(path, &parquetio.ReadOptions{ChunkRows: 1000},
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
	err := parquetio.ReadFileChunksFunc(path, &parquetio.ReadOptions{ChunkRows: 500},
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
	err := parquetio.ReadFileChunksFunc(path, &parquetio.ReadOptions{ChunkRows: 400},
		func(f *gobi.Frame) error {
			idCol, _ := f.Column("id")
			arr := idCol.Column().Data().Chunks()[0].(*array.Int64)
			for i := range arr.Len() {
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
	err := parquetio.ReadFileChunksFunc(path, &parquetio.ReadOptions{ChunkRows: 400},
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
		&parquetio.ReadOptions{Columns: []string{"key"}, ChunkRows: 500},
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
	src, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
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

// writeNestedSchemaFixture writes a parquet file whose top-level arrow
// fields include a struct-typed column (bbox: struct<xmin, xmax, ymin,
// ymax>) placed between the flat primitive columns. This exercises the
// nested-schema branch of resolveColumns — top-level arrow field index
// no longer matches parquet leaf column index once a nested field
// widens the leaf count. gobi's own writer only emits flat schemas, so
// this helper reaches into pqarrow directly.
//
// Resulting arrow top-level fields:  id, bbox, subtype, geometry
// Resulting parquet leaf columns:    id(0), bbox.xmin(1), bbox.xmax(2),
//
//	bbox.ymin(3), bbox.ymax(4),
//	subtype(5), geometry(6)
func writeNestedSchemaFixture(t *testing.T, path string, n int) {
	t.Helper()
	pool := memory.DefaultAllocator

	bboxType := arrow.StructOf(
		arrow.Field{Name: "xmin", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "xmax", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "ymin", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "ymax", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	)
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "bbox", Type: bboxType, Nullable: false},
		{Name: "subtype", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "geometry", Type: arrow.BinaryTypes.Binary, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	bboxB := array.NewStructBuilder(pool, bboxType)
	defer bboxB.Release()
	subtypeB := array.NewStringBuilder(pool)
	defer subtypeB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()

	xminB := bboxB.FieldBuilder(0).(*array.Float64Builder)
	xmaxB := bboxB.FieldBuilder(1).(*array.Float64Builder)
	yminB := bboxB.FieldBuilder(2).(*array.Float64Builder)
	ymaxB := bboxB.FieldBuilder(3).(*array.Float64Builder)

	for i := range n {
		idB.Append(int64(i))
		bboxB.Append(true)
		xminB.Append(float64(i))
		xmaxB.Append(float64(i) + 1)
		yminB.Append(float64(i) * 2)
		ymaxB.Append(float64(i)*2 + 1)
		subtypeB.Append(fmt.Sprintf("subtype-%d", i%3))
		geomB.Append(fmt.Appendf(nil, "wkb-%d", i))
	}

	arrs := []arrow.Array{
		idB.NewArray(), bboxB.NewArray(), subtypeB.NewArray(), geomB.NewArray(),
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()

	rec := array.NewRecordBatch(schema, arrs, int64(n))
	defer rec.Release()

	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	// pqarrow.FileWriter.Close closes the underlying io.Writer.
	props := parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Snappy))
	w, err := pqarrow.NewFileWriter(schema, out, props, pqarrow.NewArrowWriterProperties())
	if err != nil {
		_ = out.Close()
		t.Fatalf("new file writer: %v", err)
	}
	if err := w.Write(rec); err != nil {
		_ = w.Close()
		t.Fatalf("write record: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
}

// TestReadFile_ColumnProjection_NestedSchema is the regression test for
// the Overture-shaped bug: when a top-level arrow field is nested
// (struct-typed, list-of-struct, etc.), arrow field index N no longer
// equals parquet leaf column index N. Projecting by name across such a
// schema previously returned the wrong columns because resolveColumns
// treated arrow field indices as leaf column indices directly.
func TestReadFile_ColumnProjection_NestedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested.parquet")
	writeNestedSchemaFixture(t, path, 50)

	// Sanity: unprojected read sees all 4 top-level fields.
	full, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("unprojected read: %v", err)
	}
	if got, want := full.ColumnNames(), []string{"id", "bbox", "subtype", "geometry"}; !slicesEqual(got, want) {
		t.Fatalf("unprojected cols = %v, want %v", got, want)
	}
	full.Release()

	// Project past the nested struct: id sits before bbox (flat), while
	// subtype and geometry sit after it — those are exactly the ones the
	// pre-fix arrow-index==leaf-index shortcut would misroute onto bbox
	// child leaves.
	requested := []string{"id", "subtype", "geometry"}
	projected, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Columns: requested})
	if err != nil {
		t.Fatalf("projected read: %v", err)
	}
	defer projected.Release()

	if got := projected.ColumnNames(); !slicesEqual(got, requested) {
		t.Fatalf("projected cols = %v, want %v", got, requested)
	}
	if got, want := projected.NumRows(), 50; got != want {
		t.Fatalf("num rows = %d, want %d", got, want)
	}

	// Data integrity: values in the projected columns must correspond to
	// the correct source columns, not to some other leaf that happened to
	// slot into the same arrow-field-index slot pre-fix.
	idCol, err := projected.Column("id")
	if err != nil {
		t.Fatalf("id column: %v", err)
	}
	subCol, err := projected.Column("subtype")
	if err != nil {
		t.Fatalf("subtype column: %v", err)
	}
	idArr := idCol.Column().Data().Chunks()[0].(*array.Int64)
	subArr := subCol.Column().Data().Chunks()[0].(*array.String)
	for i := range idArr.Len() {
		if idArr.Value(i) != int64(i) {
			t.Fatalf("id[%d] = %d, want %d", i, idArr.Value(i), i)
		}
		want := fmt.Sprintf("subtype-%d", i%3)
		if subArr.Value(i) != want {
			t.Fatalf("subtype[%d] = %q, want %q", i, subArr.Value(i), want)
		}
	}

	// Also verify that a nested top-level field can be selected on its
	// own — its child leaves need to travel together.
	bboxOnly, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Columns: []string{"bbox"}})
	if err != nil {
		t.Fatalf("nested-only read: %v", err)
	}
	defer bboxOnly.Release()
	if got := bboxOnly.ColumnNames(); !slicesEqual(got, []string{"bbox"}) {
		t.Fatalf("nested-only cols = %v, want [bbox]", got)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeGeoParquetFileLevelOnly writes a parquet file whose geometry
// column is declared solely via the file-level "geo" JSON blob — no
// per-field arrow metadata on the geometry column itself. This
// reproduces the Overture / geopandas / DuckDB-spatial shape, where
// GeoParquet 1.1 file-level metadata is the only signal that a column
// is a WKB geometry. gobi's writer stamps per-field metadata on top of
// the file-level blob, so exercising the reader's file-level-only path
// requires reaching past it.
func writeGeoParquetFileLevelOnly(t *testing.T, path string) {
	t.Helper()
	pool := memory.DefaultAllocator

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		// No metadata on `geometry` — the file-level blob is the only
		// declaration that this column holds WKB.
		{Name: "geometry", Type: arrow.BinaryTypes.Binary, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for i := range 5 {
		idB.Append(int64(i))
		geomB.Append(fmt.Appendf(nil, "wkb-%d", i))
	}
	arrs := []arrow.Array{idB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	rec := array.NewRecordBatch(schema, arrs, 5)
	defer rec.Release()

	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	// pqarrow.FileWriter.Close closes the underlying io.Writer.
	props := parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Snappy))
	w, err := pqarrow.NewFileWriter(schema, out, props, pqarrow.NewArrowWriterProperties())
	if err != nil {
		_ = out.Close()
		t.Fatalf("new file writer: %v", err)
	}
	if err := w.Write(rec); err != nil {
		_ = w.Close()
		t.Fatalf("write record: %v", err)
	}
	// GeoParquet 1.1-style file-level metadata. crs is null → readers
	// treat as OGC:CRS84 == EPSG:4326 for planar WKB.
	geoBlob := `{"version":"1.1.0","primary_column":"geometry","columns":{"geometry":{"encoding":"WKB","geometry_types":["Point"],"crs":null}}}`
	if err := w.AppendKeyValueMetadata(gobi.GeoParquetMetadataKey, geoBlob); err != nil {
		_ = w.Close()
		t.Fatalf("append geo metadata: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
}

// TestReadFile_GeoParquet_RecognizesFileLevelMetadata verifies that
// reading a GeoParquet 1.1 file whose only geometry declaration lives
// in the file-level "geo" JSON (no per-field arrow metadata) still
// yields a Frame whose Series.IsGeometry() reports true. Regression
// against the Overture bug: the projection now returns the right
// column names, but callers previously couldn't run any geometry-aware
// operator on the column because the field-level tag IsGeometry checks
// wasn't present.
func TestReadFile_GeoParquet_RecognizesFileLevelMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geo_file_level.parquet")
	writeGeoParquetFileLevelOnly(t, path)

	df, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer df.Release()

	geom, err := df.Column("geometry")
	if err != nil {
		t.Fatalf("column geometry: %v", err)
	}
	if !geom.IsGeometry() {
		t.Fatalf("geometry column not recognised: field metadata = %+v",
			df.Schema().Field(1).Metadata)
	}

	// Column projection must preserve the tag when the geometry column
	// is projected — Overture callers reach the column via a Columns
	// projection, not the full-file read.
	proj, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		Columns: []string{"geometry"},
	})
	if err != nil {
		t.Fatalf("projected read: %v", err)
	}
	defer proj.Release()
	pg, err := proj.Column("geometry")
	if err != nil {
		t.Fatalf("projected geometry column: %v", err)
	}
	if !pg.IsGeometry() {
		t.Fatalf("projected geometry column not recognised")
	}
}

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

// TestWrite_HilbertSortZonedTimestamp — Concat of streamed batches
// keeps one chunk per batch; HilbertSort must be able to permute
// timestamp[us, tz=UTC], timestamp[ns, tz=UTC] and naive
// timestamp[ns] columns across those chunks.
func TestWrite_HilbertSortZonedTimestamp(t *testing.T) {
	pts := pointFrame(t, []float64{50, 0, 25, 10}, []float64{5, 0, 2, 1}, 0)
	defer pts.Release()
	times := []time.Time{time.Unix(50, 0), time.Unix(0, 0), time.Unix(25, 0), time.Unix(10, 0)}
	ts, err := gobi.NewTimestampSeriesUnit("t", times, nil, arrow.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	// The two nanosecond shapes: zoned (e.g. Hive INT96 read back) and
	// naive (time.Time through FromStructs / NewTimestampSeries).
	nsUTC, err := gobi.NewTimestampSeriesUnit("t_ns_utc", times, nil, arrow.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	nsNaive := gobi.NewTimestampSeries("t_ns", times, nil)
	df := pts
	for _, s := range []gobi.Series{ts, nsUTC, nsNaive} {
		next, err := df.WithColumn(s.Name(), s)
		if err != nil {
			t.Fatal(err)
		}
		if df != pts {
			df.Release()
		}
		df = next
	}
	defer df.Release()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.parquet")
	if err := parquetio.WriteFile(df, in, &parquetio.WriteOptions{RowGroupRows: 1}); err != nil {
		t.Fatal(err)
	}
	var batches []*gobi.Frame
	if err := parquetio.ReadFileChunksFunc(in, &parquetio.ReadOptions{ChunkRows: 1}, func(f *gobi.Frame) error {
		f.Retain()
		batches = append(batches, f)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	back, err := gobi.Concat(batches...)
	for _, b := range batches {
		b.Release()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer back.Release()
	if c, _ := back.Column("t"); len(c.Column().Data().Chunks()) < 2 {
		t.Fatalf("want a multi-chunk column, got %d chunk(s)", len(c.Column().Data().Chunks()))
	}
	out := filepath.Join(dir, "out.parquet")
	if err := parquetio.WriteFile(back, out, &parquetio.WriteOptions{HilbertSort: true}); err != nil {
		t.Fatalf("HilbertSort write: %v", err)
	}
	sorted, err := parquetio.ReadFile(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sorted.Release()
	lon, _ := sorted.Column("lon")
	xs, _ := lon.Float64s()
	for _, name := range []string{"t", "t_ns_utc", "t_ns"} {
		tc, _ := sorted.Column(name)
		got, _, err := tc.AsTimes()
		if err != nil {
			t.Fatal(err)
		}
		for i := range xs {
			if int64(xs[i]) != got[i].Unix() {
				t.Errorf("%s row %d: lon %v, t %v — rows split apart", name, i, xs[i], got[i].Unix())
			}
		}
	}
	for name, want := range map[string]string{"t": "timestamp[us, tz=UTC]", "t_ns_utc": "timestamp[ns, tz=UTC]", "t_ns": "timestamp[ns]"} {
		if c, _ := back.Column(name); c.DataType().String() != want || len(c.Column().Data().Chunks()) < 2 {
			t.Errorf("%s: %s in %d chunk(s), want multi-chunk %s", name, c.DataType(), len(c.Column().Data().Chunks()), want)
		}
	}
}

// TestIsIn_RowGroupPruning — Predicate IsIn skips row groups whose
// min/max holds none of the values, and a lazy Filter(IsIn) still
// returns exactly the matching rows.
func TestIsIn_RowGroupPruning(t *testing.T) {
	ids := make([]int64, 10)
	for i := range ids {
		ids[i] = int64(i)
	}
	df, err := gobi.NewFrameFromSeries(gobi.NewInt64Series("id", ids, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	path := filepath.Join(t.TempDir(), "ids.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{RowGroupRows: 2}); err != nil {
		t.Fatal(err)
	}

	// Predicate only prunes: rows 2,3 (group of 3) and 8,9 (group of 9).
	pruned, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Predicate: gobi.Col("id").IsIn(3, 9, 100)})
	if err != nil {
		t.Fatal(err)
	}
	defer pruned.Release()
	c, _ := pruned.Column("id")
	if v, _ := c.Int64s(); !slices.Equal(v, []int64{2, 3, 8, 9}) {
		t.Errorf("pruned read = %v, want row groups [2 3] and [8 9]", v)
	}

	got, err := parquetio.ScanFile(path, nil).Filter(gobi.Col("id").IsIn([]int64{3, 9, 100})).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer got.Release()
	c, _ = got.Column("id")
	v, _ := c.Int64s()
	slices.Sort(v) // the parallel scan returns batches in no fixed order
	if !slices.Equal(v, []int64{3, 9}) {
		t.Errorf("lazy filter = %v, want [3 9]", v)
	}
}

// TestIsIn_PruningUnsignedAndFloat32 — real Parquet stats: uint32
// above MaxInt32 (stored as a negative int32 bit pattern) and float32
// 0.1 (widened to 0.100000001…) must not prune matching row groups.
func TestIsIn_PruningUnsignedAndFloat32(t *testing.T) {
	pool := memory.DefaultAllocator
	ub := array.NewUint32Builder(pool)
	ub.AppendValues([]uint32{5, 3_000_000_000}, nil)
	fb := array.NewFloat32Builder(pool)
	fb.AppendValues([]float32{0.1, 0.1}, nil)
	df, err := gobi.NewFrameFromSeries(
		gobi.SeriesFromArray(arrow.Field{Name: "u", Type: arrow.PrimitiveTypes.Uint32, Nullable: true}, ub.NewArray()),
		gobi.SeriesFromArray(arrow.Field{Name: "f", Type: arrow.PrimitiveTypes.Float32, Nullable: true}, fb.NewArray()),
	)
	ub.Release()
	fb.Release()
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	path := filepath.Join(t.TempDir(), "uf.parquet")
	if err := parquetio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	for name, pred := range map[string]gobi.Expr{
		"uint32 5":      gobi.Col("u").IsIn(uint32(5)),
		"uint32 3e9":    gobi.Col("u").IsIn(int64(3_000_000_000)),
		"uint32 Eq 5":   gobi.Col("u").Eq(gobi.Lit(int64(5))),
		"float32 0.1":   gobi.Col("f").IsIn(0.1),
		"float32 NaN+1": gobi.Col("f").IsIn(math.NaN(), 1.0),
	} {
		got, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Predicate: pred})
		if err != nil {
			t.Fatal(err)
		}
		if got.NumRows() != 2 {
			t.Errorf("%s: row group pruned (%d rows read)", name, got.NumRows())
		}
		got.Release()
	}
	// And a value outside the range still prunes.
	got, err := parquetio.ReadFile(path, &parquetio.ReadOptions{Predicate: gobi.Col("u").IsIn(4)})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Release()
	if got.NumRows() != 0 {
		t.Errorf("IsIn(4): %d rows read, want the group pruned", got.NumRows())
	}
}

// TestScanFile_SelectMissingColumn — a scan with projection pushdown
// reports a missing column from Collect instead of panicking while the
// plan is built.
func TestScanFile_SelectMissingColumn(t *testing.T) {
	df, err := gobi.NewFrameFromSeries(gobi.NewInt64Series("a", []int64{1, 2}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	path := filepath.Join(t.TempDir(), "a.parquet")
	if err := parquetio.WriteFile(df, path, nil); err != nil {
		t.Fatal(err)
	}
	for name, lf := range map[string]*gobi.LazyFrame{
		"SelectCols":   parquetio.ScanFile(path, nil).SelectCols("a", "nope"),
		"after Filter": parquetio.ScanFile(path, nil).Filter(gobi.Col("a").Gt(gobi.Lit(int64(0)))).SelectCols("nope"),
	} {
		if _, err := lf.Collect(); !errors.Is(err, gobi.ErrColumnNotFound) {
			t.Errorf("%s: err = %v, want ErrColumnNotFound", name, err)
		}
	}
}

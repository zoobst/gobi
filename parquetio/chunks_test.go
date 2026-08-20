package parquetio_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

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

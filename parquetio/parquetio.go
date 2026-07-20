// Package parquetio reads and writes gobi Frames as Apache Parquet.
//
// Compression is delegated to Parquet's built-in codecs. When a Frame
// contains geometry columns, WriteFile emits a GeoParquet 1.1 metadata
// blob under the Parquet file-level "geo" key; ReadFile re-hydrates it
// into the returned Frame's schema.
//
// The reader offers two entry points:
//
//   - ReadFile materializes the whole file as a single Frame. Peak memory
//     is roughly the file's decompressed size. Good for small/medium files
//     where you want the whole dataset at once.
//
//   - ReadFileChunksFunc streams the file as record-batch-sized Frames.
//     Only one batch's arrow buffers are live at a time, so peak memory
//     is bounded regardless of source file size. Good for ETL / bounded-
//     memory pipelines.
//
// Both entry points accept an Options.Columns list to project the read
// to a subset of columns. Projected-away columns are neither fetched
// from disk nor decompressed nor materialized into arrow arrays.
package parquetio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/zoobst/gobi"
)

// Codec identifies a Parquet-level compression codec.
type Codec string

const (
	CodecUncompressed Codec = "uncompressed"
	CodecSnappy       Codec = "snappy"
	CodecGzip         Codec = "gzip"
	CodecBrotli       Codec = "brotli"
	CodecLZ4          Codec = "lz4"
	CodecZstd         Codec = "zstd"
)

// DefaultChunkRows is the arrow record-batch size used by
// ReadFileChunksFunc when Options.ChunkRows is 0.
const DefaultChunkRows int64 = 64 * 1024

// Errors.
var (
	ErrUnknownCodec   = errors.New("parquetio: unknown compression codec")
	ErrColumnNotFound = errors.New("parquetio: column not found")
	ErrChunksAborted  = errors.New("parquetio: chunk callback returned error")
)

// Options controls parquet read behavior. A nil pointer is treated as
// the zero value.
type Options struct {
	// Columns projects the file to a subset of top-level columns by
	// name. nil or empty = read all columns.
	//
	// Names not present in the file's schema return ErrColumnNotFound.
	// Column projection is applied at the parquet reader layer: the
	// excluded columns are never fetched, decompressed, or materialized
	// into arrow arrays. The savings scale with how large those columns
	// are relative to the file — narrow analytical files where the caller
	// wants a few columns out of many benefit most.
	Columns []string

	// ChunkRows is the arrow record-batch size used by
	// ReadFileChunksFunc. Each RecordReader.Next() call produces at
	// most ChunkRows rows. 0 = DefaultChunkRows. Ignored by ReadFile.
	//
	// Sub-partitioning a row group into fixed-size batches is what
	// bounds streaming memory to ~one batch at a time regardless of
	// the file's row-group sizes.
	ChunkRows int64

	// Allocator overrides the Arrow allocator. nil = memory.DefaultAllocator.
	Allocator memory.Allocator
}

// ParseCodec resolves a codec by name (case-insensitive). Empty and "none"
// map to CodecUncompressed.
func ParseCodec(s string) (Codec, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "uncompressed":
		return CodecUncompressed, nil
	case "snappy":
		return CodecSnappy, nil
	case "gzip", "gz":
		return CodecGzip, nil
	case "brotli", "br":
		return CodecBrotli, nil
	case "lz4":
		return CodecLZ4, nil
	case "zstd":
		return CodecZstd, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownCodec, s)
	}
}

func (c Codec) toArrow() (compress.Compression, error) {
	switch c {
	case CodecUncompressed:
		return compress.Codecs.Uncompressed, nil
	case CodecSnappy:
		return compress.Codecs.Snappy, nil
	case CodecGzip:
		return compress.Codecs.Gzip, nil
	case CodecBrotli:
		return compress.Codecs.Brotli, nil
	case CodecLZ4:
		return compress.Codecs.Lz4Raw, nil
	case CodecZstd:
		return compress.Codecs.Zstd, nil
	default:
		return compress.Codecs.Uncompressed, fmt.Errorf("%w: %q", ErrUnknownCodec, c)
	}
}

// ReadFile reads path into a single Frame. If opts.Columns is non-empty,
// only those columns are fetched + decoded. If the file has a GeoParquet
// "geo" key, it is re-attached to the Frame's Arrow schema so downstream
// code can detect geometry columns.
func ReadFile(path string, opts *Options) (*gobi.Frame, error) {
	rc, err := openReader(path, opts)
	if err != nil {
		return nil, err
	}
	defer rc.close()

	table, err := rc.reader.ReadRowGroups(context.Background(), rc.colIndices, rc.rowGroups)
	if err != nil {
		return nil, err
	}
	return frameFromTable(table, rc.geoRaw)
}

// ReadFileChunksFunc streams path as record-batch-sized Frames. fn is
// invoked once per batch (~DefaultChunkRows rows by default; override
// via Options.ChunkRows). Only the current batch's arrow buffers are
// in memory, so peak footprint is bounded to roughly one batch.
//
// The Frame handed to fn is Released after fn returns. To retain a Frame
// past the callback, call frame.Retain() inside fn and match with a
// frame.Release() when you're done with it.
//
// If fn returns an error, iteration stops and the error is wrapped in
// ErrChunksAborted so callers can errors.Is / errors.As it. Underlying
// parquet read errors are returned directly.
func ReadFileChunksFunc(path string, opts *Options, fn func(*gobi.Frame) error) error {
	rc, err := openReader(path, opts)
	if err != nil {
		return err
	}
	defer rc.close()

	rr, err := rc.reader.GetRecordReader(context.Background(), rc.colIndices, rc.rowGroups)
	if err != nil {
		return fmt.Errorf("parquetio: build record reader: %w", err)
	}
	defer rr.Release()

	for rr.Next() {
		rec := rr.RecordBatch()
		frame, err := frameFromRecord(rec, rc.geoRaw)
		if err != nil {
			return err
		}
		cbErr := fn(frame)
		frame.Release()
		if cbErr != nil {
			return fmt.Errorf("%w: %v", ErrChunksAborted, cbErr)
		}
	}
	if err := rr.Err(); err != nil {
		return fmt.Errorf("parquetio: %w", err)
	}
	return nil
}

// WriteFile writes f to path with the requested codec. If f contains any
// geometry columns, the output includes a GeoParquet 1.1 metadata blob
// under the file-level "geo" key.
func WriteFile(f *gobi.Frame, path string, codec Codec) error {
	compression, err := codec.toArrow()
	if err != nil {
		return err
	}
	meta, err := gobi.BuildGeoParquetMetadata(f)
	if err != nil {
		return err
	}

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	writer, err := pqarrow.NewFileWriter(
		f.Schema(),
		out,
		parquet.NewWriterProperties(parquet.WithCompression(compression)),
		pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()),
	)
	if err != nil {
		return err
	}
	if err := writer.WriteTable(f.Table(), int64(f.NumRows())); err != nil {
		_ = writer.Close()
		return err
	}
	if meta != nil {
		blob, err := marshalGeoMeta(meta)
		if err != nil {
			_ = writer.Close()
			return err
		}
		if err := writer.AppendKeyValueMetadata(gobi.GeoParquetMetadataKey, blob); err != nil {
			_ = writer.Close()
			return err
		}
	}
	return writer.Close()
}

// -----------------------------------------------------------------------------
// Shared reader setup
// -----------------------------------------------------------------------------

// readerContext holds the opened parquet file + arrow reader + resolved
// column and row-group indices, shared by ReadFile and
// ReadFileChunksFunc. Callers must invoke close() when done.
//
// colIndices and rowGroups are always explicit slices, never nil.
// pqarrow.FileReader.ReadRowGroups treats nil as "read nothing," unlike
// GetRecordReader which treats nil as "read everything," so we always
// pass concrete lists to keep both paths symmetric.
type readerContext struct {
	file        *os.File
	parquetFile *file.Reader
	reader      *pqarrow.FileReader
	colIndices  []int
	rowGroups   []int
	geoRaw      string
}

func (rc *readerContext) close() {
	if rc.parquetFile != nil {
		_ = rc.parquetFile.Close()
	}
	if rc.file != nil {
		_ = rc.file.Close()
	}
}

// openReader opens path, builds a pqarrow.FileReader with a batch size
// suitable for streaming, extracts geo metadata, and resolves
// opts.Columns to arrow-schema indices.
func openReader(path string, opts *Options) (*readerContext, error) {
	if opts == nil {
		opts = &Options{}
	}
	pool := opts.Allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	pf, err := file.NewParquetReader(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	geoRaw := ""
	if kv := pf.MetaData().KeyValueMetadata(); kv != nil {
		if v := kv.FindValue(gobi.GeoParquetMetadataKey); v != nil {
			geoRaw = *v
		}
	}

	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{
		Parallel:  true,
		BatchSize: chunkRows(opts),
	}, pool)
	if err != nil {
		_ = pf.Close()
		_ = f.Close()
		return nil, err
	}

	colIndices, err := resolveColumns(pf, fr, opts.Columns)
	if err != nil {
		_ = pf.Close()
		_ = f.Close()
		return nil, err
	}

	rowGroups := make([]int, pf.NumRowGroups())
	for i := range rowGroups {
		rowGroups[i] = i
	}

	return &readerContext{
		file:        f,
		parquetFile: pf,
		reader:      fr,
		colIndices:  colIndices,
		rowGroups:   rowGroups,
		geoRaw:      geoRaw,
	}, nil
}

// resolveColumns maps opts.Columns (names) to leaf-parquet-column indices
// for GetRecordReader / ReadRowGroups. When names is empty, returns an
// explicit "all indices" slice — nil would work for GetRecordReader but
// ReadRowGroups treats nil as "no columns," so we always emit a
// concrete list to keep both paths symmetric.
//
// Parquet flat schemas (all top-level primitives) have arrow-field-index
// == parquet-leaf-column-index. Nested types would need a different
// mapping; gobi currently only emits flat schemas.
func resolveColumns(pf *file.Reader, fr *pqarrow.FileReader, names []string) ([]int, error) {
	numLeaves := pf.MetaData().Schema.NumColumns()
	if len(names) == 0 {
		all := make([]int, numLeaves)
		for i := range all {
			all[i] = i
		}
		return all, nil
	}
	arrowSchema, err := fr.Schema()
	if err != nil {
		return nil, fmt.Errorf("parquetio: read arrow schema: %w", err)
	}
	nameToIdx := make(map[string]int, len(arrowSchema.Fields()))
	for i, field := range arrowSchema.Fields() {
		nameToIdx[field.Name] = i
	}
	out := make([]int, 0, len(names))
	for _, name := range names {
		idx, ok := nameToIdx[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
		}
		out = append(out, idx)
	}
	return out, nil
}

func chunkRows(opts *Options) int64 {
	if opts != nil && opts.ChunkRows > 0 {
		return opts.ChunkRows
	}
	return DefaultChunkRows
}

// -----------------------------------------------------------------------------
// Frame construction
// -----------------------------------------------------------------------------

// frameFromTable wraps table's columns in a Frame, attaching the geo
// metadata blob to the schema if present.
func frameFromTable(table arrow.Table, geoRaw string) (*gobi.Frame, error) {
	schema := table.Schema()
	if geoRaw != "" {
		var err error
		schema, err = attachGeoKey(schema, geoRaw)
		if err != nil {
			return nil, err
		}
	}
	cols := make([]arrow.Column, table.NumCols())
	for i := int64(0); i < table.NumCols(); i++ {
		cols[i] = *table.Column(int(i))
	}
	return gobi.NewFrame(schema, cols)
}

// frameFromRecord wraps one record batch's arrays in a Frame. Uses
// arrow.NewColumnFromArr, which Retains each array once — so the Frame
// owns its refs and the source record can be Released independently.
func frameFromRecord(rec arrow.RecordBatch, geoRaw string) (*gobi.Frame, error) {
	schema := rec.Schema()
	if geoRaw != "" {
		var err error
		schema, err = attachGeoKey(schema, geoRaw)
		if err != nil {
			return nil, err
		}
	}
	n := int(rec.NumCols())
	cols := make([]arrow.Column, n)
	for i := range n {
		cols[i] = arrow.NewColumnFromArr(schema.Field(i), rec.Column(i))
	}
	return gobi.NewFrame(schema, cols)
}

// marshalGeoMeta serializes the metadata blob. Kept here (rather than
// exposed on gobi) so the JSON layout stays a parquetio implementation
// detail.
func marshalGeoMeta(meta *gobi.GeoParquetMetadata) (string, error) {
	return gobi.MarshalGeoParquetMetadata(meta)
}

// attachGeoKey returns schema with the "geo" file-level metadata key set
// to raw.
func attachGeoKey(schema *arrow.Schema, raw string) (*arrow.Schema, error) {
	keys := []string{gobi.GeoParquetMetadataKey}
	values := []string{raw}
	if schema.HasMetadata() {
		old := schema.Metadata()
		for i, k := range old.Keys() {
			if k == gobi.GeoParquetMetadataKey {
				continue
			}
			keys = append(keys, k)
			values = append(values, old.Values()[i])
		}
	}
	md := arrow.NewMetadata(keys, values)
	return arrow.NewSchema(schema.Fields(), &md), nil
}

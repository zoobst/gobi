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
// Both entry points accept an ReadOptions.Columns list to project the read
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
// ReadFileChunksFunc when ReadOptions.ChunkRows is 0.
const DefaultChunkRows = 64 * 1024

// Errors.
var (
	ErrUnknownCodec   = errors.New("parquetio: unknown compression codec")
	ErrColumnNotFound = errors.New("parquetio: column not found")
	ErrChunksAborted  = errors.New("parquetio: chunk callback returned error")
)

// ReadOptions controls parquet read behavior. A nil pointer is treated as
// the zero value.
type ReadOptions struct {
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
	ChunkRows int

	// Allocator overrides the Arrow allocator. nil = memory.DefaultAllocator.
	Allocator memory.Allocator

	// Predicate is a hint from the optimizer for row-group skipping.
	// When set, ReadFile / ReadFileChunksFunc walk each row-group's
	// footer statistics and skip whole groups whose (min, max) bounds
	// prove no row could satisfy the predicate. The Filter operation
	// above the read still runs — this is a coarse fast-path that
	// avoids fetching irrelevant row-groups off disk.
	//
	// Predicates only prune when they reference columns present in
	// the file's schema. Unrecognized columns silently prevent
	// pruning (conservative — a "maybe" survives). Uses the same
	// Expr type as Frame.FilterExpr.
	Predicate gobi.Expr

	// RowGroups optionally restricts the read to a specific set of
	// row-group indices. When set, ReadFile / ReadFileChunksFunc /
	// ScanFile process only those row-groups; when nil or empty,
	// all row-groups are read.
	//
	// Primarily used internally to partition scans across parallel
	// workers (see ScanWorkers) — each worker gets a disjoint
	// RowGroups slice. Callers can set it directly for very
	// targeted reads (e.g. "just the last row-group of this file"),
	// though the more common way to restrict is via Columns or
	// Predicate.
	RowGroups []int

	// ScanWorkers controls row-group-level parallelism for ScanFile.
	// 0 (default) = runtime.GOMAXPROCS(0), capped at NumRowGroups.
	// 1 = single-threaded (the previous behavior). n > 1 = n
	// workers, capped at NumRowGroups.
	//
	// Ignored by ReadFile (always single-threaded — reads one whole
	// file) and by ReadFileChunksFunc (also single-threaded — the
	// callback API is fundamentally serial). Applies only when the
	// scan flows through the Layer 6 executor via ScanFile +
	// LazyFrame.Collect.
	ScanWorkers int
}

// WriteOptions controls parquet write behavior. A nil pointer is
// treated as the zero value (CodecSnappy + parquet-arrow's default
// row-group sizing).
type WriteOptions struct {
	// Codec selects the Parquet page compression codec. Empty string
	// defaults to CodecSnappy — matches parquet-arrow's own default
	// and is the common choice for good balance between size and
	// decode speed.
	Codec Codec

	// RowGroupRows caps the maximum number of rows per row group. 0
	// uses parquet-arrow's default (~1M rows).
	//
	// Smaller row groups → more granular predicate pushdown (readers
	// can skip whole groups via rowgroup statistics) and lower peak
	// memory when streaming one group at a time. Larger row groups →
	// better compression ratios and less per-group metadata overhead.
	// 64k–256k is a reasonable range for analytical workloads that
	// filter on min/max stats; leave at 0 for archive/bulk-load files
	// where read patterns are full-scan.
	RowGroupRows int64

	// BloomFilterColumns names columns that should have a bloom
	// filter attached to each row group. High-cardinality equality-
	// filtered columns (user IDs, hashes, categorical keys) benefit
	// most; skew-free min/max distributions do not — parquet's row-
	// group statistics already handle those.
	//
	// gobi's own reader does not yet consume bloom filters for row-
	// group skipping (that lands with the query optimizer). Files
	// produced here are still consumed correctly by DuckDB, Spark,
	// Polars, and pyarrow, which do use bloom filters for predicate
	// pushdown on equality filters.
	BloomFilterColumns []string

	// BloomFilterFPP is the target false-positive probability for
	// the bloom filters written above. 0 uses arrow-go's default
	// (0.05). Lower FPP → larger filter on disk; reasonable range
	// 0.01–0.1. Ignored when BloomFilterColumns is empty.
	BloomFilterFPP float64
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

// ReadSchema opens path, reads just the parquet footer, and returns
// the arrow schema of the file — projected through opts.Columns and
// stamped with the GeoParquet "geo" metadata if present.
//
// Reads no column data. Used by ScanFile to populate a lazy plan
// node's output schema without materializing any rows.
func ReadSchema(path string, opts *ReadOptions) (*arrow.Schema, error) {
	rc, err := openReader(path, opts)
	if err != nil {
		return nil, err
	}
	defer rc.close()

	arrowSchema, err := rc.reader.Schema()
	if err != nil {
		return nil, err
	}

	// If Columns was set, project the schema to just those fields.
	if len(opts.readColumns()) > 0 {
		nameToIdx := make(map[string]int, len(arrowSchema.Fields()))
		for i, f := range arrowSchema.Fields() {
			nameToIdx[f.Name] = i
		}
		projected := make([]arrow.Field, 0, len(opts.readColumns()))
		for _, name := range opts.readColumns() {
			if i, ok := nameToIdx[name]; ok {
				projected = append(projected, arrowSchema.Field(i))
			}
		}
		arrowSchema = arrow.NewSchema(projected, schemaMetadataPtr(arrowSchema))
	}

	// Attach the "geo" key if the file carried one.
	if rc.geoRaw != "" {
		return attachGeoKey(arrowSchema, rc.geoRaw)
	}
	return arrowSchema, nil
}

// ScanFile returns a LazyFrame anchored at a parquet scan. No data
// is read until Collect() is called; the schema is read eagerly from
// the parquet footer so downstream nodes can propagate types.
//
// If the file can't be opened at construction (missing file, bad
// footer, unknown codec), the returned LazyFrame still builds — the
// error surfaces at Collect. This matches DuckDB's / Polars'
// `scan_parquet` semantics: cheap to compose, errors bubble at
// materialization.
//
// Composes with the LazyFrame chain: Filter, Select, WithColumn,
// SortBy, GroupBy.Agg, Join, Limit, Head, Tail, DropColumn.
//
// A future optimizer will push Filter and Select nodes above the
// scan back INTO the parquet reader (predicate + projection
// pushdown, bloom-filter-driven rowgroup skipping). Today ScanFile is
// pure API shape — it reads the whole file at Collect regardless of
// what's above it.
func ScanFile(path string, opts *ReadOptions) *gobi.LazyFrame {
	// Try to read the schema eagerly. If that fails, the read
	// closure below will surface the same error at Collect time.
	sch, schemaErr := ReadSchema(path, opts)

	label := buildScanLabel(path, opts)

	node := gobi.NewScanNode(label, sch, func() (*gobi.Frame, error) {
		if schemaErr != nil {
			return nil, schemaErr
		}
		return ReadFile(path, opts)
	}, gobi.WithColumnProjection(func(cols []string) gobi.LogicalPlan {
		// Called by the optimizer's projection-pushdown rule. If
		// the caller already restricted columns explicitly, keep
		// their choice — the optimizer's set is derived from what
		// the plan actually uses, but user intent wins.
		//
		// If no user projection is set, produce a new ScanFile
		// with ReadOptions.Columns = cols. The recursive ScanFile
		// terminates because the new node has cols set, so the
		// next optimizer pass won't project it again.
		if len(opts.readColumns()) > 0 {
			return nil // treated as "no change" by ProjectColumns caller
		}
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		newOpts.Columns = cols
		return ScanFile(path, &newOpts).Plan()
	}), gobi.WithStreamRead(func(cb func(*gobi.Frame) error) error {
		if schemaErr != nil {
			return schemaErr
		}
		return ReadFileChunksFunc(path, opts, cb)
	}), gobi.WithParallelStreamReads(func() []func(cb func(*gobi.Frame) error) error {
		// Only produce a parallel plan if we actually have >1
		// worker's worth of work to do. otherwise nil signals
		// fallback to the serial WithStreamRead callback.
		if schemaErr != nil {
			return nil
		}
		return partitionRowGroups(path, opts)
	}), gobi.WithPredicatePushdown(func(pred gobi.Expr) gobi.LogicalPlan {
		// Called by the optimizer's PushPredicateToScan rule.
		// Layered atop any existing predicate via AND — a caller-
		// supplied Predicate stays applied, and the optimizer's
		// contribution is added on top.
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		if newOpts.Predicate.Node() == nil {
			newOpts.Predicate = pred
		} else {
			newOpts.Predicate = newOpts.Predicate.And(pred)
		}
		return ScanFile(path, &newOpts).Plan()
	}))
	return gobi.NewLazyFrame(node)
}

// buildScanLabel produces the human-readable Scan[parquet](...) label
// used in Explain output. Includes column projection and predicate
// pushdown state so it's obvious from Explain what the scan sees.
func buildScanLabel(path string, opts *ReadOptions) string {
	label := fmt.Sprintf("Scan[parquet](%q)", path)
	if opts == nil {
		return label
	}
	if len(opts.Columns) > 0 && opts.Predicate.Node() != nil {
		return fmt.Sprintf("Scan[parquet](%q, cols=%v, pred=%s)",
			path, opts.Columns, opts.Predicate)
	}
	if len(opts.Columns) > 0 {
		return fmt.Sprintf("Scan[parquet](%q, cols=%v)", path, opts.Columns)
	}
	if opts.Predicate.Node() != nil {
		return fmt.Sprintf("Scan[parquet](%q, pred=%s)", path, opts.Predicate)
	}
	return label
}

// readColumns returns opts.Columns, treating a nil *ReadOptions as empty.
// Used by ReadSchema and ScanFile without repeated nil checks.
func (o *ReadOptions) readColumns() []string {
	if o == nil {
		return nil
	}
	return o.Columns
}

// schemaMetadataPtr mirrors the helper of the same name in gobi/plan.go,
// re-declared here to avoid pulling in the whole package for one line.
func schemaMetadataPtr(s *arrow.Schema) *arrow.Metadata {
	if s == nil || !s.HasMetadata() {
		return nil
	}
	m := s.Metadata()
	return &m
}

// ReadFile reads path into a single Frame. If opts.Columns is non-empty,
// only those columns are fetched + decoded. If the file has a GeoParquet
// "geo" key, it is re-attached to the Frame's Arrow schema so downstream
// code can detect geometry columns.
func ReadFile(path string, opts *ReadOptions) (*gobi.Frame, error) {
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
// via ReadOptions.ChunkRows). Only the current batch's arrow buffers are
// in memory, so peak footprint is bounded to roughly one batch.
//
// The Frame handed to fn is Released after fn returns. To retain a Frame
// past the callback, call frame.Retain() inside fn and match with a
// frame.Release() when you're done with it.
//
// If fn returns an error, iteration stops and the error is wrapped in
// ErrChunksAborted so callers can errors.Is / errors.As it. Underlying
// parquet read errors are returned directly.
func ReadFileChunksFunc(path string, opts *ReadOptions, fn func(*gobi.Frame) error) error {
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

// WriteFile writes f to path. A nil opts uses defaults:
// CodecSnappy compression and parquet-arrow's default row-group
// sizing (~1M rows).
//
// If f contains any geometry columns, the output includes a
// GeoParquet 1.1 metadata blob under the file-level "geo" key.
//
// Tuning row-group size matters for readers that use rowgroup
// statistics for predicate pushdown or that stream one rowgroup at a
// time. Smaller groups → more granular filter skipping and lower
// per-batch memory; larger groups → better compression ratios and
// less per-group overhead. The parquet default is a reasonable
// starting point for most workloads.
func WriteFile(f *gobi.Frame, path string, opts *WriteOptions) error {
	if opts == nil {
		opts = &WriteOptions{}
	}
	codec := opts.Codec
	if codec == "" {
		codec = CodecSnappy
	}
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

	writerProps := []parquet.WriterProperty{parquet.WithCompression(compression)}
	if opts.RowGroupRows > 0 {
		writerProps = append(writerProps, parquet.WithMaxRowGroupLength(opts.RowGroupRows))
	}
	if len(opts.BloomFilterColumns) > 0 {
		if opts.BloomFilterFPP > 0 {
			writerProps = append(writerProps, parquet.WithBloomFilterFPP(opts.BloomFilterFPP))
		}
		for _, col := range opts.BloomFilterColumns {
			writerProps = append(writerProps, parquet.WithBloomFilterEnabledFor(col, true))
		}
	}

	writer, err := pqarrow.NewFileWriter(
		f.Schema(),
		out,
		parquet.NewWriterProperties(writerProps...),
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
func openReader(path string, opts *ReadOptions) (*readerContext, error) {
	if opts == nil {
		opts = &ReadOptions{}
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

	// Row-group selection: honor ReadOptions.RowGroups when set,
	// otherwise all groups. Then narrow further by predicate stats.
	var rowGroups []int
	if len(opts.RowGroups) > 0 {
		total := pf.NumRowGroups()
		rowGroups = make([]int, 0, len(opts.RowGroups))
		for _, rg := range opts.RowGroups {
			if rg < 0 || rg >= total {
				_ = pf.Close()
				_ = f.Close()
				return nil, fmt.Errorf("parquetio: row-group index %d out of range [0,%d)", rg, total)
			}
			rowGroups = append(rowGroups, rg)
		}
	} else {
		rowGroups = make([]int, pf.NumRowGroups())
		for i := range rowGroups {
			rowGroups[i] = i
		}
	}
	// Predicate pushdown: filter row-groups by footer stats. Never
	// causes correctness issues — a false positive (row-group kept
	// that could have been skipped) just costs a bit of extra I/O.
	// filterRowGroupsByPredicate handles a nil Predicate as a no-op.
	rowGroups = filterRowGroupsByPredicate(pf, opts.Predicate, rowGroups)

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

func chunkRows(opts *ReadOptions) int64 {
	if opts != nil && opts.ChunkRows > 0 {
		return int64(opts.ChunkRows)
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

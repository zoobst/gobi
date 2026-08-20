// Package parquetio reads and writes gobi Frames as Apache Parquet.
//
// Compression is delegated to Parquet's built-in codecs. When a Frame
// contains geometry columns, the writers emit a GeoParquet 1.1
// metadata blob under the Parquet file-level "geo" key; the readers
// re-hydrate it into the returned Frame's schema.
//
// The writer offers two entry points:
//
//   - WriteFile serializes a Frame to a filesystem path.
//   - Write serializes a Frame to any io.Writer; the caller owns the
//     stream. Useful for object storage uploads, tar streams, or
//     in-memory buffers.
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
	"io"
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
	// A "top-level column" is a field of the file's root arrow schema.
	// Flat primitives (int64, string, binary, …) map to a single parquet
	// leaf; nested types (struct, list, list-of-struct, map) expand into
	// multiple leaves that all travel together — selecting a struct-typed
	// name by itself pulls its full child tree, matching pyarrow / DuckDB
	// / Polars behavior. Selecting a nested descendant by dotted path
	// (e.g. "bbox.xmin") is not supported.
	//
	// Names not present in the file's top-level schema return
	// ErrColumnNotFound. Column projection is applied at the parquet
	// reader layer: the excluded columns are never fetched, decompressed,
	// or materialized into arrow arrays. The savings scale with how large
	// those columns are relative to the file — narrow analytical files
	// where the caller wants a few columns out of many benefit most.
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

	// IncludeCoveringColumns, when true, returns the GeoParquet 1.1
	// bounding-box covering columns (typically named
	// <geom>_bbox_xmin/_ymin/_xmax/_ymax) in the output frame.
	//
	// Default false — the covering columns exist for row-group
	// pruning at read time and aren't meaningful to callers doing
	// analysis on the returned frame. Preserves the WriteFile ↔
	// ReadFile round-trip contract: a frame written by gobi reads
	// back with the same visible columns.
	//
	// The bbox columns are still USED for pruning regardless of
	// this flag; setting it only controls whether they're visible
	// in the output frame.
	IncludeCoveringColumns bool
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

	// SkipBboxCovering disables the GeoParquet 1.1 covering-bbox
	// column emission that otherwise runs on every write with a
	// geometry column.
	//
	// Default false (bbox columns are emitted) — matches the
	// pushdown story: readers with a spatial predicate hint prune
	// row groups without decoding WKB.
	//
	// Set true when the write cost matters more than the read cost:
	// tiny frames where the extra scan doubles write latency,
	// streaming append loops where footprint is more important
	// than random-access reads, or when writing to a target whose
	// consumer doesn't do row-group pruning anyway. The extra scan
	// is O(N) with one WKB parse per row and adds 32 bytes/row
	// (4 × Float64) to file size.
	SkipBboxCovering bool

	// HilbertSort opts into spatial pre-sorting: before writing,
	// gobi reorders rows by the Hilbert-curve index of each row's
	// primary-geometry centroid so that per-row-group bboxes cluster
	// tightly in space. This is what turns the v0.3.4 row-group
	// pushdown from a synthetic-benchmark curiosity into a real-
	// world speedup: an AOI-shaped predicate can skip most of the
	// file when row groups are spatially local.
	//
	// Default false — spatial sort touches every row (O(N log N))
	// and adds noticeable write latency on large frames. Set true
	// for query-heavy files where the file is written once and
	// scanned many times with AOI-style predicates. Files in
	// insertion order (a raw shp→parquet dump, an append log) see
	// little to no pushdown benefit without it.
	//
	// Sort key is the CENTROID of the primary geometry column
	// (matches how GeoParquet 1.1's covering bbox is computed).
	// Files with multiple geometry columns sort by the first one
	// declared as "primary" in the geo metadata. Ignored on frames
	// that don't have a geometry column.
	HilbertSort bool
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

	// Drop GeoParquet 1.1 covering-bbox columns unless the caller
	// opted in — must match the streaming path's hideCovering
	// projection, otherwise plan.Schema() (from here) and the
	// runtime batches (from frameFromRecord) disagree and downstream
	// operators walk the wrong column indices.
	if opts == nil || !opts.IncludeCoveringColumns {
		hidden := coveringColumnNames(rc.geoRaw, true)
		if len(hidden) > 0 {
			kept := make([]arrow.Field, 0, len(arrowSchema.Fields()))
			for _, f := range arrowSchema.Fields() {
				if _, drop := hidden[f.Name]; drop {
					continue
				}
				kept = append(kept, f)
			}
			arrowSchema = arrow.NewSchema(kept, schemaMetadataPtr(arrowSchema))
		}
	}

	// Attach the "geo" key if the file carried one.
	if rc.geoRaw != "" {
		return attachGeoKey(arrowSchema, rc.geoRaw)
	}
	return arrowSchema, nil
}

// exprAlreadyApplied reports whether pred appears anywhere in the
// current predicate tree — including as an AND-child, an OR-child,
// or nested deeper. Idempotency guard for the ScanFile pushdown
// callback: the optimizer's fixed-point loop re-fires the pushdown
// rule until no plan changes, and without this check we'd AND the
// same pred onto opts.Predicate every pass.
//
// Uses Expr.String() equality — a plan's string form is deterministic
// for a given tree, and the pushdown callback only compares plans it
// itself would have produced, so structural drift isn't a risk here.
func exprAlreadyApplied(current, pred gobi.Expr) bool {
	if current.Node() == nil || pred.Node() == nil {
		return false
	}
	target := pred.String()
	return exprContainsString(current, target)
}

func exprContainsString(e gobi.Expr, target string) bool {
	if e.Node() == nil {
		return false
	}
	if e.String() == target {
		return true
	}
	for _, child := range e.Node().Children() {
		if exprContainsString(child, target) {
			return true
		}
	}
	return false
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
		//
		// Return nil (== "no change") when the incoming pred is
		// already applied. Without this idempotency check, the
		// optimizer's fixed-point loop re-pushes the same predicate
		// every pass and builds up an exponentially-nested chain of
		// (P AND P AND P ...) — 30+ deep by the iteration cap.
		if opts != nil && exprAlreadyApplied(opts.Predicate, pred) {
			return nil
		}
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
	defer table.Release() // frameFromTable Retains what the Frame keeps
	return frameFromTable(table, rc.geoRaw, rc.hideCovering)
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
		frame, err := frameFromRecord(rec, rc.geoRaw, rc.hideCovering)
		if err != nil {
			return err
		}
		cbErr := fn(frame)
		frame.Release()
		if cbErr != nil {
			return fmt.Errorf("%w: %w", ErrChunksAborted, cbErr)
		}
	}
	if err := rr.Err(); err != nil {
		return fmt.Errorf("parquetio: %w", err)
	}
	return nil
}

// ReadReader is the io.ReaderAt-backed counterpart to ReadFile. Reads
// a Parquet file from any random-access byte source whose size is
// known upfront. Materializes the whole payload as a single Frame —
// memory footprint mirrors ReadFile.
//
// Why io.ReaderAt (not plain io.Reader) on the read side? Parquet
// reads footer-first, then jumps to individual row groups; a
// sequential stream can't satisfy that shape. Common sources that
// already satisfy io.ReaderAt without any adapter:
//
//   - *bytes.Reader (in-memory payloads, test fixtures)
//   - *os.File (any file — pair with fi.Size())
//   - S3 GetObject output — s3.GetObjectOutput.Body wrapped as
//     io.ReaderAt via a thin shim on top of Range GET requests
//     (aws-sdk-go-v2's manager.NewDownloader and third-party
//     packages provide this out of the box)
//
// The caller retains ownership of r; ReadReader does not Close it.
//
// GeoParquet metadata + column projection + predicate pushdown work
// the same as ReadFile — ReadOptions is honored uniformly.
func ReadReader(r io.ReaderAt, size int64, opts *ReadOptions) (*gobi.Frame, error) {
	rc, err := openReaderFromRS(newReaderAtSeeker(r, size), noopCloser{}, opts)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	table, err := rc.reader.ReadRowGroups(context.Background(), rc.colIndices, rc.rowGroups)
	if err != nil {
		return nil, err
	}
	defer table.Release() // frameFromTable Retains what the Frame keeps
	return frameFromTable(table, rc.geoRaw, rc.hideCovering)
}

// ReadReaderChunksFunc is the io.ReaderAt-backed counterpart to
// ReadFileChunksFunc. Streams the Parquet payload as record-batch-sized
// Frames without materializing the whole file. Batch lifetime + error
// semantics mirror the path-based version.
//
// The caller retains ownership of r; ReadReaderChunksFunc does not
// Close it.
func ReadReaderChunksFunc(r io.ReaderAt, size int64, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	rc, err := openReaderFromRS(newReaderAtSeeker(r, size), noopCloser{}, opts)
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
		frame, err := frameFromRecord(rec, rc.geoRaw, rc.hideCovering)
		if err != nil {
			return err
		}
		cbErr := fn(frame)
		frame.Release()
		if cbErr != nil {
			return fmt.Errorf("%w: %w", ErrChunksAborted, cbErr)
		}
	}
	if err := rr.Err(); err != nil {
		return fmt.Errorf("parquetio: %w", err)
	}
	return nil
}

// readerAtSeeker wraps io.ReaderAt + known Size into arrow-go's
// parquet.ReaderAtSeeker interface. Seek is implemented against the
// known size (arrow-go uses SeekEnd/0 to discover file length; the
// remaining Seek modes track a virtual position for compatibility).
type readerAtSeeker struct {
	ra   io.ReaderAt
	size int64
	pos  int64
}

func newReaderAtSeeker(ra io.ReaderAt, size int64) *readerAtSeeker {
	return &readerAtSeeker{ra: ra, size: size}
}

func (r *readerAtSeeker) ReadAt(p []byte, off int64) (int, error) {
	return r.ra.ReadAt(p, off)
}

func (r *readerAtSeeker) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	n, err := r.ra.ReadAt(p, r.pos)
	r.pos += int64(n)
	return n, err
}

func (r *readerAtSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("parquetio: readerAtSeeker: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("parquetio: readerAtSeeker: negative position")
	}
	r.pos = abs
	return abs, nil
}

// noopCloser satisfies io.Closer for reader-based paths where the
// caller retains ownership of the byte source.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

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
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := Write(f, out, opts); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// writeOnly hides an io.Writer's io.Closer surface (if any) from
// pqarrow. FileWriter.Close calls Close on the underlying writer
// when it satisfies io.Closer, which would violate Write's
// caller-owns-w contract (e.g. a *gzip.Writer wrapping a *os.File,
// or a caller who intends to append more data after the parquet
// payload).
type writeOnly struct{ w io.Writer }

func (wo writeOnly) Write(p []byte) (int, error) { return wo.w.Write(p) }

// Write serializes f as Parquet to w. The caller owns w and is
// responsible for closing it. Use WriteFile for the common
// path-based case.
func Write(f *gobi.Frame, w io.Writer, opts *WriteOptions) error {
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
	// Route the write through one of three paths depending on
	// HilbertSort / SkipBboxCovering. The fused path is the sweet
	// spot for HilbertSort=true: sort + bbox-covering augmentation
	// share a single WKB parse pass instead of walking every row
	// twice.
	var (
		augmented *gobi.Frame
		meta      *gobi.GeoParquetMetadata
	)
	switch {
	case opts.HilbertSort && !opts.SkipBboxCovering:
		// Fused single-pass: sort + augment share one WKB scan for
		// the primary geometry column.
		if primary := primaryGeometryColumn(f); primary != "" {
			augmented, meta, err = gobi.HilbertSortWithCovering(f, primary)
		} else {
			// No geometry column → HilbertSort is a no-op; fall
			// through to the normal augment path.
			augmented, meta, err = gobi.WithBboxCoveringColumns(f)
		}
	case opts.HilbertSort && opts.SkipBboxCovering:
		// Sort but skip augmentation. Two-step form is unavoidable
		// (there's no augmentation to fuse with).
		//
		// Refcount discipline: Retain() runs only AFTER
		// BuildGeoParquetMetadata succeeds. A metadata error before
		// the Retain leaves nothing to leak; after would leak the
		// extra ref (the defer at the bottom only fires when we
		// reach the code after the error check).
		if primary := primaryGeometryColumn(f); primary != "" {
			var sorted *gobi.Frame
			sorted, err = f.SortByHilbert(primary)
			if err == nil {
				meta, err = gobi.BuildGeoParquetMetadata(sorted)
				if err == nil {
					augmented = sorted
					augmented.Retain()
				}
				sorted.Release()
			}
		} else {
			meta, err = gobi.BuildGeoParquetMetadata(f)
			if err == nil {
				augmented = f
				augmented.Retain()
			}
		}
	case opts.SkipBboxCovering:
		// No sort, no bbox augmentation. Same Retain-after-metadata
		// discipline as above.
		meta, err = gobi.BuildGeoParquetMetadata(f)
		if err == nil {
			augmented = f
			augmented.Retain()
		}
	default:
		// No sort, standard augmentation.
		augmented, meta, err = gobi.WithBboxCoveringColumns(f)
	}
	if err != nil {
		return err
	}
	defer augmented.Release()

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
		augmented.Schema(),
		writeOnly{w: w},
		parquet.NewWriterProperties(writerProps...),
		pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()),
	)
	if err != nil {
		return err
	}
	// f.Table() Retains each column (NewTable's contract). Release
	// the transient Table view after WriteTable consumes it —
	// otherwise the per-column ref stays live for the lifetime of
	// f, effectively doubling f's memory footprint until it's
	// eventually collected.
	tbl := augmented.Table()
	// On error paths, join Close's error with the primary failure —
	// the parquet footer is written on Close, so a truncated/invalid
	// output leaves diagnostic value in Close's return even when the
	// primary error is more informative.
	if err := writer.WriteTable(tbl, int64(augmented.NumRows())); err != nil {
		tbl.Release()
		return errors.Join(err, writer.Close())
	}
	tbl.Release()
	if meta != nil {
		blob, err := marshalGeoMeta(meta)
		if err != nil {
			return errors.Join(err, writer.Close())
		}
		if err := writer.AppendKeyValueMetadata(gobi.GeoParquetMetadataKey, blob); err != nil {
			return errors.Join(err, writer.Close())
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
	// closer is the outer resource owning the parquet bytes — an
	// *os.File for path-based reads, a no-op for reader-based reads
	// where the caller manages the underlying stream. Always non-nil.
	closer      io.Closer
	parquetFile *file.Reader
	reader      *pqarrow.FileReader
	colIndices  []int
	rowGroups   []int
	geoRaw      string
	// hideCovering: opts.IncludeCoveringColumns was false → the
	// output Frame should drop bbox covering columns declared in
	// geoRaw. The columns are still read from parquet (needed for
	// row-group pruning stats) — the flag only controls Frame
	// projection. See ReadOptions.IncludeCoveringColumns.
	hideCovering bool
}

func (rc *readerContext) close() {
	if rc.parquetFile != nil {
		_ = rc.parquetFile.Close()
	}
	if rc.closer != nil {
		_ = rc.closer.Close()
	}
}

// openReader opens path and calls openReaderFromRS. Kept as a thin
// wrapper so path-based callers (ReadFile / ScanFile / etc.) don't
// have to know about the ReaderAtSeeker interface.
func openReader(path string, opts *ReadOptions) (*readerContext, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return openReaderFromRS(f, f, opts)
}

// openReaderFromRS is the shared reader-construction path for both
// path-based (openReader) and reader-based (ReadReader et al.) entry
// points. rs is the parquet.ReaderAtSeeker fed to arrow-go's parquet
// reader; closer owns the underlying byte source. rs and closer may
// point at the same value (as they do for *os.File).
func openReaderFromRS(rs parquet.ReaderAtSeeker, closer io.Closer, opts *ReadOptions) (*readerContext, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	pool := opts.Allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}

	rp := parquet.NewReaderProperties(pool)
	rp.PageStreamingEnabled = true
	pf, err := file.NewParquetReader(rs, file.WithReadProps(rp))
	if err != nil {
		_ = closer.Close()
		return nil, err
	}

	geoRaw := ""
	if kv := pf.MetaData().KeyValueMetadata(); kv != nil {
		if v := kv.FindValue(gobi.GeoParquetMetadataKey); v != nil {
			geoRaw = *v
		}
	}

	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{
		Parallel:           true,
		BatchSize:          chunkRows(opts),
		PreAllocBinaryData: true,
	}, pool)
	if err != nil {
		_ = pf.Close()
		_ = closer.Close()
		return nil, err
	}

	colIndices, err := resolveColumns(pf, fr, opts.Columns)
	if err != nil {
		_ = pf.Close()
		_ = closer.Close()
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
				_ = closer.Close()
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
		closer:       closer,
		parquetFile:  pf,
		reader:       fr,
		colIndices:   colIndices,
		rowGroups:    rowGroups,
		geoRaw:       geoRaw,
		hideCovering: !opts.IncludeCoveringColumns,
	}, nil
}

// resolveColumns maps opts.Columns (names) to leaf-parquet-column indices
// for GetRecordReader / ReadRowGroups. When names is empty, returns an
// explicit "all indices" slice — nil would work for GetRecordReader but
// ReadRowGroups treats nil as "no columns," so we always emit a
// concrete list to keep both paths symmetric.
//
// Top-level arrow fields can expand to more than one parquet leaf (a
// struct, a list-of-struct, a map, etc.), so we walk the pqarrow
// SchemaManifest for each requested name and collect every leaf ColIndex
// beneath it. Assuming arrow-field-index == parquet-leaf-index only
// works for fully flat schemas and silently returns the wrong columns.
func resolveColumns(pf *file.Reader, fr *pqarrow.FileReader, names []string) ([]int, error) {
	numLeaves := pf.MetaData().Schema.NumColumns()
	if len(names) == 0 {
		all := make([]int, numLeaves)
		for i := range all {
			all[i] = i
		}
		return all, nil
	}
	manifest := fr.Manifest
	nameToField := make(map[string]*pqarrow.SchemaField, len(manifest.Fields))
	for i := range manifest.Fields {
		f := &manifest.Fields[i]
		nameToField[f.Field.Name] = f
	}
	out := make([]int, 0, len(names))
	for _, name := range names {
		field, ok := nameToField[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
		}
		appendLeafColIndices(field, &out)
	}
	return out, nil
}

// appendLeafColIndices walks a pqarrow SchemaField subtree and appends
// every leaf's parquet column index to out in declaration order.
func appendLeafColIndices(f *pqarrow.SchemaField, out *[]int) {
	if f.IsLeaf() {
		*out = append(*out, f.ColIndex)
		return
	}
	for i := range f.Children {
		appendLeafColIndices(&f.Children[i], out)
	}
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
// metadata blob to the schema if present. When hideCovering is true,
// the GeoParquet 1.1 covering-bbox columns declared in geoRaw are
// dropped from the returned frame — preserving the WriteFile ↔
// ReadFile round-trip contract (the bbox columns still exist in the
// file and are used by predicate pushdown before this call runs).
//
// Retains each kept column's Chunked so the Frame owns its own ref
// — the caller is expected to `table.Release()` after this returns
// (see ReadFile / ReadReader). Without the Retain here, the copied
// Column values share the Table's Chunked pointers without an
// ownership increment, and either (a) never get freed if the Table's
// Release isn't called, or (b) double-decrement when both
// Table.Release and Frame.Release run against the same underlying
// Chunked.
func frameFromTable(table arrow.Table, geoRaw string, hideCovering bool) (*gobi.Frame, error) {
	schema := table.Schema()
	if geoRaw != "" {
		var err error
		schema, err = attachGeoKey(schema, geoRaw)
		if err != nil {
			return nil, err
		}
	}
	hidden := coveringColumnNames(geoRaw, hideCovering)
	keptFields := make([]arrow.Field, 0, table.NumCols())
	keptCols := make([]arrow.Column, 0, table.NumCols())
	for i := int64(0); i < table.NumCols(); i++ {
		c := *table.Column(int(i))
		if _, drop := hidden[c.Name()]; drop {
			continue
		}
		c.Data().Retain() // Frame gets its own ref on the Chunked.
		keptFields = append(keptFields, schema.Field(int(i)))
		keptCols = append(keptCols, c)
	}
	outSchema := arrow.NewSchema(keptFields, schemaMetadataPtr(schema))
	return gobi.NewFrame(outSchema, keptCols)
}

// frameFromRecord wraps one record batch's arrays in a Frame. Uses
// arrow.NewColumnFromArr, which Retains each array once — so the Frame
// owns its refs and the source record can be Released independently.
// Honors hideCovering the same way as frameFromTable.
func frameFromRecord(rec arrow.RecordBatch, geoRaw string, hideCovering bool) (*gobi.Frame, error) {
	schema := rec.Schema()
	if geoRaw != "" {
		var err error
		schema, err = attachGeoKey(schema, geoRaw)
		if err != nil {
			return nil, err
		}
	}
	hidden := coveringColumnNames(geoRaw, hideCovering)
	n := int(rec.NumCols())
	keptFields := make([]arrow.Field, 0, n)
	keptCols := make([]arrow.Column, 0, n)
	for i := range n {
		field := schema.Field(i)
		if _, drop := hidden[field.Name]; drop {
			continue
		}
		keptFields = append(keptFields, field)
		keptCols = append(keptCols, arrow.NewColumnFromArr(field, rec.Column(i)))
	}
	outSchema := arrow.NewSchema(keptFields, schemaMetadataPtr(schema))
	return gobi.NewFrame(outSchema, keptCols)
}

// coveringColumnNames returns the set of GeoParquet 1.1 covering-bbox
// column names declared in geoRaw, or an empty map when hideCovering
// is false / geoRaw is empty / no covering entries are present. Used
// by frameFromTable / frameFromRecord to skip these columns in the
// output frame while still keeping them available on disk for
// row-group pruning.
// primaryGeometryColumn returns the name of the primary geometry
// column in f — the geometry a HilbertSort should sort against.
//
// Resolution order:
//
//  1. The schema-level "geo" metadata blob's primary_column field,
//     when set. This is what GeoParquet 1.1 files carry explicitly
//     and honors the writer's declared choice for multi-geometry-
//     column frames.
//  2. First schema-order field tagged as a geometry column via
//     MetaGeometryType. Fallback for frames built up in-process
//     that never got a geo metadata blob attached.
//  3. Empty string when f has no geometry columns — HilbertSort
//     becomes a no-op.
//
// Schema-only lookup — no data scan.
func primaryGeometryColumn(f *gobi.Frame) string {
	// Step 1: consult the geo metadata blob if the schema carries one.
	if md := f.Schema().Metadata(); md.Len() > 0 {
		if raw, ok := md.GetValue(gobi.GeoParquetMetadataKey); ok && raw != "" {
			if meta, err := gobi.ParseGeoParquetMetadata(raw); err == nil && meta != nil && meta.PrimaryColumn != "" {
				// Verify the declared primary column actually exists
				// in the schema (defensive against stale metadata).
				for _, field := range f.Schema().Fields() {
					if field.Name == meta.PrimaryColumn {
						return meta.PrimaryColumn
					}
				}
			}
		}
	}
	// Step 2: schema-order fallback.
	for _, field := range f.Schema().Fields() {
		if _, ok := field.Metadata.GetValue(gobi.MetaGeometryType); ok {
			return field.Name
		}
	}
	return ""
}

func coveringColumnNames(geoRaw string, hideCovering bool) map[string]struct{} {
	if !hideCovering || geoRaw == "" {
		return nil
	}
	// Malformed geoRaw (hand-written / third-party writer bug) is
	// swallowed deliberately: this function's failure mode should be
	// "leave every column visible" rather than "fail the read." The
	// bbox columns will surface in the output frame, which is at
	// worst a UX blemish, whereas failing the read blocks the whole
	// pipeline for a metadata problem that's tangential to the data.
	// A stricter caller can call gobi.ParseGeoParquetMetadata directly
	// via the schema's "geo" key and report the error themselves.
	meta, err := gobi.ParseGeoParquetMetadata(geoRaw)
	if err != nil || meta == nil {
		return nil
	}
	out := map[string]struct{}{}
	for _, cm := range meta.Columns {
		if cm.Covering == nil || cm.Covering.Bbox == nil {
			continue
		}
		bb := cm.Covering.Bbox
		for _, path := range [][]string{bb.Xmin, bb.Ymin, bb.Xmax, bb.Ymax} {
			// Flat covering: single-element path is the top-level
			// column name. Nested (struct-field) paths aren't
			// hideable at the Frame level today — leave them visible.
			if len(path) == 1 {
				out[path[0]] = struct{}{}
			}
		}
	}
	return out
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

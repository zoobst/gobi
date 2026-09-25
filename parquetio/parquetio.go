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
	"math"
	"math/bits"
	"os"
	"slices"
	"strconv"
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

	// IncludeCoveringColumns, when true, returns the generated
	// GeoParquet 1.1 bounding-box covering columns
	// (<geom>_bbox_xmin/_ymin/_xmax/_ymax) in the output frame.
	//
	// Only columns with those generated names are ever hidden. A
	// covering that points at ordinary data columns — e.g. lon / lat
	// via WriteOptions.Coverings — leaves them visible, since they're
	// real data.
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

	// SerialColumnDecode decodes each row group's columns on the
	// calling goroutine instead of one goroutine per column.
	//
	// Default false: per-column parallelism speeds up a single wide
	// read. Set it when the caller already reads many files
	// concurrently. There the extra goroutines (projected columns ×
	// concurrent files) add scheduling and memory overhead without
	// adding throughput. Doesn't change ScanWorkers, which splits
	// row groups across workers at the LazyFrame layer.
	SerialColumnDecode bool

	// BufferedStreamBytes, when > 0, reads each column chunk through
	// a buffer of this many bytes instead of loading the chunk's
	// whole compressed bytes into memory before decoding. That lowers
	// peak memory for wide or large row groups, at the cost of more,
	// smaller reads — worth it on local disk, usually not on
	// high-latency object storage. 0 (default) reads whole chunks.
	BufferedStreamBytes int64
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
	// uses arrow-go's default cap of 64Mi rows — in practice one row
	// group per write for most frames.
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
	//
	// Sizing. Each filter is sized to the distinct values its row
	// group actually holds: the writer tracks power-of-two candidate
	// filters from BloomFilterMaxBytes down to the smallest size
	// arrow-go rates for at least 500 distinct values at
	// BloomFilterFPP, and keeps the smallest candidate that fits
	// (arrow-go's adaptive bloom filter). At the default 1% FPP the
	// floor is 2 KiB: a row group with up to ~500 distinct values
	// gets 2 KiB, ~20k distinct gets 64 KiB. Looser FPPs have a lower
	// floor. arrow-go rates candidates conservatively, so a filter
	// can be up to 2× the theoretical optimum.
	//
	// Memory: every candidate stays resident until the row group's
	// distinct count rules it out, so a low-cardinality column holds
	// about 2 × BloomFilterMaxBytes (1 + 1/2 + 1/4 + …) per column
	// chunk while writing — ~2 MiB at the default cap. Set
	// BloomFilterNDV for a column to size it from a known distinct
	// count instead (one filter, no candidates).
	BloomFilterColumns []string

	// BloomFilterFPP is the target false-positive probability for
	// the bloom filters written above. 0 uses arrow-go's default
	// (0.01). Lower FPP → larger filter on disk; reasonable range
	// 0.01–0.1. Must be in [0, 1); Write rejects anything else.
	BloomFilterFPP float64

	// BloomFilterNDV optionally gives the expected number of distinct
	// values per row group for a bloom-filtered column. A column with
	// an entry gets a filter sized for exactly that NDV at
	// BloomFilterFPP (capped at BloomFilterMaxBytes), skipping the
	// adaptive candidates. Use it when the cardinality is known and
	// stable; an underestimate raises the real false-positive rate.
	// Columns not listed here, or listed with 0, use adaptive sizing.
	//
	// Every key must also appear in BloomFilterColumns — an NDV does
	// not enable a filter by itself — and values must be in
	// [0, 2^32). Write returns an error otherwise.
	BloomFilterNDV map[string]int64

	// BloomFilterMaxBytes caps each bloom filter's size. 0 uses
	// arrow-go's default, 1 MiB. Rounded DOWN to a power of two so
	// the cap holds on every path (arrow-go's split-block filters
	// are power-of-two sized on the adaptive path, and a clamp to an
	// arbitrary byte count would not be a whole number of 32-byte
	// blocks on the NDV path). With adaptive sizing this is the
	// largest candidate: a row group whose distinct count needs more
	// than this at BloomFilterFPP gets a max-size filter with a
	// higher false-positive rate. Must be 0 or in [32 B, 128 MiB].
	BloomFilterMaxBytes int64

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

	// Coverings declares, per geometry column, existing numeric columns
	// that already hold each row's bounding box — for point data,
	// PointCovering("lon", "lat"). Such a column gets no generated
	// <geom>_bbox_* columns; the GeoParquet covering.bbox names the
	// declared columns instead, and readers prune row groups from their
	// min / max statistics. Every row is checked: a geometry outside its
	// declared covering fails the write, since it would let readers skip
	// row groups that hold matching rows. Geometry columns not listed
	// here follow SkipBboxCovering as before.
	Coverings map[string]Covering

	// CompressionLevel sets the codec's compression level. 0 (default)
	// uses the codec's own default. Valid ranges: zstd 1–22, gzip 1–9,
	// brotli 1–11. Setting a level for a codec without levels (snappy,
	// lz4, uncompressed) is an error.
	//
	// The pure-Go zstd encoder maps levels onto four tiers (roughly 1
	// fastest, 2–3 default, 4–8 better, 9+ best), and size isn't
	// monotonic in the tier: on 200k rows of text-like strings, level 1
	// wrote a smaller file than the default, and the best tier was
	// smallest but ~7× slower. Measure on your data before raising it.
	CompressionLevel int

	// Allocator allocates the writer's buffers: encoded pages,
	// dictionaries, bloom filters, and the generated bbox covering
	// columns. nil uses memory.DefaultAllocator.
	Allocator memory.Allocator

	// KeyValueMetadata adds entries to the parquet footer's key-value
	// metadata, written in key order after gobi's own. A key that is
	// also in the frame's schema-level metadata replaces it rather than
	// being duplicated. "ARROW:schema" is reserved, and so is "geo" for
	// frames with a geometry column (use Coverings to shape gobi's
	// GeoParquet entry); setting either is an error.
	KeyValueMetadata map[string]string

	// CoerceTimestamps converts every timestamp column to this unit
	// on write. Empty (the default) writes each column at its own
	// unit — gobi's native Timestamp[ns] becomes parquet
	// TIMESTAMP(NANOS), which older Spark / Hive / Athena engine v2
	// readers reject. Set TimestampMicros for broad engine
	// compatibility. Each column's time zone is kept: a zoned column
	// writes isAdjustedToUTC=true, a zone-less one false.
	//
	// Coarsening that drops precision (ns → us with a nonzero
	// sub-microsecond part) is an error unless
	// AllowTruncatedTimestamps is set. The file reads back at the
	// coerced unit.
	CoerceTimestamps TimestampUnit

	// AllowTruncatedTimestamps lets CoerceTimestamps drop sub-unit
	// precision instead of failing the write. Ignored when
	// CoerceTimestamps is empty.
	AllowTruncatedTimestamps bool
}

// TimestampUnit names a parquet timestamp precision for
// WriteOptions.CoerceTimestamps.
type TimestampUnit string

const (
	TimestampMillis TimestampUnit = "ms"
	TimestampMicros TimestampUnit = "us"
	TimestampNanos  TimestampUnit = "ns"
)

func (u TimestampUnit) toArrow() (arrow.TimeUnit, error) {
	switch u {
	case TimestampMillis:
		return arrow.Millisecond, nil
	case TimestampMicros:
		return arrow.Microsecond, nil
	case TimestampNanos:
		return arrow.Nanosecond, nil
	}
	return 0, fmt.Errorf("parquetio: CoerceTimestamps %q (want ms, us, or ns)", string(u))
}

// writerSchema returns the schema handed to pqarrow's FileWriter,
// with the schema-level metadata keys that gobi writes to the footer
// itself removed.
//
// pqarrow copies schema-level metadata into the parquet footer, and
// gobi appends its own entries after that: the GeoParquet "geo" entry
// and WriteOptions.KeyValueMetadata. A frame read from a GeoParquet
// file carries the source file's "geo" in its schema metadata (see
// attachGeoKey), so writing it back used to produce two "geo" footer
// keys — the stale one first, which is the one readers take. Dropping
// the schema's copy of every key gobi writes keeps exactly one entry
// per key.
//
// Only schema-level metadata changes; fields (and their geometry
// tags) are untouched, and arrow's Schema.Equal ignores schema
// metadata, so WriteTable still accepts the frame's own table.
func writerSchema(s *arrow.Schema, drop map[string]bool) *arrow.Schema {
	if len(drop) == 0 || !s.HasMetadata() {
		return s
	}
	md := s.Metadata()
	keys := make([]string, 0, md.Len())
	values := make([]string, 0, md.Len())
	for i, k := range md.Keys() {
		if drop[k] {
			continue
		}
		keys = append(keys, k)
		values = append(values, md.Values()[i])
	}
	if len(keys) == md.Len() {
		return s
	}
	stripped := arrow.NewMetadata(keys, values)
	return arrow.NewSchemaWithEndian(s.Fields(), &stripped, s.Endianness())
}

// footerKeys is the set of footer keys gobi writes itself for a file:
// "geo" when it has geometry, plus every KeyValueMetadata key.
func footerKeys(hasGeo bool, opts *WriteOptions) map[string]bool {
	keys := make(map[string]bool, len(opts.KeyValueMetadata)+1)
	if hasGeo {
		keys[gobi.GeoParquetMetadataKey] = true
	}
	for k := range opts.KeyValueMetadata {
		keys[k] = true
	}
	return keys
}

// appendFooter writes gobi's footer entries — the GeoParquet "geo"
// entry (when meta is non-nil), then KeyValueMetadata in key order.
func appendFooter(fw *pqarrow.FileWriter, meta *gobi.GeoParquetMetadata, opts *WriteOptions) error {
	if meta != nil {
		blob, err := marshalGeoMeta(meta)
		if err != nil {
			return err
		}
		if err := fw.AppendKeyValueMetadata(gobi.GeoParquetMetadataKey, blob); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(opts.KeyValueMetadata))
	for k := range opts.KeyValueMetadata {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := fw.AppendKeyValueMetadata(k, opts.KeyValueMetadata[k]); err != nil {
			return fmt.Errorf("parquetio: KeyValueMetadata[%q]: %w", k, err)
		}
	}
	return nil
}

// Bloom filter size bounds, matching arrow-go's split-block filter:
// one 32-byte block minimum, 128 MiB maximum.
const (
	minBloomFilterBytes = 32
	maxBloomFilterBytes = 128 << 20
)

// bloomFilterProps validates the bloom options and builds the writer
// properties for opts.BloomFilterColumns. Returns nil props when no
// columns are requested.
//
// Without an NDV or adaptive sizing, arrow-go allocates every filter
// at the max size (1 MiB) regardless of cardinality — five filtered
// columns cost 5 MiB per row group. Adaptive sizing fixes that, but
// arrow-go's default of 5 candidates halves from the max only down
// to max/16 (64 KiB), still ~30× what a few-hundred-NDV row group
// needs. So gobi asks for one candidate per power of two down to the
// 32-byte minimum. arrow-go stops generating candidates at the first
// size it rates below 500 distinct values at the target FPP, so the
// effective floor follows the FPP (2 KiB at 1%) and the extra count
// costs nothing.
func bloomFilterProps(opts *WriteOptions) ([]parquet.WriterProperty, error) {
	if opts.BloomFilterFPP < 0 || opts.BloomFilterFPP >= 1 {
		return nil, fmt.Errorf("parquetio: BloomFilterFPP %v must be in [0, 1)", opts.BloomFilterFPP)
	}
	if opts.BloomFilterMaxBytes != 0 &&
		(opts.BloomFilterMaxBytes < minBloomFilterBytes || opts.BloomFilterMaxBytes > maxBloomFilterBytes) {
		return nil, fmt.Errorf("parquetio: BloomFilterMaxBytes %d must be 0 or in [%d, %d]",
			opts.BloomFilterMaxBytes, minBloomFilterBytes, maxBloomFilterBytes)
	}
	enabled := make(map[string]bool, len(opts.BloomFilterColumns))
	for _, col := range opts.BloomFilterColumns {
		enabled[col] = true
	}
	for col, ndv := range opts.BloomFilterNDV {
		if !enabled[col] {
			return nil, fmt.Errorf("parquetio: BloomFilterNDV[%q] set but %q is not in BloomFilterColumns", col, col)
		}
		if ndv < 0 || ndv > math.MaxUint32 {
			return nil, fmt.Errorf("parquetio: BloomFilterNDV[%q] = %d must be in [0, %d]", col, ndv, uint32(math.MaxUint32))
		}
	}
	if len(opts.BloomFilterColumns) == 0 {
		return nil, nil
	}

	var props []parquet.WriterProperty
	if opts.BloomFilterFPP > 0 {
		props = append(props, parquet.WithBloomFilterFPP(opts.BloomFilterFPP))
	}
	maxBytes := int64(parquet.DefaultMaxBloomFilterBytes)
	if opts.BloomFilterMaxBytes > 0 {
		// Round down to a power of two (see BloomFilterMaxBytes).
		maxBytes = int64(1) << (bits.Len64(uint64(opts.BloomFilterMaxBytes)) - 1)
		props = append(props, parquet.WithMaxBloomFilterBytes(maxBytes))
	}
	// One candidate per power of two from maxBytes down to the
	// 32-byte minimum: 1 MiB → 16 candidates, of which arrow-go keeps
	// those it rates for >= 500 NDV.
	candidates := bits.Len64(uint64(maxBytes / minBloomFilterBytes))
	for _, col := range opts.BloomFilterColumns {
		props = append(props, parquet.WithBloomFilterEnabledFor(col, true))
		if ndv := opts.BloomFilterNDV[col]; ndv > 0 {
			// arrow-go sizes from NDV + FPP when NDV is set; adaptive
			// is ignored for this column.
			props = append(props, parquet.WithBloomFilterNDVFor(col, ndv))
			continue
		}
		props = append(props,
			parquet.WithAdaptiveBloomFilterEnabledFor(col, true),
			parquet.WithBloomFilterCandidatesFor(col, candidates))
	}
	return props, nil
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
// The optimizer pushes work above the scan into the parquet reader:
//   - Select → projection pushdown: only the referenced columns are
//     read (ReadOptions.Columns).
//   - Filter → predicate pushdown: row groups whose min/max statistics
//     (and GeoParquet covering, for spatial predicates) prove the
//     predicate false are skipped (ReadOptions.Predicate). Rows in the
//     surviving row groups are still filtered above the scan.
//
// Row groups stream in batches, split across ScanWorkers workers.
// Bloom filters are not consulted yet.
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
// checkCompressionLevel validates level for codec (see
// WriteOptions.CompressionLevel).
func checkCompressionLevel(codec Codec, level int) error {
	var lo, hi int
	switch codec {
	case CodecZstd:
		lo, hi = 1, 22
	case CodecGzip:
		lo, hi = 1, 9
	case CodecBrotli:
		lo, hi = 1, 11
	default:
		return fmt.Errorf("parquetio: CompressionLevel %d: codec %q has no compression levels", level, codec)
	}
	if level < lo || level > hi {
		return fmt.Errorf("parquetio: CompressionLevel %d out of range [%d, %d] for %s", level, lo, hi, codec)
	}
	return nil
}

// writerProperties validates opts and builds the parquet and pqarrow
// writer properties shared by Write and Writer: codec, row-group cap,
// bloom filters, stored Arrow schema, timestamp coercion.
func writerProperties(opts *WriteOptions) ([]parquet.WriterProperty, []pqarrow.WriterOption, error) {
	codec := opts.Codec
	if codec == "" {
		codec = CodecSnappy
	}
	compression, err := codec.toArrow()
	if err != nil {
		return nil, nil, err
	}
	bloomProps, err := bloomFilterProps(opts)
	if err != nil {
		return nil, nil, err
	}
	writerProps := []parquet.WriterProperty{parquet.WithCompression(compression)}
	if opts.CompressionLevel != 0 {
		if err := checkCompressionLevel(codec, opts.CompressionLevel); err != nil {
			return nil, nil, err
		}
		writerProps = append(writerProps, parquet.WithCompressionLevel(opts.CompressionLevel))
	}
	if opts.Allocator != nil {
		writerProps = append(writerProps, parquet.WithAllocator(opts.Allocator))
	}
	if opts.RowGroupRows > 0 {
		writerProps = append(writerProps, parquet.WithMaxRowGroupLength(opts.RowGroupRows))
	}
	writerProps = append(writerProps, bloomProps...)

	arrowProps := []pqarrow.WriterOption{pqarrow.WithStoreSchema()}
	if opts.Allocator != nil {
		arrowProps = append(arrowProps, pqarrow.WithAllocator(opts.Allocator))
	}
	if opts.CoerceTimestamps != "" {
		unit, err := opts.CoerceTimestamps.toArrow()
		if err != nil {
			return nil, nil, err
		}
		arrowProps = append(arrowProps,
			pqarrow.WithCoerceTimestamps(unit),
			pqarrow.WithTruncatedTimestamps(opts.AllowTruncatedTimestamps))
	}
	return writerProps, arrowProps, nil
}

type writeOnly struct{ w io.Writer }

func (wo writeOnly) Write(p []byte) (int, error) { return wo.w.Write(p) }

// Write serializes f as Parquet to w. The caller owns w and is
// responsible for closing it. Use WriteFile for the common
// path-based case.
func Write(f *gobi.Frame, w io.Writer, opts *WriteOptions) error {
	if opts == nil {
		opts = &WriteOptions{}
	}
	// Validate before the (possibly O(N)) augmentation work below.
	writerProps, arrowProps, err := writerProperties(opts)
	if err != nil {
		return err
	}
	if err := validateGeoOptions(f.Schema(), opts); err != nil {
		return err
	}
	// HilbertSort with generated covering columns takes the fused path:
	// sort + bbox augmentation share one WKB parse. Declared Coverings
	// (or SkipBboxCovering) sort first, then augment what's left to
	// generate. Everything else goes straight to prepareGeo.
	var (
		augmented *gobi.Frame
		meta      *gobi.GeoParquetMetadata
	)
	primary := primaryGeometryColumn(f)
	switch {
	case opts.HilbertSort && primary != "" && !opts.SkipBboxCovering && len(opts.Coverings) == 0:
		augmented, meta, err = gobi.HilbertSortWithCovering(f, primary)
	case opts.HilbertSort && primary != "":
		var sorted *gobi.Frame
		if sorted, err = f.SortByHilbert(primary); err == nil {
			augmented, meta, err = prepareGeo(sorted, opts)
			sorted.Release()
		}
	default:
		augmented, meta, err = prepareGeo(f, opts)
	}
	if err != nil {
		return err
	}
	defer augmented.Release()

	writer, err := pqarrow.NewFileWriter(
		writerSchema(augmented.Schema(), footerKeys(meta != nil, opts)),
		writeOnly{w: w},
		parquet.NewWriterProperties(writerProps...),
		pqarrow.NewArrowWriterProperties(arrowProps...),
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
	if err := appendFooter(writer, meta, opts); err != nil {
		return errors.Join(err, writer.Close())
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

	if opts.BufferedStreamBytes < 0 {
		_ = closer.Close()
		return nil, fmt.Errorf("parquetio: BufferedStreamBytes %d must be >= 0", opts.BufferedStreamBytes)
	}
	rp := parquet.NewReaderProperties(pool)
	rp.PageStreamingEnabled = true
	if opts.BufferedStreamBytes > 0 {
		rp.BufferedStreamEnabled = true
		rp.BufferSize = opts.BufferedStreamBytes
	}
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
		Parallel:           !opts.SerialColumnDecode,
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
		field := schema.Field(int(i))
		if _, drop := hidden[field.Name]; drop {
			continue
		}
		// Rebuild the Column against the (possibly geometry-stamped)
		// schema field rather than reusing the pqarrow-provided
		// Column.Field. NewColumn retains the underlying Chunked, so
		// the Frame ends up with its own ref — mirrors the pre-fix
		// c.Data().Retain() but also lets attachGeoKey's per-field
		// tags reach Series.field via Column.Field().
		src := table.Column(int(i))
		keptFields = append(keptFields, field)
		keptCols = append(keptCols, *arrow.NewColumn(field, src.Data()))
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
	for geom, cm := range meta.Columns {
		if cm.Covering == nil || cm.Covering.Bbox == nil {
			continue
		}
		bb := cm.Covering.Bbox
		xmin, ymin, xmax, ymax := gobi.BboxColumnNames(geom)
		for _, c := range []struct {
			path []string
			gen  string
		}{{bb.Xmin, xmin}, {bb.Ymin, ymin}, {bb.Xmax, xmax}, {bb.Ymax, ymax}} {
			// Hide only gobi's generated covering columns. A covering
			// that names ordinary columns (lon / lat for points) must
			// not make real data disappear. Nested (struct-field)
			// paths aren't hideable at the Frame level either.
			if len(c.path) == 1 && c.path[0] == c.gen {
				out[c.gen] = struct{}{}
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
// to raw AND with per-field gobi:geometry_type / gobi:crs_epsg tags
// added to every top-level column declared as a geometry in the blob
// that isn't already tagged.
//
// Rationale: GeoParquet 1.1 declares geometry columns only in the
// file-level JSON blob — files not written by gobi (Overture,
// geopandas, DuckDB spatial, etc.) carry no arrow-level per-field
// metadata for the geometry column. gobi's IsGeometry check works off
// the per-field tag, so re-projecting the blob into per-field
// metadata at read time is what makes third-party GeoParquet files
// interoperate with the geometry-aware operators.
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

	fields := schema.Fields()
	if meta, err := gobi.ParseGeoParquetMetadata(raw); err == nil && meta != nil && len(meta.Columns) > 0 {
		stamped := make([]arrow.Field, len(fields))
		copy(stamped, fields)
		for i := range stamped {
			cm, ok := meta.Columns[stamped[i].Name]
			if !ok {
				continue
			}
			if stamped[i].Type.ID() != arrow.BINARY {
				// gobi's geometry path only recognises BINARY-typed
				// WKB columns; leave WKT/GeoArrow-native fields alone.
				continue
			}
			if _, already := stamped[i].Metadata.GetValue(gobi.MetaGeometryType); already {
				continue
			}
			stamped[i] = stampGeometryField(stamped[i], cm)
		}
		fields = stamped
	}

	return arrow.NewSchema(fields, &md), nil
}

// stampGeometryField merges the gobi geometry tags derived from cm
// into f's per-field metadata, preserving any pre-existing keys.
func stampGeometryField(f arrow.Field, cm gobi.GeoParquetColumnMeta) arrow.Field {
	encoding := cm.Encoding
	if encoding == "" {
		encoding = "WKB" // GeoParquet 1.1 spec default
	}
	epsg := epsgFromCRS(cm.CRS)

	newKeys := make([]string, 0, f.Metadata.Len()+2)
	newVals := make([]string, 0, f.Metadata.Len()+2)
	for i, k := range f.Metadata.Keys() {
		newKeys = append(newKeys, k)
		newVals = append(newVals, f.Metadata.Values()[i])
	}
	newKeys = append(newKeys, gobi.MetaGeometryType, gobi.MetaGeometryCRS)
	newVals = append(newVals, encoding, strconv.FormatInt(int64(epsg), 10))

	md := arrow.NewMetadata(newKeys, newVals)
	return arrow.Field{
		Name:     f.Name,
		Type:     f.Type,
		Nullable: f.Nullable,
		Metadata: md,
	}
}

// epsgFromCRS pulls the EPSG code from a PROJJSON blob's top-level
// id.{authority: "EPSG", code: N}. GeoParquet 1.1 treats a missing or
// null crs as OGC:CRS84 — same datum and axis order as EPSG:4326 for
// planar-encoded WKB coordinates, so gobi returns 4326 there. Returns
// 0 when the blob is present but doesn't cleanly encode an EPSG code;
// the field is still tagged as geometry but with an unknown CRS.
func epsgFromCRS(crs map[string]any) int32 {
	if len(crs) == 0 {
		return 4326
	}
	id, ok := crs["id"].(map[string]any)
	if !ok {
		return 0
	}
	if auth, _ := id["authority"].(string); auth != "EPSG" {
		return 0
	}
	switch code := id["code"].(type) {
	case float64:
		return int32(code)
	case int:
		return int32(code)
	case int64:
		return int32(code)
	default:
		return 0
	}
}

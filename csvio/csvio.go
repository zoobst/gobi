// Package csvio reads and writes CSV data as gobi Frames.
//
// The reader infers each column's Arrow type from a user-supplied Go struct
// whose fields carry `csv:"header"`, `geom:"true"`, and optional `time:"…"`
// tags. Read parsing is delegated to Arrow's typed CSV reader, which parses
// each column directly into its Arrow buffer — no per-row `[]string`
// intermediary and no per-cell reflection dispatch. Columns tagged as
// geometry or time.Time are read as strings and post-transformed into
// their target types (WKB Binary / Timestamp[ns]) in a single bulk pass
// per column.
package csvio

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	arrowcsv "github.com/apache/arrow-go/v18/arrow/csv"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// Errors.
var (
	ErrUnsupportedFieldType  = errors.New("csvio: unsupported field type")
	ErrHeaderMissing         = errors.New("csvio: expected header row not found in file")
	ErrRowFieldCountMismatch = errors.New("csvio: row field count does not match schema")
	ErrTimeParse             = errors.New("csvio: cannot parse cell as time.Time")
)

// DefaultTimeLayouts is the ordered list of time layouts tried by the
// CSV reader when a time.Time field has no `time:"…"` tag. Callers who
// need a different format should set the tag explicitly rather than
// mutating this slice.
var DefaultTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// DefaultChunkRows is the row batch size Arrow's CSV reader emits when
// Options.ChunkRows is zero. Larger batches amortize per-batch overhead;
// smaller batches bound peak memory. 64k rows is a reasonable middle
// ground for typical wide tables.
const DefaultChunkRows = 64 * 1024

// Options controls CSV parsing.
type Options struct {
	// HasHeader indicates whether the first row is a header. Defaults to true.
	HasHeader *bool
	// Delimiter overrides the default comma. Any rune is accepted, e.g.
	// '\t' for TSV or ';' for European-style CSV.
	Delimiter rune
	// Comment marks lines that should be skipped when starting with this rune.
	Comment rune
	// SkipRows drops the first N data rows (after any header). Implemented
	// by consuming N line-terminated records from the raw stream before
	// the Arrow reader sees them, so quoted embedded newlines inside
	// skipped rows will not be counted correctly — reserve SkipRows for
	// well-behaved (no quoted newlines in the skip zone) input.
	SkipRows int
	// CRSHint gives geometry columns a CRS when the CSV does not encode one.
	CRSHint int32
	// Allocator overrides the Arrow allocator.
	Allocator memory.Allocator
	// Compression selects the stream-compression codec used to decode the
	// input. The zero value (CodecAuto) means "infer from the filename in
	// ReadFile, or treat as uncompressed in Read." Set to CodecNone to
	// force no decompression even if the filename suggests otherwise.
	Compression Codec
	// NullTokens is the set of cell values that decode as SQL null in
	// addition to the empty string (which is always treated as null).
	// Useful for CSVs that write "NA", "NULL", "N/A", etc.
	NullTokens []string
	// LazyQuotes tolerates broken quoting: unescaped quotes inside a
	// quoted field, unquoted fields that begin with a quote, etc. Off
	// by default because it can mask genuinely malformed input.
	LazyQuotes bool
	// UseCRLF signals that record separators are "\r\n" rather than "\n".
	// Rare in practice; Arrow's reader auto-handles most cases without
	// this hint.
	UseCRLF bool
	// ChunkRows overrides DefaultChunkRows. Values ≤ 0 use the default.
	ChunkRows int
}

func (o *Options) hasHeader() bool {
	if o == nil || o.HasHeader == nil {
		return true
	}
	return *o.HasHeader
}

// ReadFile reads path into a Frame, inferring the schema from T. If
// opts.Compression is CodecAuto (the default), the codec is inferred from
// the filename's extension (`.gz`, `.zst`, `.bz2`).
func ReadFile[T any](path string, opts *Options) (*gobi.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Auto-detect the codec from the filename unless the caller has
	// explicitly set one.
	if opts == nil {
		opts = &Options{}
	}
	if opts.Compression == CodecAuto {
		local := *opts
		local.Compression = detectCodecFromPath(path)
		opts = &local
	}
	return Read[T](f, opts)
}

// Read reads r into a Frame, inferring the schema from T. Streams reaching
// this function are treated as uncompressed unless opts.Compression is set
// explicitly (Read has no filename to inspect).
func Read[T any](r io.Reader, opts *Options) (*gobi.Frame, error) {
	if opts == nil {
		opts = &Options{}
	}
	if opts.Compression != CodecAuto && opts.Compression != CodecNone {
		dec, release, err := wrapCodec(r, opts.Compression)
		if err != nil {
			return nil, err
		}
		defer release()
		r = dec
	}

	pool := opts.Allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}

	plan, err := planStruct[T](opts.CRSHint)
	if err != nil {
		return nil, err
	}

	// SkipRows is applied to the raw stream before Arrow's reader sees
	// it, so the header row (if any) still counts against Arrow's own
	// parser rather than being consumed here.
	if opts.SkipRows > 0 {
		br := bufio.NewReader(r)
		for range opts.SkipRows {
			if _, err := br.ReadBytes('\n'); err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
		}
		r = br
	}

	readSchema := arrow.NewSchema(planReadFields(plan), nil)

	arrowOpts := []arrowcsv.Option{
		arrowcsv.WithAllocator(pool),
		arrowcsv.WithHeader(opts.hasHeader()),
		arrowcsv.WithChunk(chunkRows(opts)),
	}
	if opts.Delimiter != 0 {
		arrowOpts = append(arrowOpts, arrowcsv.WithComma(opts.Delimiter))
	}
	if opts.Comment != 0 {
		arrowOpts = append(arrowOpts, arrowcsv.WithComment(opts.Comment))
	}
	if opts.LazyQuotes {
		arrowOpts = append(arrowOpts, arrowcsv.WithLazyQuotes(true))
	}
	if opts.UseCRLF {
		arrowOpts = append(arrowOpts, arrowcsv.WithCRLF(true))
	}
	// Always route empty strings + user-supplied null tokens through the
	// null path. stringsCanBeNull=true keeps the previous behavior of
	// mapping empty String cells to null.
	nullTokens := append([]string{""}, opts.NullTokens...)
	arrowOpts = append(arrowOpts, arrowcsv.WithNullReader(true, nullTokens...))

	rdr := arrowcsv.NewReader(r, readSchema, arrowOpts...)
	defer rdr.Release()

	// Collect record batches; each batch is one chunk-sized arrow.Record.
	var recs []arrow.Record
	defer func() {
		for _, rec := range recs {
			rec.Release()
		}
	}()
	for rdr.Next() {
		rec := rdr.Record()
		rec.Retain()
		recs = append(recs, rec)
	}
	if err := rdr.Err(); err != nil {
		if errors.Is(err, io.EOF) && len(recs) == 0 && opts.hasHeader() {
			return nil, ErrHeaderMissing
		}
		return nil, fmt.Errorf("csvio: %w", err)
	}

	// Concatenate the per-batch arrays into a single array per column,
	// so the resulting Frame has single-chunk columns (matches the fast
	// paths downstream in gobi).
	numCols := len(plan)
	readArrs := make([]arrow.Array, numCols)
	defer func() {
		for _, a := range readArrs {
			if a != nil {
				a.Release()
			}
		}
	}()
	for c := range numCols {
		chunks := make([]arrow.Array, 0, len(recs))
		for _, rec := range recs {
			chunks = append(chunks, rec.Column(c))
		}
		out, err := array.Concatenate(chunks, pool)
		if err != nil {
			return nil, fmt.Errorf("csvio: concatenate column %q: %w", plan[c].outputField.Name, err)
		}
		readArrs[c] = out
	}

	// Transform tagged columns into their output type.
	outArrs := make([]arrow.Array, numCols)
	for i, p := range plan {
		switch p.transform {
		case xformWKTToWKB:
			bin, err := transformWKTColumn(pool, readArrs[i])
			if err != nil {
				return nil, fmt.Errorf("csvio: column %q: %w", p.outputField.Name, err)
			}
			outArrs[i] = bin
		case xformStringToTimestamp:
			ts, err := transformTimeColumn(pool, readArrs[i], p.timeLayout)
			if err != nil {
				return nil, fmt.Errorf("csvio: column %q: %w", p.outputField.Name, err)
			}
			outArrs[i] = ts
		default:
			// No transform: the read array becomes the output array. Retain
			// it so both defers can release safely.
			readArrs[i].Retain()
			outArrs[i] = readArrs[i]
		}
	}
	defer func() {
		for _, a := range outArrs {
			if a != nil {
				a.Release()
			}
		}
	}()

	// Assemble the Frame with the output (post-transform) schema.
	outFields := make([]arrow.Field, numCols)
	for i, p := range plan {
		outFields[i] = p.outputField
	}
	outSchema := arrow.NewSchema(outFields, nil)
	cols := make([]arrow.Column, numCols)
	for i, a := range outArrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(outFields[i], chunked)
	}
	return gobi.NewFrame(outSchema, cols)
}

// chunkRows resolves the effective row batch size.
func chunkRows(opts *Options) int {
	if opts != nil && opts.ChunkRows > 0 {
		return opts.ChunkRows
	}
	return DefaultChunkRows
}

// -----------------------------------------------------------------------------
// Streaming API
//
// ReadChunksFunc / ReadFileChunksFunc yield one Frame per Arrow record
// batch (~64k rows by default). Callers process each batch and return —
// the Frame's underlying Arrow buffers are released when the callback
// returns. This bounds peak memory at ~1 batch regardless of source file
// size, and is the right shape for ETL-style pipelines.
//
// To keep a Frame past the callback boundary, call `frame.Retain()`
// inside the callback and match with `frame.Release()` when done.
// -----------------------------------------------------------------------------

// ErrChunksAborted is returned by ReadChunksFunc when the user's callback
// returned a non-nil error. The original callback error is wrapped so
// callers can `errors.Is` / `errors.As` it out of the chain.
var ErrChunksAborted = errors.New("csvio: chunk callback returned error")

// ReadFileChunksFunc is the file variant of ReadChunksFunc: opens path,
// auto-detects compression from the filename (unless opts.Compression is
// explicitly set), and streams record-batch-sized Frames to fn.
func ReadFileChunksFunc[T any](path string, opts *Options, fn func(*gobi.Frame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if opts == nil {
		opts = &Options{}
	}
	if opts.Compression == CodecAuto {
		local := *opts
		local.Compression = detectCodecFromPath(path)
		opts = &local
	}
	return ReadChunksFunc[T](f, opts, fn)
}

// ReadChunksFunc reads r and invokes fn once per record batch (~64k rows
// by default; override via Options.ChunkRows). The Frame passed to fn has
// single-chunk columns matching the final output schema — tagged
// geometry/time columns have already been post-transformed. Peak memory
// is bounded to roughly one batch plus a working buffer for transforms.
//
// The Frame's Arrow buffers are released after fn returns. To retain a
// Frame past the callback, call frame.Retain() before returning; the
// caller is then responsible for the matching Release().
//
// If fn returns an error, iteration stops and the error is wrapped in
// ErrChunksAborted. If reading fails, the underlying Arrow error is
// returned directly.
func ReadChunksFunc[T any](r io.Reader, opts *Options, fn func(*gobi.Frame) error) error {
	sc, err := setupReader[T](r, opts)
	if err != nil {
		return err
	}
	defer sc.close()

	for sc.rdr.Next() {
		rec := sc.rdr.Record()
		frame, err := recordToFrame(rec, sc.plan, sc.pool, sc.outSchema, sc.outFields)
		if err != nil {
			return err
		}
		cbErr := fn(frame)
		frame.Release()
		if cbErr != nil {
			return fmt.Errorf("%w: %v", ErrChunksAborted, cbErr)
		}
	}
	if err := sc.rdr.Err(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("csvio: %w", err)
	}
	return nil
}

// setupContext bundles the shared reader state used by both Read and
// ReadChunksFunc.
type setupContext struct {
	rdr       *arrowcsv.Reader
	plan      []columnPlan
	pool      memory.Allocator
	outSchema *arrow.Schema
	outFields []arrow.Field
	release   func()
}

func (s *setupContext) close() {
	if s.rdr != nil {
		s.rdr.Release()
	}
	if s.release != nil {
		s.release()
	}
}

// setupReader builds the arrowcsv.Reader + column plan + output schema
// shared by Read and the streaming API. On success, the caller must
// invoke setupContext.close() when done.
func setupReader[T any](r io.Reader, opts *Options) (*setupContext, error) {
	if opts == nil {
		opts = &Options{}
	}
	sc := &setupContext{release: func() {}}
	if opts.Compression != CodecAuto && opts.Compression != CodecNone {
		dec, release, err := wrapCodec(r, opts.Compression)
		if err != nil {
			return nil, err
		}
		sc.release = release
		r = dec
	}

	sc.pool = opts.Allocator
	if sc.pool == nil {
		sc.pool = memory.DefaultAllocator
	}

	plan, err := planStruct[T](opts.CRSHint)
	if err != nil {
		sc.close()
		return nil, err
	}
	sc.plan = plan

	if opts.SkipRows > 0 {
		br := bufio.NewReader(r)
		for range opts.SkipRows {
			if _, err := br.ReadBytes('\n'); err != nil {
				if err == io.EOF {
					break
				}
				sc.close()
				return nil, err
			}
		}
		r = br
	}

	readSchema := arrow.NewSchema(planReadFields(plan), nil)
	arrowOpts := []arrowcsv.Option{
		arrowcsv.WithAllocator(sc.pool),
		arrowcsv.WithHeader(opts.hasHeader()),
		arrowcsv.WithChunk(chunkRows(opts)),
	}
	if opts.Delimiter != 0 {
		arrowOpts = append(arrowOpts, arrowcsv.WithComma(opts.Delimiter))
	}
	if opts.Comment != 0 {
		arrowOpts = append(arrowOpts, arrowcsv.WithComment(opts.Comment))
	}
	if opts.LazyQuotes {
		arrowOpts = append(arrowOpts, arrowcsv.WithLazyQuotes(true))
	}
	if opts.UseCRLF {
		arrowOpts = append(arrowOpts, arrowcsv.WithCRLF(true))
	}
	nullTokens := append([]string{""}, opts.NullTokens...)
	arrowOpts = append(arrowOpts, arrowcsv.WithNullReader(true, nullTokens...))

	sc.rdr = arrowcsv.NewReader(r, readSchema, arrowOpts...)

	sc.outFields = make([]arrow.Field, len(plan))
	for i, p := range plan {
		sc.outFields[i] = p.outputField
	}
	sc.outSchema = arrow.NewSchema(sc.outFields, nil)
	return sc, nil
}

// recordToFrame applies per-column transforms to one arrow.Record and
// wraps the result in a Frame with the output schema. Ownership: the
// returned Frame owns its arrays (they are Retain'd or freshly-built).
// Release the Frame to drop those refs.
func recordToFrame(
	rec arrow.Record,
	plan []columnPlan,
	pool memory.Allocator,
	outSchema *arrow.Schema,
	outFields []arrow.Field,
) (*gobi.Frame, error) {
	numCols := len(plan)
	outArrs := make([]arrow.Array, numCols)
	for i, p := range plan {
		col := rec.Column(i)
		switch p.transform {
		case xformWKTToWKB:
			a, err := transformWKTColumn(pool, col)
			if err != nil {
				return nil, fmt.Errorf("csvio: column %q: %w", p.outputField.Name, err)
			}
			outArrs[i] = a
		case xformStringToTimestamp:
			a, err := transformTimeColumn(pool, col, p.timeLayout)
			if err != nil {
				return nil, fmt.Errorf("csvio: column %q: %w", p.outputField.Name, err)
			}
			outArrs[i] = a
		default:
			col.Retain()
			outArrs[i] = col
		}
	}

	cols := make([]arrow.Column, numCols)
	for i, a := range outArrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(outFields[i], chunked)
		// The Column holds its own ref via the chunked; release ours so
		// the Frame is the sole owner.
		a.Release()
	}
	return gobi.NewFrame(outSchema, cols)
}

// -----------------------------------------------------------------------------
// Schema / plan
// -----------------------------------------------------------------------------

// transformKind identifies a post-parse column transformation.
type transformKind uint8

const (
	xformNone transformKind = iota
	// xformWKTToWKB: parse each cell as WKT and emit WKB Binary.
	xformWKTToWKB
	// xformStringToTimestamp: parse each cell via time.Parse and emit
	// Timestamp[ns].
	xformStringToTimestamp
)

// columnPlan describes one column both as it should be read by Arrow's
// CSV reader (readField) and as it should appear in the final Frame
// (outputField). For most columns these are the same; geometry and time
// columns read as String and transform after.
type columnPlan struct {
	outputField arrow.Field
	readField   arrow.Field
	transform   transformKind
	timeLayout  string
}

func planReadFields(plan []columnPlan) []arrow.Field {
	out := make([]arrow.Field, len(plan))
	for i, p := range plan {
		out[i] = p.readField
	}
	return out
}

// planStruct reflects on T and produces one columnPlan per exported field.
func planStruct[T any](crsHint int32) ([]columnPlan, error) {
	var zero T
	tp := reflect.TypeOf(zero)
	if tp.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: T must be a struct, got %s", ErrUnsupportedFieldType, tp.Kind())
	}
	out := make([]columnPlan, 0, tp.NumField())
	timeType := reflect.TypeFor[time.Time]()
	for sf := range tp.Fields() {
		if !sf.IsExported() {
			continue
		}
		name := sf.Tag.Get("csv")
		if name == "" {
			name = sf.Name
		}
		if sf.Tag.Get("geom") == "true" {
			// Read as String, emit as Binary WKB tagged with the geometry
			// metadata so downstream code recognizes it as a geometry column.
			out = append(out, columnPlan{
				outputField: gobi.GeometryField(name, crsHint),
				readField: arrow.Field{
					Name: name, Type: arrow.BinaryTypes.String, Nullable: true,
				},
				transform: xformWKTToWKB,
			})
			continue
		}
		if sf.Type == timeType {
			layout := sf.Tag.Get("time")
			out = append(out, columnPlan{
				outputField: arrow.Field{
					Name: name, Type: &arrow.TimestampType{Unit: arrow.Nanosecond}, Nullable: true,
				},
				readField: arrow.Field{
					Name: name, Type: arrow.BinaryTypes.String, Nullable: true,
				},
				transform:  xformStringToTimestamp,
				timeLayout: layout,
			})
			continue
		}
		dt, err := arrowTypeFor(sf.Type)
		if err != nil {
			return nil, err
		}
		f := arrow.Field{Name: name, Type: dt, Nullable: true}
		out = append(out, columnPlan{outputField: f, readField: f})
	}
	return out, nil
}

func arrowTypeFor(t reflect.Type) (arrow.DataType, error) {
	switch t.Kind() {
	case reflect.String:
		return arrow.BinaryTypes.String, nil
	case reflect.Bool:
		return arrow.FixedWidthTypes.Boolean, nil
	case reflect.Int, reflect.Int64:
		return arrow.PrimitiveTypes.Int64, nil
	case reflect.Int32:
		return arrow.PrimitiveTypes.Int32, nil
	case reflect.Float32:
		return arrow.PrimitiveTypes.Float32, nil
	case reflect.Float64:
		return arrow.PrimitiveTypes.Float64, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFieldType, t.Kind())
	}
}

// -----------------------------------------------------------------------------
// Column transforms
// -----------------------------------------------------------------------------

// transformWKTColumn walks a String array and emits a Binary array whose
// non-null values are WKB-encoded from the input's WKT cells.
func transformWKTColumn(pool memory.Allocator, src arrow.Array) (arrow.Array, error) {
	strArr, ok := src.(*array.String)
	if !ok {
		return nil, fmt.Errorf("%w: geometry column not String, got %T",
			ErrUnsupportedFieldType, src)
	}
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()
	n := strArr.Len()
	for i := range n {
		if strArr.IsNull(i) {
			b.AppendNull()
			continue
		}
		v := strings.TrimSpace(strArr.Value(i))
		if v == "" {
			b.AppendNull()
			continue
		}
		g, err := geometry.ParseWKT(v)
		if err != nil {
			return nil, err
		}
		b.Append(geometry.WKB(g))
	}
	return b.NewArray(), nil
}

// transformTimeColumn walks a String array and emits a Timestamp[ns]
// array by parsing each cell with parseTime.
func transformTimeColumn(pool memory.Allocator, src arrow.Array, layout string) (arrow.Array, error) {
	strArr, ok := src.(*array.String)
	if !ok {
		return nil, fmt.Errorf("%w: time column not String, got %T",
			ErrUnsupportedFieldType, src)
	}
	b := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Nanosecond})
	defer b.Release()
	n := strArr.Len()
	for i := range n {
		if strArr.IsNull(i) {
			b.AppendNull()
			continue
		}
		v := strings.TrimSpace(strArr.Value(i))
		if v == "" {
			b.AppendNull()
			continue
		}
		t, err := parseTime(v, layout)
		if err != nil {
			return nil, err
		}
		b.Append(arrow.Timestamp(t.UnixNano()))
	}
	return b.NewArray(), nil
}

// parseTime tries the explicit layout first when present; otherwise it
// walks DefaultTimeLayouts and returns on the first success.
func parseTime(raw, layout string) (time.Time, error) {
	if layout != "" {
		t, err := time.Parse(layout, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %q with layout %q: %v",
				ErrTimeParse, raw, layout, err)
		}
		return t, nil
	}
	for _, l := range DefaultTimeLayouts {
		if t, err := time.Parse(l, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: %q (tried %d default layouts)",
		ErrTimeParse, raw, len(DefaultTimeLayouts))
}

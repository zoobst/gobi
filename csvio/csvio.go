// Package csvio reads and writes CSV data as gobi Frames.
//
// The reader infers each column's Arrow type from a user-supplied Go struct
// whose fields carry `csv:"header"`, `geom:"true"`, and optional `time:"…"`
// tags. Fields find their columns by header name, so the file's column
// order doesn't matter and columns the struct doesn't name are skipped;
// a field with no column is ErrColumnMissing. Without a header, or with
// ReadOptions.MatchByPosition, columns are taken in field order. Read parsing is delegated to Arrow's typed CSV reader, which parses
// each column directly into its Arrow buffer — no per-row `[]string`
// intermediary and no per-cell reflection dispatch. Columns tagged as
// geometry or time.Time are read as strings and post-transformed into
// their target types (WKB Binary / Timestamp[ns]) in a single bulk pass
// per column.
//
// For CSVs whose columns aren't known at compile time, ReadStrings and
// its file / streaming variants take columns from the header and read
// every cell as a String, with no type inference.
//
// Write, WriteFile and WriteStructs write a Frame (or structs) as CSV
// in formats the readers parse back; NewWriter streams frames to one
// file in pieces.
package csvio

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
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
	// ErrColumnMissing: a struct field's column isn't in the header.
	ErrColumnMissing = errors.New("csvio: header has no column for struct field")
	// ErrDuplicateField: two struct fields name the same header column.
	ErrDuplicateField = errors.New("csvio: struct fields share a column name")
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
// ReadOptions.ChunkRows is zero. Larger batches amortize per-batch overhead;
// smaller batches bound peak memory. 64k rows is a reasonable middle
// ground for typical wide tables.
const DefaultChunkRows = 64 * 1024

// ReadOptions controls CSV parsing.
type ReadOptions struct {
	// HasHeader indicates whether the first row is a header. Defaults to true.
	// With a header, the typed readers (Read, ReadChunksFunc, ScanFile, …)
	// match struct fields to columns by name; without one, by position.
	HasHeader *bool
	// MatchByPosition makes the typed readers take columns in struct
	// field order even when there's a header, which is then skipped
	// unread. The file must have exactly one column per field. For files
	// whose header names don't match the struct's tags.
	MatchByPosition bool
	// Delimiter overrides the default comma. Any rune is accepted, e.g.
	// '\t' for TSV or ';' for European-style CSV.
	Delimiter rune
	// Comment marks lines that should be skipped when starting with this rune.
	Comment rune
	// SkipRows drops the first N raw lines of the input, before the
	// header (or before the data, without one) — for preambles such as
	// a title line above the header. Lines are counted by "\n", so a
	// quoted field with an embedded newline in the skipped lines throws
	// the count off. Parse errors still report line numbers from the
	// start of the input.
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
	// UseCRLF has no effect: "\r\n" and "\n" line endings are both
	// read either way. Kept so existing code compiles.
	UseCRLF bool
	// ChunkRows overrides DefaultChunkRows. Values ≤ 0 use the default.
	ChunkRows int
}

func (o *ReadOptions) hasHeader() bool {
	if o == nil || o.HasHeader == nil {
		return true
	}
	return *o.HasHeader
}

// ReadFile reads path into a Frame, inferring the schema from T. If
// opts.Compression is CodecAuto (the default), the codec is inferred from
// the filename's extension (`.gz`, `.zst`, `.bz2`).
func ReadFile[T any](path string, opts *ReadOptions) (*gobi.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Auto-detect the codec from the filename unless the caller has
	// explicitly set one.
	if opts == nil {
		opts = &ReadOptions{}
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
func Read[T any](r io.Reader, opts *ReadOptions) (*gobi.Frame, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	sc, err := setupReader[T](r, opts)
	if err != nil {
		return nil, err
	}
	defer sc.close()
	if sc.empty {
		return nil, ErrHeaderMissing
	}
	pool, plan, rdr := sc.pool, sc.plan, sc.rdr

	// Collect each plan column's batches. Only the struct's columns are
	// kept: a header column the struct doesn't name is dropped with
	// its batch.
	chunks := make([][]arrow.Array, len(plan))
	defer func() {
		for _, cs := range chunks {
			for _, a := range cs {
				a.Release()
			}
		}
	}()
	batches := 0
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for c := range plan {
			a := rec.Column(sc.cols[c])
			a.Retain()
			chunks[c] = append(chunks[c], a)
		}
		batches++
	}
	if err := rdr.Err(); err != nil {
		if errors.Is(err, io.EOF) && batches == 0 {
			return nil, ErrHeaderMissing
		}
		return nil, sc.readErr(err)
	}

	// Concatenate the per-batch arrays into a single array per column,
	// so the resulting Frame has single-chunk columns (matches the fast
	// paths downstream in gobi).
	readArrs := make([]arrow.Array, len(plan))
	defer func() {
		for _, a := range readArrs {
			if a != nil {
				a.Release()
			}
		}
	}()
	for c := range plan {
		if batches == 0 { // header only
			readArrs[c] = array.MakeArrayOfNull(pool, plan[c].readField.Type, 0)
			continue
		}
		out, err := array.Concatenate(chunks[c], pool)
		if err != nil {
			return nil, fmt.Errorf("csvio: concatenate column %q: %w", plan[c].outputField.Name, err)
		}
		readArrs[c] = out
		for _, a := range chunks[c] {
			a.Release()
		}
		chunks[c] = nil
	}
	return sc.buildFrame(func(i int) arrow.Array { return readArrs[i] })
}

// skipBOM drops a UTF-8 byte-order mark (as Excel writes) from the
// start of r. It has to go before the CSV parser runs: left in, it
// makes a quoted first header cell parse as an unquoted field holding
// a stray quote, which is a parse error.
func skipBOM(r io.Reader) *bufio.Reader {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && string(b) == "\xef\xbb\xbf" {
		_, _ = br.Discard(3)
	}
	return br
}

// chunkRows resolves the effective row batch size.
func chunkRows(opts *ReadOptions) int {
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
func ReadFileChunksFunc[T any](path string, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if opts == nil {
		opts = &ReadOptions{}
	}
	if opts.Compression == CodecAuto {
		local := *opts
		local.Compression = detectCodecFromPath(path)
		opts = &local
	}
	return ReadChunksFunc[T](f, opts, fn)
}

// ReadChunksFunc reads r and invokes fn once per record batch (~64k rows
// by default; override via ReadOptions.ChunkRows). The Frame passed to fn has
// single-chunk columns matching the final output schema — tagged
// geometry/time columns have already been post-transformed. Peak memory
// is bounded to roughly one batch plus a working buffer for transforms.
//
// The Frame's Arrow buffers are released after fn returns. To retain a
// Frame past the callback, call frame.Retain() before returning; the
// caller is then responsible for the matching Release().
//
// If fn returns an error, iteration stops and the error is wrapped in
// ErrChunksAborted; errors.Is / errors.As match both. Empty input
// yields no chunks and no error.
func ReadChunksFunc[T any](r io.Reader, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	sc, err := setupReader[T](r, opts)
	if err != nil {
		return err
	}
	defer sc.close()
	if sc.empty {
		return nil
	}
	batches := 0
	for sc.rdr.Next() {
		batches++
		rec := sc.rdr.RecordBatch()
		frame, err := sc.buildFrame(func(i int) arrow.Array { return rec.Column(sc.cols[i]) })
		if err != nil {
			return err
		}
		cbErr := fn(frame)
		frame.Release()
		if cbErr != nil {
			return fmt.Errorf("%w: %w", ErrChunksAborted, cbErr)
		}
	}
	if err := sc.rdr.Err(); err != nil {
		if errors.Is(err, io.EOF) && batches == 0 {
			return nil // empty input
		}
		return sc.readErr(err)
	}
	return nil
}

// setupContext bundles the shared reader state used by both Read and
// ReadChunksFunc.
type setupContext struct {
	rdr       *arrowcsv.Reader
	plan      []columnPlan
	cols      []int // per plan column: its column in rdr's records
	lines     int   // input lines consumed before rdr's first
	empty     bool  // no input at all (rdr is nil)
	pool      memory.Allocator
	outSchema *arrow.Schema
	outFields []arrow.Field
	release   func()
}

// readErr maps an error from rdr: parse errors get line numbers
// counted from the start of the input rather than from where rdr
// began. (Arrow reports empty input, when it reads the header itself,
// as an io.EOF error before any batch; callers check for that first.)
func (s *setupContext) readErr(err error) error {
	return csvErr(shiftLines(err, s.lines))
}

// buildFrame turns one batch into a Frame: read(i) is plan column i as
// read (owned by the caller), which is transformed to its output type.
func (s *setupContext) buildFrame(read func(i int) arrow.Array) (*gobi.Frame, error) {
	out := make([]arrow.Array, 0, len(s.plan))
	defer func() {
		for _, a := range out {
			a.Release()
		}
	}()
	for i, p := range s.plan {
		a, err := transformColumn(s.pool, p, read(i))
		if err != nil {
			return nil, fmt.Errorf("csvio: column %q: %w", p.outputField.Name, err)
		}
		out = append(out, a)
	}
	cols := make([]arrow.Column, len(out))
	for i, a := range out {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(s.outFields[i], chunked)
		chunked.Release()
	}
	return gobi.NewFrame(s.outSchema, cols)
}

// transformColumn converts a column as read into its output type,
// returning a new reference.
func transformColumn(pool memory.Allocator, p columnPlan, col arrow.Array) (arrow.Array, error) {
	switch p.transform {
	case xformWKTToWKB:
		return transformWKTColumn(pool, col)
	case xformStringToTimestamp:
		return transformTimeColumn(pool, col, p.timeLayout)
	}
	col.Retain()
	return col, nil
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
func setupReader[T any](r io.Reader, opts *ReadOptions) (*setupContext, error) {
	if opts == nil {
		opts = &ReadOptions{}
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

	br := skipBOM(r)
	if sc.lines, err = skipLines(br, opts.SkipRows); err != nil {
		sc.close()
		return nil, err
	}

	readFields := planReadFields(plan)
	sc.cols = make([]int, len(plan))
	for i := range sc.cols {
		sc.cols[i] = i
	}
	header := opts.hasHeader()
	if header && !opts.MatchByPosition {
		var lines int
		readFields, sc.cols, lines, err = matchHeader(br, plan, opts)
		if errors.Is(err, ErrHeaderMissing) {
			// Empty input: no reader; Read reports it, the streaming
			// readers yield no chunks.
			sc.empty = true
			sc.setOutSchema()
			return sc, nil
		}
		if err != nil {
			sc.close()
			return nil, shiftLines(err, sc.lines)
		}
		sc.lines += lines
		header = false // consumed by matchHeader
	}

	readSchema := arrow.NewSchema(readFields, nil)
	arrowOpts := []arrowcsv.Option{
		arrowcsv.WithAllocator(sc.pool),
		arrowcsv.WithHeader(header),
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
	// Empty strings and the caller's null tokens read as null.
	nullTokens := append([]string{""}, opts.NullTokens...)
	arrowOpts = append(arrowOpts, arrowcsv.WithNullReader(true, nullTokens...))

	// br is shared with matchHeader's csv.Reader and Arrow's:
	// csv.NewReader reuses a *bufio.Reader it's given, so Arrow starts
	// exactly where the header ended.
	sc.rdr = arrowcsv.NewReader(br, readSchema, arrowOpts...)

	sc.setOutSchema()
	return sc, nil
}

func (s *setupContext) setOutSchema() {
	s.outFields = make([]arrow.Field, len(s.plan))
	for i, p := range s.plan {
		s.outFields[i] = p.outputField
	}
	s.outSchema = arrow.NewSchema(s.outFields, nil)
}

// matchHeader reads the header record from br and lines plan's
// columns up with it by name. It returns the fields to read, in file
// order — matched columns typed from the plan, the others read as
// String and dropped — per plan column, its position in the file, and
// the number of lines the header took.
//
// br is left at the first data record: encoding/csv reads through br
// itself (csv.NewReader reuses a *bufio.Reader) one line at a time,
// never past the record it returns, so a quoted header cell spanning
// lines is handled too.
func matchHeader(br *bufio.Reader, plan []columnPlan, opts *ReadOptions) ([]arrow.Field, []int, int, error) {
	cr := newCSVReader(br, opts)
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, 0, ErrHeaderMissing
	}
	if err != nil {
		return nil, nil, 0, csvErr(err)
	}
	// The header ends on the line its last field starts on, plus that
	// field's own line breaks (comment and blank lines before it are
	// in FieldPos's count).
	last := len(header) - 1
	lines, _ := cr.FieldPos(last)
	lines += strings.Count(header[last], "\n")
	// Each field takes the column whose header equals its name exactly,
	// or else the one column equal to it ignoring case and surrounding
	// spaces ("ID" finds "id", "name" finds " name").
	exact := make(map[string][]int, len(header))
	folded := make(map[string][]int, len(header))
	fields := make([]arrow.Field, len(header))
	for i, name := range header {
		exact[name] = append(exact[name], i)
		folded[foldHeader(name)] = append(folded[foldHeader(name)], i)
		fields[i] = arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true}
	}
	cols := make([]int, len(plan))
	var missing []string
	taken := make(map[int]string, len(plan))
	for i, p := range plan {
		name := p.readField.Name
		at := exact[name]
		if len(at) == 0 {
			at = folded[foldHeader(name)]
		}
		switch {
		case len(at) == 0:
			missing = append(missing, strconv.Quote(name))
			continue
		case len(at) > 1:
			return nil, nil, 0, fmt.Errorf("%w: %q matches %d columns", ErrDuplicateHeader, name, len(at))
		}
		j := at[0]
		if other, ok := taken[j]; ok {
			return nil, nil, 0, fmt.Errorf("%w: %q and %q both read column %q", ErrDuplicateField, other, name, header[j])
		}
		taken[j] = name
		fields[j] = p.readField
		cols[i] = j
	}
	if len(missing) > 0 {
		return nil, nil, 0, fmt.Errorf("%w: %s (ReadOptions.MatchByPosition reads columns by position instead)",
			ErrColumnMissing, strings.Join(missing, ", "))
	}
	return fields, cols, lines, nil
}

// foldHeader is the case- and space-insensitive form of a header or
// field name.
func foldHeader(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// newCSVReader is the encoding/csv reader both the header match and
// ReadStrings parse with, configured from opts the same way Arrow's
// reader is.
func newCSVReader(br *bufio.Reader, opts *ReadOptions) *csv.Reader {
	cr := csv.NewReader(br)
	cr.LazyQuotes = opts.LazyQuotes
	if opts.Delimiter != 0 {
		cr.Comma = opts.Delimiter
	}
	if opts.Comment != 0 {
		cr.Comment = opts.Comment
	}
	return cr
}

// skipLines drops n raw lines from br (ReadOptions.SkipRows) and
// returns how many it dropped, fewer at end of input.
func skipLines(br *bufio.Reader, n int) (int, error) {
	for i := range n {
		if _, err := br.ReadBytes('\n'); err != nil {
			if err == io.EOF {
				return i, nil
			}
			return i, err
		}
	}
	return n, nil
}

// shiftLines adds n to the line numbers of a parse error from a reader
// that started n lines into the input.
func shiftLines(err error, n int) error {
	var pe *csv.ParseError
	if n != 0 && errors.As(err, &pe) {
		pe.StartLine += n
		pe.Line += n
	}
	return err
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
		name, skip := gobi.ResolveFieldName(sf, "csv")
		if skip {
			continue
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
		ts, err := gobi.TimestampOf(t, arrow.Nanosecond)
		if err != nil {
			return nil, fmt.Errorf("%w (read the column into a string field and convert it with gobi's StrToTime, which keeps microseconds)", err)
		}
		b.Append(ts)
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

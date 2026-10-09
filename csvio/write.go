package csvio

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/klauspost/compress/zstd"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// ErrInvalidDelimiter is returned when WriteOptions.Delimiter can't
// separate CSV fields: a quote, carriage return, newline, or an
// invalid rune.
var ErrInvalidDelimiter = errors.New("csvio: invalid delimiter")

// DefaultDateFormat is the layout Date32 and Date64 cells are written
// with when WriteOptions.DateFormat is empty.
const DefaultDateFormat = "2006-01-02"

// WriteOptions controls CSV output. A nil *WriteOptions uses the
// defaults.
type WriteOptions struct {
	// HasHeader writes the column names as the first row. Defaults to
	// true.
	HasHeader *bool
	// Delimiter overrides the default comma, e.g. '\t' for TSV.
	Delimiter rune
	// UseCRLF ends records with "\r\n" instead of "\n".
	UseCRLF bool
	// NullValue is written for null cells. The default, an empty field,
	// is what Read and ReadStrings read back as null. A non-empty value
	// is ambiguous with a string cell holding the same text.
	NullValue string
	// TimeFormat is the time.Format layout for Timestamp cells, written
	// in the column's time zone (UTC for a column without one). Defaults
	// to time.RFC3339Nano, the first layout Read tries.
	TimeFormat string
	// DateFormat is the layout for Date32 and Date64 cells. Defaults to
	// DefaultDateFormat.
	DateFormat string
	// Compression selects the stream codec. The zero value (CodecAuto)
	// infers it from the filename in WriteFile and writes uncompressed
	// in Write and NewWriter. CodecBzip2 can't be written: the Go
	// standard library has no bzip2 encoder.
	Compression Codec
}

func (o *WriteOptions) hasHeader() bool {
	if o == nil || o.HasHeader == nil {
		return true
	}
	return *o.HasHeader
}

// WriteFile writes f to path as CSV. With opts.Compression unset, a
// .gz / .zst / .zstd name gzip- or zstd-compresses the output; a .bz2
// name is an error. See Write for how each column type is written.
func WriteFile(f *gobi.Frame, path string, opts *WriteOptions) (err error) {
	o := WriteOptions{}
	if opts != nil {
		o = *opts
	}
	if o.Compression == CodecAuto {
		o.Compression = detectCodecFromPath(path)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); err == nil {
			err = cerr
		}
	}()
	return Write(f, file, &o)
}

// Write writes f to w as CSV: a header row of the column names (unless
// opts.HasHeader is false), then one record per row. Fields are quoted
// as encoding/csv quotes them.
//
// Cell formats, chosen so Read and ReadStrings read them back:
//
//   - Null cells: opts.NullValue (default: empty). An empty string cell
//     is also written empty, so it reads back as null.
//   - Strings: as is, except that a CR LF inside a value reads back as
//     LF (the reader normalizes it). Booleans: true / false.
//   - Integers: decimal. Floats: the shortest form that parses back to
//     the same value, with NaN, +Inf and -Inf spelled that way.
//     Decimals: with the column's scale, e.g. 12.50.
//   - Timestamp: opts.TimeFormat in the column's zone, UTC when it has
//     none. Date32 / Date64: opts.DateFormat. Time32 / Time64:
//     15:04:05.999999999. Duration: the integer count in the column's
//     unit.
//   - Geometry columns: WKT. The CRS isn't written; pass it back as
//     ReadOptions.CRSHint.
//   - Other binary columns: standard base64.
//   - Lists, structs and maps: JSON.
//   - Dictionary columns: the dictionary value.
//
// Any other type is written as Arrow formats it (Array.ValueStr).
func Write(f *gobi.Frame, w io.Writer, opts *WriteOptions) error {
	if f == nil {
		return fmt.Errorf("csvio: Write: nil frame")
	}
	cw, err := NewWriter(w, f.Schema(), opts)
	if err != nil {
		return err
	}
	if err := cw.Write(f); err != nil {
		_ = cw.Close()
		return err
	}
	return cw.Close()
}

// WriteStructs writes rows to path as CSV, one column per field.
// Column names and options come from gobi's tag resolution under the
// "csv" namespace, as in ReadStructs; structOpts are passed to
// gobi.FromStructs. Wraps gobi.FromStructs + WriteFile, so a
// `geom:"true"` string field is written as WKT and a zero time.Time
// as null.
func WriteStructs[T any](rows []T, path string, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("csv")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteFile(f, path, opts)
}

// WriteStructsWriter is the io.Writer counterpart to WriteStructs.
func WriteStructsWriter[T any](rows []T, w io.Writer, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("csv")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return Write(f, w, opts)
}

// Writer writes frames to one CSV stream in pieces, e.g. from
// ReadFileChunksFunc or a LazyFrame's batches, so the whole table
// never has to be in memory. The output is what Write produces for
// all the frames concatenated.
//
// Not safe for concurrent use. After any error the Writer is unusable:
// later calls return the same error.
type Writer struct {
	bw      *bufio.Writer
	closeFn func() error // finishes the compression stream, if any
	schema  *arrow.Schema

	comma     rune
	crlf      bool
	null      []byte
	timeFmt   string
	dateFmt   string
	rows      int64
	scratch   []byte
	locations map[string]*time.Location // per time zone name

	closed bool
	err    error // sticky
}

// NewWriter starts a CSV stream on w for frames with the given schema,
// writing the header row now (unless opts.HasHeader is false), so a
// stream closed after no Write is header-only. Invalid options fail
// here. Close finishes the stream but doesn't close w.
func NewWriter(w io.Writer, schema *arrow.Schema, opts *WriteOptions) (*Writer, error) {
	if schema == nil || schema.NumFields() == 0 {
		return nil, fmt.Errorf("csvio: NewWriter: schema has no columns")
	}
	o := WriteOptions{}
	if opts != nil {
		o = *opts
	}
	comma := o.Delimiter
	if comma == 0 {
		comma = ','
	}
	if comma == '"' || comma == '\r' || comma == '\n' || !utf8.ValidRune(comma) || comma == utf8.RuneError {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDelimiter, comma)
	}
	out, closeFn, err := wrapEncoder(w, o.Compression)
	if err != nil {
		return nil, err
	}
	cw := &Writer{
		bw:        bufio.NewWriterSize(out, 64<<10),
		closeFn:   closeFn,
		schema:    schema,
		comma:     comma,
		crlf:      o.UseCRLF,
		null:      []byte(o.NullValue),
		timeFmt:   o.TimeFormat,
		dateFmt:   o.DateFormat,
		locations: map[string]*time.Location{},
	}
	if cw.timeFmt == "" {
		cw.timeFmt = time.RFC3339Nano
	}
	if cw.dateFmt == "" {
		cw.dateFmt = DefaultDateFormat
	}
	if o.hasHeader() {
		for i, fld := range schema.Fields() {
			if i > 0 {
				cw.bw.WriteRune(comma)
			}
			cw.writeField([]byte(fld.Name))
		}
		cw.endRecord()
	}
	return cw, nil
}

// wrapEncoder returns w wrapped in codec's encoder and a func that
// finishes the compressed stream (without closing w).
func wrapEncoder(w io.Writer, codec Codec) (io.Writer, func() error, error) {
	switch codec {
	case CodecAuto, CodecNone:
		return w, func() error { return nil }, nil
	case CodecGzip:
		gz := gzip.NewWriter(w)
		return gz, gz.Close, nil
	case CodecZstd:
		z, err := zstd.NewWriter(w)
		if err != nil {
			return nil, nil, fmt.Errorf("csvio: zstd: %w", err)
		}
		return z, z.Close, nil
	case CodecBzip2:
		return nil, nil, fmt.Errorf("%w: %q can be read but not written (the standard library has no bzip2 encoder)", ErrUnknownCodec, codec)
	default:
		return nil, nil, fmt.Errorf("%w: %q", ErrUnknownCodec, codec)
	}
}

// Write appends f's rows. f's schema must match the Writer's (same
// fields, types and field metadata). Zero-row frames are a no-op. The
// Writer doesn't retain f.
func (w *Writer) Write(f *gobi.Frame) error {
	if err := w.usable(); err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("csvio: Writer.Write: nil frame")
	}
	if !f.Schema().Equal(w.schema) {
		return fmt.Errorf("csvio: Writer.Write: frame schema does not match the writer's\nframe:  %s\nwriter: %s",
			f.Schema(), w.schema)
	}
	n := f.NumRows()
	if n == 0 {
		return nil
	}
	cols := make([]colCursor, f.NumCols())
	for j := range cols {
		s, err := f.ColumnAt(j)
		if err != nil {
			return w.fail(err)
		}
		cols[j] = colCursor{chunks: s.Column().Data().Chunks(), geom: s.IsGeometry(), c: -1}
	}
	for r := 0; r < n; r++ {
		for j := range cols {
			if j > 0 {
				w.bw.WriteRune(w.comma)
			}
			if err := w.writeCell(&cols[j]); err != nil {
				return w.fail(fmt.Errorf("csvio: Writer.Write: row %d column %q: %w", w.rows+int64(r), w.schema.Field(j).Name, err))
			}
		}
		w.endRecord()
	}
	w.rows += int64(n)
	return nil
}

// NumRows reports the data rows written so far (not counting the
// header).
func (w *Writer) NumRows() int64 { return w.rows }

// Close flushes buffered output and finishes the compression stream.
// It doesn't close the io.Writer passed to NewWriter. Calling it again
// returns nil.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.err
	if ferr := w.bw.Flush(); err == nil {
		err = ferr
	}
	if cerr := w.closeFn(); err == nil {
		err = cerr
	}
	return err
}

func (w *Writer) usable() error {
	switch {
	case w.err != nil:
		return w.err
	case w.closed:
		return fmt.Errorf("csvio: Writer is closed")
	}
	return nil
}

func (w *Writer) fail(err error) error {
	w.err = err
	return err
}

// colCursor walks one column's chunks in row order. Chunk boundaries
// differ between columns, so each column keeps its own position.
type colCursor struct {
	chunks []arrow.Array
	geom   bool
	c      int // current chunk, -1 before the first cell
	local  int // next row within chunks[c]
	fn     cellFn
}

// cellFn appends the non-null cell i of a chunk to dst. null reports a
// cell that turned out to be null (a dictionary index pointing at a
// null entry).
type cellFn func(dst []byte, i int) (out []byte, null bool, err error)

// writeCell writes the column's next cell and advances the cursor.
func (w *Writer) writeCell(cc *colCursor) error {
	for cc.c < 0 || cc.local >= cc.chunks[cc.c].Len() {
		cc.c++
		cc.local = 0
		if cc.c >= len(cc.chunks) {
			return fmt.Errorf("column shorter than the frame")
		}
		fn, err := w.cellFormatter(cc.chunks[cc.c], cc.geom)
		if err != nil {
			return err
		}
		cc.fn = fn
	}
	arr, i := cc.chunks[cc.c], cc.local
	cc.local++
	if arr.IsNull(i) {
		w.writeField(w.null)
		return nil
	}
	out, null, err := cc.fn(w.scratch[:0], i)
	if err != nil {
		return err
	}
	w.scratch = out
	if null {
		out = w.null
	}
	w.writeField(out)
	return nil
}

// cellFormatter returns the appender for arr's type. geom marks a
// geometry column, whose WKB cells are written as WKT.
func (w *Writer) cellFormatter(arr arrow.Array, geom bool) (cellFn, error) {
	if geom {
		if bin, ok := arr.(interface{ Value(int) []byte }); ok {
			return func(dst []byte, i int) ([]byte, bool, error) {
				g, err := geometry.ParseWKB(bin.Value(i))
				if err != nil {
					return dst, false, fmt.Errorf("parse WKB: %w", err)
				}
				return append(dst, g.WKT()...), false, nil
			}, nil
		}
	}
	switch a := arr.(type) {
	case *array.String:
		return func(dst []byte, i int) ([]byte, bool, error) { return append(dst, a.Value(i)...), false, nil }, nil
	case *array.LargeString:
		return func(dst []byte, i int) ([]byte, bool, error) { return append(dst, a.Value(i)...), false, nil }, nil
	case *array.StringView:
		return func(dst []byte, i int) ([]byte, bool, error) { return append(dst, a.Value(i)...), false, nil }, nil
	case *array.Boolean:
		return func(dst []byte, i int) ([]byte, bool, error) { return strconv.AppendBool(dst, a.Value(i)), false, nil }, nil
	case *array.Int64:
		return intFn(a.Int64Values()), nil
	case *array.Int32:
		return intFn(a.Int32Values()), nil
	case *array.Int16:
		return intFn(a.Int16Values()), nil
	case *array.Int8:
		return intFn(a.Int8Values()), nil
	case *array.Uint64:
		return uintFn(a.Uint64Values()), nil
	case *array.Uint32:
		return uintFn(a.Uint32Values()), nil
	case *array.Uint16:
		return uintFn(a.Uint16Values()), nil
	case *array.Uint8:
		return uintFn(a.Uint8Values()), nil
	case *array.Float64:
		v := a.Float64Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return strconv.AppendFloat(dst, v[i], 'g', -1, 64), false, nil
		}, nil
	case *array.Float32:
		v := a.Float32Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return strconv.AppendFloat(dst, float64(v[i]), 'g', -1, 32), false, nil
		}, nil
	case *array.Float16:
		v := a.Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return strconv.AppendFloat(dst, float64(v[i].Float32()), 'g', -1, 32), false, nil
		}, nil
	case *array.Decimal128:
		scale := a.DataType().(*arrow.Decimal128Type).Scale
		return func(dst []byte, i int) ([]byte, bool, error) {
			return append(dst, a.Value(i).ToString(scale)...), false, nil
		}, nil
	case *array.Decimal256:
		scale := a.DataType().(*arrow.Decimal256Type).Scale
		return func(dst []byte, i int) ([]byte, bool, error) {
			return append(dst, a.Value(i).ToString(scale)...), false, nil
		}, nil
	case *array.Timestamp:
		tt := a.DataType().(*arrow.TimestampType)
		loc, err := w.location(tt)
		if err != nil {
			return nil, err
		}
		v := a.TimestampValues()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return v[i].ToTime(tt.Unit).In(loc).AppendFormat(dst, w.timeFmt), false, nil
		}, nil
	case *array.Date32:
		v := a.Date32Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return v[i].ToTime().AppendFormat(dst, w.dateFmt), false, nil
		}, nil
	case *array.Date64:
		v := a.Date64Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return v[i].ToTime().AppendFormat(dst, w.dateFmt), false, nil
		}, nil
	case *array.Time32:
		unit := a.DataType().(*arrow.Time32Type).Unit
		v := a.Time32Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return v[i].ToTime(unit).AppendFormat(dst, "15:04:05.999999999"), false, nil
		}, nil
	case *array.Time64:
		unit := a.DataType().(*arrow.Time64Type).Unit
		v := a.Time64Values()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return v[i].ToTime(unit).AppendFormat(dst, "15:04:05.999999999"), false, nil
		}, nil
	case *array.Duration:
		v := a.DurationValues()
		return func(dst []byte, i int) ([]byte, bool, error) {
			return strconv.AppendInt(dst, int64(v[i]), 10), false, nil
		}, nil
	case *array.Binary:
		return base64Fn(a.Value), nil
	case *array.LargeBinary:
		return base64Fn(a.Value), nil
	case *array.BinaryView:
		return base64Fn(a.Value), nil
	case *array.FixedSizeBinary:
		return base64Fn(a.Value), nil
	case *array.Dictionary:
		dict := a.Dictionary()
		inner, err := w.cellFormatter(dict, geom)
		if err != nil {
			return nil, err
		}
		return func(dst []byte, i int) ([]byte, bool, error) {
			idx := a.GetValueIndex(i)
			if dict.IsNull(idx) {
				return dst, true, nil
			}
			return inner(dst, idx)
		}, nil
	}
	switch arr.DataType().ID() {
	case arrow.LIST, arrow.LARGE_LIST, arrow.FIXED_SIZE_LIST, arrow.LIST_VIEW, arrow.LARGE_LIST_VIEW,
		arrow.STRUCT, arrow.MAP:
		return func(dst []byte, i int) ([]byte, bool, error) {
			b, err := json.Marshal(arr.GetOneForMarshal(i))
			if err != nil {
				return dst, false, fmt.Errorf("encode %s as JSON: %w", arr.DataType(), err)
			}
			return append(dst, b...), false, nil
		}, nil
	}
	return func(dst []byte, i int) ([]byte, bool, error) { return append(dst, arr.ValueStr(i)...), false, nil }, nil
}

func intFn[T int64 | int32 | int16 | int8](v []T) cellFn {
	return func(dst []byte, i int) ([]byte, bool, error) {
		return strconv.AppendInt(dst, int64(v[i]), 10), false, nil
	}
}

func uintFn[T uint64 | uint32 | uint16 | uint8](v []T) cellFn {
	return func(dst []byte, i int) ([]byte, bool, error) {
		return strconv.AppendUint(dst, uint64(v[i]), 10), false, nil
	}
}

func base64Fn(value func(int) []byte) cellFn {
	return func(dst []byte, i int) ([]byte, bool, error) {
		return base64.StdEncoding.AppendEncode(dst, value(i)), false, nil
	}
}

// location resolves a timestamp column's zone once per zone name.
func (w *Writer) location(tt *arrow.TimestampType) (*time.Location, error) {
	if tt.TimeZone == "" {
		return time.UTC, nil
	}
	if loc, ok := w.locations[tt.TimeZone]; ok {
		return loc, nil
	}
	loc, err := tt.GetZone()
	if err != nil {
		return nil, fmt.Errorf("time zone %q: %w", tt.TimeZone, err)
	}
	w.locations[tt.TimeZone] = loc
	return loc, nil
}

// writeField writes one field, quoting it as encoding/csv does.
func (w *Writer) writeField(field []byte) {
	if !w.fieldNeedsQuotes(field) {
		w.bw.Write(field)
		return
	}
	w.bw.WriteByte('"')
	for len(field) > 0 {
		i := bytes.IndexAny(field, "\"\r\n")
		if i < 0 {
			i = len(field)
		}
		w.bw.Write(field[:i])
		field = field[i:]
		if len(field) > 0 {
			switch field[0] {
			case '"':
				w.bw.WriteString(`""`)
			case '\r':
				if !w.crlf {
					w.bw.WriteByte('\r')
				}
			case '\n':
				if w.crlf {
					w.bw.WriteString("\r\n")
				} else {
					w.bw.WriteByte('\n')
				}
			}
			field = field[1:]
		}
	}
	w.bw.WriteByte('"')
}

// fieldNeedsQuotes mirrors encoding/csv: quote a field holding the
// delimiter, a quote, CR or LF, one starting with a space, and `\.`
// (which some tools read as end-of-data).
func (w *Writer) fieldNeedsQuotes(field []byte) bool {
	if len(field) == 0 {
		return false
	}
	if string(field) == `\.` {
		return true
	}
	if w.comma < utf8.RuneSelf {
		for _, c := range field {
			if c == '\n' || c == '\r' || c == '"' || c == byte(w.comma) {
				return true
			}
		}
	} else if bytes.ContainsRune(field, w.comma) || bytes.ContainsAny(field, "\"\r\n") {
		return true
	}
	r, _ := utf8.DecodeRune(field)
	return unicode.IsSpace(r)
}

func (w *Writer) endRecord() {
	if w.crlf {
		w.bw.WriteString("\r\n")
	} else {
		w.bw.WriteByte('\n')
	}
}

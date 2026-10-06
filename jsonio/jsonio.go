// Package jsonio reads JSON records — a JSON array of objects, or
// newline-delimited JSON (NDJSON, one object per line) — into gobi
// Frames. For GeoJSON, use geojsonio.
//
// Each object is a row and each key a column, in first-seen order. A
// key missing from a row, or a JSON null, is a null cell. Parsing is
// encoding/json/jsontext (json v2) at its default strictness: a key
// repeated within one object, or invalid UTF-8, is an error. Numbers are
// kept as their literal text until the column's type is settled, so
// nothing is rounded through float64 on the way in.
//
// Column types, from the column's non-null values:
//
//   - all true/false: Boolean
//   - all integers that fit int64: Int64 (an ID above 2^53 survives)
//   - all numbers, some fractional or exponent-form: Float64, provided
//     every integer in the column converts to float64 exactly
//   - all strings: String
//   - anything else — mixed kinds, an integer float64 can't hold
//     exactly in an otherwise-Float64 column, integers beyond int64,
//     numbers beyond float64, nested objects or arrays: String. Strings
//     keep their value (no quotes); numbers, booleans and nested values
//     become their JSON text (numbers as written, nested compacted).
//     So [7, "a"] reads as "7" and "a".
//   - no non-null values at all: String, every row null
//
// ReadOptions.AllStrings skips inference and reads every column as
// String the same way.
package jsonio

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
)

// ErrNotObject is returned when a record (an array element, or an
// NDJSON value) isn't a JSON object.
var ErrNotObject = errors.New("jsonio: record is not a JSON object")

// Format selects the input layout.
type Format uint8

const (
	// FormatAuto picks FormatArray when the first non-space byte is
	// '[' and FormatNDJSON otherwise.
	FormatAuto Format = iota
	// FormatArray is a single JSON array of objects.
	FormatArray
	// FormatNDJSON is a stream of objects, conventionally one per
	// line; any whitespace between objects is accepted.
	FormatNDJSON
)

// ReadOptions controls JSON parsing. A nil *ReadOptions uses the
// defaults.
type ReadOptions struct {
	// Format overrides input-layout detection.
	Format Format
	// AllStrings reads every column as String, with no inference:
	// strings as their value (no quotes), everything else as its JSON
	// text.
	AllStrings bool
	// Allocator overrides the Arrow allocator.
	Allocator memory.Allocator
}

// ReadFile reads the JSON records in path into a Frame.
func ReadFile(path string, opts *ReadOptions) (*gobi.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f, opts)
}

// Read reads JSON records from r into a Frame. Every column is a
// single chunk. Empty input (or an empty array) gives a Frame with no
// columns and no rows. A Frame's row count comes from its columns, so
// input whose objects are all empty ({} per line) also reads as no
// columns and no rows.
//
// Parsing uses encoding/json/jsontext with its default strictness: a
// key repeated within one object, or invalid UTF-8, is an error.
func Read(r io.Reader, opts *ReadOptions) (*gobi.Frame, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	dec := jsontext.NewDecoder(r)
	format := opts.Format
	if format == FormatAuto {
		format = FormatNDJSON
		if dec.PeekKind() == '[' {
			format = FormatArray
		}
	}

	t := &table{index: map[string]int{}}
	if format == FormatArray {
		if err := expectDelim(dec, '['); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("jsonio: expected an array: %w", io.ErrUnexpectedEOF)
			}
			return nil, err
		}
		for dec.PeekKind() != ']' {
			if err := t.readRow(dec); err != nil {
				if errors.Is(err, io.EOF) {
					return nil, fmt.Errorf("jsonio: array not closed: %w", io.ErrUnexpectedEOF)
				}
				return nil, err
			}
		}
		if err := expectDelim(dec, ']'); err != nil {
			return nil, err
		}
		if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("jsonio: data after the array at byte %d", dec.InputOffset())
		}
	} else {
		for {
			if err := t.readRow(dec); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, err
			}
		}
	}
	return t.frame(allocator(opts), opts.AllStrings)
}

func allocator(opts *ReadOptions) memory.Allocator {
	if opts.Allocator != nil {
		return opts.Allocator
	}
	return memory.DefaultAllocator
}

// expectDelim reads one token and checks it is want. A clean end of
// input comes back as a bare io.EOF.
func expectDelim(dec *jsontext.Decoder, want jsontext.Kind) error {
	tok, err := dec.ReadToken()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("jsonio: %w", err)
	}
	if tok.Kind() != want {
		return fmt.Errorf("jsonio: expected %v at byte %d, got %v", want, dec.InputOffset(), tok.Kind())
	}
	return nil
}

// kind is a value's JSON kind; kindNull doubles as "key absent".
type kind uint8

const (
	kindNull kind = iota
	kindBool
	kindNumber
	kindString
	kindNested
)

// column holds one key's values as text in a single arena: data holds
// every value back to back (a string's decoded value; for anything
// else its JSON text — "true", "12.5", compacted nested JSON) and
// ends[i] is where row i's text stops. One allocation pattern per
// column rather than one string per value, so a large input costs
// roughly its text plus 9 bytes a value until the Arrow arrays are
// built.
type column struct {
	name  string
	kinds []kind
	ends  []int
	data  []byte
	// seen is a bitmask of the non-null kinds present (1 << kind).
	seen uint8
}

func (c *column) text(i int) []byte {
	start := 0
	if i > 0 {
		start = c.ends[i-1]
	}
	return c.data[start:c.ends[i]]
}

// pad fills rows up to n with nulls.
func (c *column) pad(n int) {
	for len(c.kinds) < n {
		c.kinds = append(c.kinds, kindNull)
		c.ends = append(c.ends, len(c.data))
	}
}

type table struct {
	cols  []*column
	index map[string]int
	rows  int
}

// readRow decodes one object into row t.rows. Returns io.EOF (bare)
// when the stream ends cleanly before a row starts.
func (t *table) readRow(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("jsonio: row %d: %w", t.rows, err)
	}
	if tok.Kind() != '{' {
		return fmt.Errorf("%w: row %d is %v", ErrNotObject, t.rows, tok.Kind())
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadValue()
		if err != nil {
			return fmt.Errorf("jsonio: row %d: %w", t.rows, err)
		}
		col, err := t.column(name)
		if err != nil {
			return fmt.Errorf("jsonio: row %d: %w", t.rows, err)
		}
		if err := t.readValue(dec, col); err != nil {
			return fmt.Errorf("jsonio: row %d, key %q: %w", t.rows, col.name, err)
		}
	}
	if _, err := dec.ReadToken(); err != nil { // the closing '}'
		return fmt.Errorf("jsonio: row %d: %w", t.rows, err)
	}
	t.rows++
	return nil
}

// column returns the column for a raw (quoted) object name, creating
// it on first sight. The decoder rejects repeated names, so each
// column gets at most one value per row.
func (t *table) column(raw jsontext.Value) (*column, error) {
	inner := raw[1 : len(raw)-1]
	if slices.Contains(inner, '\\') {
		unq, err := jsontext.AppendUnquote(nil, raw)
		if err != nil {
			return nil, err
		}
		inner = unq
	}
	i, ok := t.index[string(inner)] // no allocation for the lookup
	if !ok {
		i = len(t.cols)
		name := string(inner)
		t.index[name] = i
		t.cols = append(t.cols, &column{name: name})
	}
	col := t.cols[i]
	col.pad(t.rows)
	return col, nil
}

// readValue appends the next value to col as row t.rows. ReadValue's
// bytes are only valid until the next read, so they're copied into
// the arena straight away.
func (t *table) readValue(dec *jsontext.Decoder, col *column) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	start := len(col.data)
	k := kindNumber
	switch raw.Kind() {
	case 'n':
		k = kindNull
	case 't', 'f':
		k = kindBool
		col.data = append(col.data, raw...)
	case '"':
		k = kindString
		if col.data, err = jsontext.AppendUnquote(col.data, raw); err != nil {
			return err
		}
	case '{', '[':
		k = kindNested
		v := jsontext.Value(append(col.data[start:start:start], raw...))
		if err := v.Compact(); err != nil {
			return err
		}
		col.data = append(col.data[:start], v...)
	default:
		col.data = append(col.data, raw...)
	}
	col.kinds = append(col.kinds, k)
	col.ends = append(col.ends, len(col.data))
	if k != kindNull {
		col.seen |= 1 << k
	}
	return nil
}

func (t *table) frame(pool memory.Allocator, allStrings bool) (*gobi.Frame, error) {
	fields := make([]arrow.Field, len(t.cols))
	cols := make([]arrow.Column, 0, len(t.cols))
	release := func() {
		for i := range cols {
			cols[i].Release()
		}
	}
	for i, col := range t.cols {
		col.pad(t.rows)
		arr, err := col.build(pool, allStrings)
		if err != nil {
			release()
			return nil, fmt.Errorf("jsonio: column %q: %w", col.name, err)
		}
		col.data, col.ends, col.kinds = nil, nil, nil // free the arena as we go
		fields[i] = arrow.Field{Name: col.name, Type: arr.DataType(), Nullable: true}
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		cols = append(cols, *arrow.NewColumn(fields[i], chunked))
		chunked.Release()
		arr.Release()
	}
	f, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		release()
		return nil, err
	}
	return f, nil
}

// build settles the column's type and builds its array, parsing each
// number once on the common paths. A numeric column tries Int64, then
// Float64, and settles on String if a value doesn't fit either
// exactly.
func (c *column) build(pool memory.Allocator, allStrings bool) (arrow.Array, error) {
	if !allStrings {
		switch c.seen {
		case 1 << kindBool:
			return c.buildBool(pool), nil
		case 1 << kindNumber:
			if arr, ok := c.buildInt64(pool); ok {
				return arr, nil
			}
			if arr, ok := c.buildFloat64(pool); ok {
				return arr, nil
			}
		}
	}
	return c.buildString(pool), nil
}

func (c *column) buildBool(pool memory.Allocator) arrow.Array {
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	b.Reserve(len(c.kinds))
	for i, k := range c.kinds {
		if k == kindNull {
			b.AppendNull()
		} else {
			b.Append(c.text(i)[0] == 't')
		}
	}
	return b.NewArray()
}

func (c *column) buildInt64(pool memory.Allocator) (arrow.Array, bool) {
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.Reserve(len(c.kinds))
	for i, k := range c.kinds {
		if k == kindNull {
			b.AppendNull()
			continue
		}
		v, err := strconv.ParseInt(string(c.text(i)), 10, 64)
		if err != nil {
			return nil, false
		}
		b.Append(v)
	}
	return b.NewArray(), true
}

// buildFloat64 fails (ok=false) if any value would change in float64:
// an integer literal float64 can't hold exactly (one above 2^53, say,
// alongside a 1.5 in the same column), or a number beyond float64's
// range. Fractional literals round to the nearest float64 as usual.
func (c *column) buildFloat64(pool memory.Allocator) (arrow.Array, bool) {
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	b.Reserve(len(c.kinds))
	for i, k := range c.kinds {
		if k == kindNull {
			b.AppendNull()
			continue
		}
		text := c.text(i)
		if !slices.ContainsFunc(text, func(r byte) bool { return r == '.' || r == 'e' || r == 'E' }) {
			n, err := strconv.ParseInt(string(text), 10, 64)
			if err != nil {
				return nil, false // integer beyond int64
			}
			f := float64(n)
			if f >= 0x1p63 || int64(f) != n {
				return nil, false
			}
			b.Append(f)
			continue
		}
		f, err := strconv.ParseFloat(string(text), 64)
		if err != nil {
			return nil, false
		}
		b.Append(f)
	}
	return b.NewArray(), true
}

func (c *column) buildString(pool memory.Allocator) arrow.Array {
	b := array.NewStringBuilder(pool)
	defer b.Release()
	b.Reserve(len(c.kinds))
	b.ReserveData(len(c.data))
	for i, k := range c.kinds {
		if k == kindNull {
			b.AppendNull()
		} else {
			b.BinaryBuilder.Append(c.text(i))
		}
	}
	return b.NewArray()
}

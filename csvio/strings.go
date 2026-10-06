package csvio

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
)

// ErrDuplicateHeader is returned by the string readers when two header
// cells share a name; a Frame's columns are looked up by name.
var ErrDuplicateHeader = errors.New("csvio: duplicate column name in header")

// ReadFileStrings reads path into a Frame without a struct: columns
// come from the header and every column is String. No type inference
// runs, so a cell such as "00123" or "1e5" stays exactly as written.
// Compression is detected from the filename unless opts.Compression
// is set. See ReadStrings for the null and header rules.
func ReadFileStrings(path string, opts *ReadOptions) (*gobi.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadStrings(f, fileOpts(path, opts))
}

// ReadStrings is the io.Reader counterpart to ReadFileStrings. Every
// column is String, nullable, and a single chunk.
//
//   - Columns are named from the header row. A UTF-8 byte-order mark
//     at the start of the input is dropped, and a blank header cell is
//     named f<i> (its zero-based index). Duplicate names return
//     ErrDuplicateHeader. Without a header (HasHeader false), columns
//     are f0, f1, ….
//   - Empty cells and opts.NullTokens become null, as in Read.
//   - A row with a different field count from the first returns
//     ErrRowFieldCountMismatch.
//
// Delimiter, Comment, LazyQuotes, SkipRows, Compression, NullTokens
// and Allocator apply as in Read; CRSHint, UseCRLF (line endings are
// detected) and ChunkRows don't.
func ReadStrings(r io.Reader, opts *ReadOptions) (*gobi.Frame, error) {
	var out *gobi.Frame
	err := readStrings(r, opts, 0, func(f *gobi.Frame) error {
		out = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReadFileStringsChunksFunc is the streaming form of ReadFileStrings:
// fn gets one Frame per ChunkRows rows. Lifetime and error rules
// match ReadFileChunksFunc.
func ReadFileStringsChunksFunc(path string, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return ReadStringsChunksFunc(f, fileOpts(path, opts), fn)
}

// ReadStringsChunksFunc is the streaming form of ReadStrings. fn gets
// one Frame per ChunkRows rows (DefaultChunkRows when unset), released
// after fn returns; call frame.Retain() to keep one. A header-only
// input calls fn once with a zero-row Frame so the columns are still
// visible. An error from fn stops the read and is wrapped in
// ErrChunksAborted.
func ReadStringsChunksFunc(r io.Reader, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	return readStrings(r, opts, chunkRows(opts), func(f *gobi.Frame) error {
		err := fn(f)
		f.Release()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrChunksAborted, err)
		}
		return nil
	})
}

// fileOpts fills in the codec from path when opts leaves it on auto.
func fileOpts(path string, opts *ReadOptions) *ReadOptions {
	if opts == nil {
		opts = &ReadOptions{}
	}
	if opts.Compression == CodecAuto {
		local := *opts
		local.Compression = detectCodecFromPath(path)
		opts = &local
	}
	return opts
}

// readStrings parses r into String columns and hands emit a Frame
// every chunk rows, plus the final partial chunk; chunk 0 means one
// Frame for the whole input. emit owns each Frame it receives.
func readStrings(r io.Reader, opts *ReadOptions, chunk int, emit func(*gobi.Frame) error) error {
	if opts == nil {
		opts = &ReadOptions{}
	}
	if opts.Compression != CodecAuto && opts.Compression != CodecNone {
		dec, release, err := wrapCodec(r, opts.Compression)
		if err != nil {
			return err
		}
		defer release()
		r = dec
	}
	pool := opts.Allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}
	r = skipBOM(r)
	// Same raw-line skip as Read: lines before the header count.
	if opts.SkipRows > 0 {
		br := bufio.NewReader(r)
		for range opts.SkipRows {
			if _, err := br.ReadBytes('\n'); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
		r = br
	}

	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	cr.LazyQuotes = opts.LazyQuotes
	if opts.Delimiter != 0 {
		cr.Comma = opts.Delimiter
	}
	if opts.Comment != 0 {
		cr.Comment = opts.Comment
	}
	nulls := map[string]bool{"": true}
	for _, tok := range opts.NullTokens {
		nulls[tok] = true
	}

	first, err := cr.Read()
	if errors.Is(err, io.EOF) {
		if opts.hasHeader() {
			return ErrHeaderMissing
		}
		empty, err := gobi.NewFrame(arrow.NewSchema(nil, nil), nil)
		if err != nil {
			return err
		}
		return emit(empty)
	}
	if err != nil {
		return csvErr(err)
	}

	fields := make([]arrow.Field, len(first))
	seen := make(map[string]bool, len(first))
	for i, cell := range first {
		name := fmt.Sprintf("f%d", i)
		if opts.hasHeader() && cell != "" {
			name = cell
		}
		if seen[name] {
			return fmt.Errorf("%w: %q", ErrDuplicateHeader, name)
		}
		seen[name] = true
		fields[i] = arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true}
	}
	schema := arrow.NewSchema(fields, nil)

	bldrs := make([]*array.StringBuilder, len(fields))
	for i := range bldrs {
		bldrs[i] = array.NewStringBuilder(pool)
		defer bldrs[i].Release()
	}
	rows := 0
	appendRow := func(rec []string) error {
		for i, cell := range rec {
			if nulls[cell] {
				bldrs[i].AppendNull()
				continue
			}
			// String offsets are int32; past that, a single-chunk
			// column can't hold the data.
			if bldrs[i].DataLen() > math.MaxInt32-len(cell) {
				hint := "use ReadStringsChunksFunc"
				if chunk > 0 {
					hint = "lower ChunkRows"
				}
				return fmt.Errorf("csvio: column %q exceeds 2 GiB of string data in one chunk; %s", fields[i].Name, hint)
			}
			bldrs[i].Append(cell)
		}
		rows++
		return nil
	}
	flush := func() error {
		cols := make([]arrow.Column, len(bldrs))
		for i, b := range bldrs {
			arr := b.NewArray()
			chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
			cols[i] = *arrow.NewColumn(fields[i], chunked)
			chunked.Release()
			arr.Release()
		}
		rows = 0
		f, err := gobi.NewFrame(schema, cols)
		if err != nil {
			for i := range cols {
				cols[i].Release()
			}
			return err
		}
		return emit(f)
	}

	if !opts.hasHeader() {
		if err := appendRow(first); err != nil {
			return err
		}
	}
	emitted := false
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return csvErr(err)
		}
		if err := appendRow(rec); err != nil {
			return err
		}
		if chunk > 0 && rows == chunk {
			if err := flush(); err != nil {
				return err
			}
			emitted = true
		}
	}
	if rows > 0 || !emitted {
		return flush()
	}
	return nil
}

// csvErr maps encoding/csv's field-count error onto
// ErrRowFieldCountMismatch, keeping the line number from the parse
// error.
func csvErr(err error) error {
	if errors.Is(err, csv.ErrFieldCount) {
		return fmt.Errorf("%w: %w", ErrRowFieldCountMismatch, err)
	}
	return fmt.Errorf("csvio: %w", err)
}

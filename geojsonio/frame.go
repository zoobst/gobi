package geojsonio

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// ReadOptions controls ReadFile / ReadFileChunksFunc / ScanFile.
type ReadOptions struct {
	// Format selects the input shape. Defaults to FormatAuto, which
	// picks LineDelimited when the file extension is `.geojsonl` /
	// `.ndjson`, FeatureCollection otherwise.
	Format Format

	// Columns projects the output Frame to just the named columns.
	// The geometry column is always kept — matches gpkgio's rule.
	Columns []string

	// Allocator overrides the arrow allocator. Defaults to
	// memory.DefaultAllocator.
	Allocator memory.Allocator

	// ChunkRows caps the number of features per emitted batch in
	// the streaming path. 0 → DefaultChunkRows. Ignored by
	// ReadFile (whole-file materialization).
	ChunkRows int
}

// WriteOptions controls WriteFile.
type WriteOptions struct {
	// Format selects the output shape. Defaults to
	// FeatureCollection.
	Format Format

	// GeomCol names the geometry column. Defaults to the first
	// column tagged as gobi.GeometryField.
	GeomCol string

	// Indent, when non-empty, pretty-prints the output with that
	// indent per level. Empty = compact single-line JSON (typical
	// for machine-consumed files). Only applies to FeatureCollection
	// output; line-delimited output is always compact so each line
	// stays a single feature.
	Indent string
}

// Format selects the on-disk shape.
type Format uint8

const (
	// FormatAuto picks LineDelimited for .geojsonl / .ndjson and
	// FeatureCollection otherwise.
	FormatAuto Format = iota
	// FormatFeatureCollection reads/writes a single
	// `{"type":"FeatureCollection","features":[...]}` document.
	FormatFeatureCollection
	// FormatLineDelimited reads/writes one Feature per line. Enables
	// bounded-memory streaming — the parser never needs to know
	// where the FeatureCollection ends before it can emit rows.
	FormatLineDelimited
)

// DefaultChunkRows is the target batch size for the streaming
// reader — matches parquetio's default.
const DefaultChunkRows = 65_536

// ReadFile reads a GeoJSON file into a single Frame. The Frame has a
// `geometry` column (WKB Binary, tagged EPSG:4326 per RFC 7946)
// plus one column per distinct property key seen across all
// features. Missing properties on individual features come back as
// null.
//
// Property column types are inferred by scanning every feature:
// the union type is picked as the "widest" compatible arrow type
// — Int64 promotes to Float64 if any value is fractional; a
// column with any string value stays String. When a column has
// mixed types that don't unify (e.g., some strings and some
// numbers), it comes out as String with every value stringified.
//
// For files too large to fit in memory, use ReadFileChunksFunc.
func ReadFile(path string, opts *ReadOptions) (*gobi.Frame, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	var frames []*gobi.Frame
	err := ReadFileChunksFunc(path, opts, func(f *gobi.Frame) error {
		f.Retain()
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return emptyFrameWithSchema(nil)
	}
	if len(frames) == 1 {
		return frames[0], nil
	}
	out, err := gobi.Concat(frames...)
	for _, f := range frames {
		f.Release()
	}
	return out, err
}

// ReadFileChunksFunc streams a GeoJSON file through fn one batch at
// a time. The batch size is opts.ChunkRows (default 65k features).
//
// For FormatFeatureCollection, the reader walks the top-level
// `features` array with a streaming JSON decoder — bounded memory
// regardless of file size. For FormatLineDelimited, each line is
// parsed as one Feature.
//
// The Frame passed to fn owns its arrow buffers; call frame.Retain()
// to hold it past the callback's return.
func ReadFileChunksFunc(path string, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	if opts == nil {
		opts = &ReadOptions{}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	format := opts.Format
	if format == FormatAuto {
		format = detectFormatFromPath(path)
	}
	batchSize := opts.ChunkRows
	if batchSize <= 0 {
		batchSize = DefaultChunkRows
	}

	// Two-pass streaming: first walk collects all features into
	// batch buffers of `batchSize` and inspects property keys +
	// types. The second walk is unnecessary — we type-infer within
	// each batch independently, so different batches may have
	// different property columns.
	//
	// Building typed columns from a heterogeneous property soup is
	// the interesting part: we buffer each batch's raw properties
	// (as map[string]any), scan them to pick a per-column arrow
	// type, then materialize.
	iter, err := newFeatureIter(f, format)
	if err != nil {
		return err
	}

	buf := &featureBatch{max: batchSize}
	for {
		g, props, more, err := iter.Next()
		if err != nil {
			return err
		}
		if !more {
			break
		}
		buf.add(g, props)
		if buf.full() {
			frame, err := buf.materialize(opts)
			if err != nil {
				return err
			}
			if err := fn(frame); err != nil {
				return err
			}
			buf.reset()
		}
	}
	if buf.count > 0 {
		frame, err := buf.materialize(opts)
		if err != nil {
			return err
		}
		return fn(frame)
	}
	return nil
}

// detectFormatFromPath returns FormatLineDelimited when the file
// name suggests it; FormatFeatureCollection otherwise. Called only
// when opts.Format is FormatAuto.
func detectFormatFromPath(path string) Format {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".geojsonl") || strings.HasSuffix(lower, ".ndjson") {
		return FormatLineDelimited
	}
	return FormatFeatureCollection
}

// featureIter abstracts the two format variants. Next returns the
// next Feature or more=false at EOF. The reader closes the
// underlying io.Reader on EOF or first error.
type featureIter interface {
	Next() (geometry.Geometry, map[string]any, bool, error)
}

func newFeatureIter(r io.Reader, format Format) (featureIter, error) {
	switch format {
	case FormatLineDelimited:
		return &lineIter{scanner: bufio.NewScanner(r)}, nil
	case FormatFeatureCollection, FormatAuto:
		return newFCIter(r)
	}
	return nil, fmt.Errorf("%w: unknown format %d", ErrInvalidGeoJSON, format)
}

// lineIter reads one JSON Feature per line. Uses bufio.Scanner with
// a bumped buffer so single-line features up to a few MB parse
// cleanly (default bufio.Scanner limit is 64KB which trips on
// realistic multi-geometry features).
type lineIter struct {
	scanner *bufio.Scanner
	init    bool
}

func (it *lineIter) Next() (geometry.Geometry, map[string]any, bool, error) {
	if !it.init {
		// Bump the max token size to 16 MB — a single Feature line
		// can be quite large (huge multipolygon, big property
		// bag). Beyond 16 MB, callers should use FeatureCollection
		// format which streams inside the array.
		it.scanner.Buffer(make([]byte, 64*1024), 16<<20)
		it.init = true
	}
	for it.scanner.Scan() {
		line := it.scanner.Bytes()
		if len(line) == 0 || allWhitespace(line) {
			continue
		}
		g, props, err := UnmarshalFeature(line)
		if err != nil {
			return nil, nil, false, err
		}
		return g, props, true, nil
	}
	if err := it.scanner.Err(); err != nil {
		return nil, nil, false, err
	}
	return nil, nil, false, nil
}

// fcIter reads a FeatureCollection via a streaming json.Decoder.
// The decoder tokenizes the top-level object, seeks the "features"
// array, then decodes one Feature at a time so peak memory stays
// bounded to a single feature.
type fcIter struct {
	dec  *json.Decoder
	done bool
}

func newFCIter(r io.Reader) (*fcIter, error) {
	dec := json.NewDecoder(r)
	// Advance past the opening `{`.
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("%w: expected '{' at top level, got %v", ErrInvalidGeoJSON, tok)
	}
	// Consume key/value pairs until we hit "features". Ignore
	// "type", "bbox", and any RFC-7946 foreign members that come
	// before it — the "features" key is required.
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: non-string object key: %v", ErrInvalidGeoJSON, tok)
		}
		if key == "features" {
			// Advance past the '['.
			openArr, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
			}
			if d, ok := openArr.(json.Delim); !ok || d != '[' {
				return nil, fmt.Errorf("%w: features not an array", ErrInvalidGeoJSON)
			}
			return &fcIter{dec: dec}, nil
		}
		// Skip the value of any non-features key.
		var scratch json.RawMessage
		if err := dec.Decode(&scratch); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
		}
	}
	return nil, fmt.Errorf("%w: FeatureCollection missing 'features' array", ErrInvalidGeoJSON)
}

func (it *fcIter) Next() (geometry.Geometry, map[string]any, bool, error) {
	if it.done {
		return nil, nil, false, nil
	}
	if !it.dec.More() {
		it.done = true
		return nil, nil, false, nil
	}
	var raw json.RawMessage
	if err := it.dec.Decode(&raw); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
	}
	g, props, err := UnmarshalFeature(raw)
	if err != nil {
		return nil, nil, false, err
	}
	return g, props, true, nil
}

// -----------------------------------------------------------------------------
// Batch buffering + Frame construction
// -----------------------------------------------------------------------------

// featureBatch accumulates raw feature data (geometry blobs +
// property bags) up to the configured batch size, then materializes
// into a typed Frame. Kept as a separate struct so ReadFileChunksFunc
// can reuse the buffer across batches without re-allocating the
// backing slices.
type featureBatch struct {
	geoms []geometry.Geometry
	props []map[string]any
	count int
	max   int
}

func (b *featureBatch) add(g geometry.Geometry, p map[string]any) {
	b.geoms = append(b.geoms, g)
	b.props = append(b.props, p)
	b.count++
}

func (b *featureBatch) full() bool { return b.count >= b.max }

func (b *featureBatch) reset() {
	b.geoms = b.geoms[:0]
	b.props = b.props[:0]
	b.count = 0
}

func (b *featureBatch) materialize(opts *ReadOptions) (*gobi.Frame, error) {
	pool := opts.Allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}

	// Collect the set of property keys (deterministic order — first-
	// occurrence). Skipping this and using the first feature only
	// would be faster but breaks when property keys are added later
	// in the file.
	keyOrder := make([]string, 0, 8)
	keySeen := make(map[string]struct{}, 8)
	for _, p := range b.props {
		for k := range p {
			if _, ok := keySeen[k]; ok {
				continue
			}
			keySeen[k] = struct{}{}
			keyOrder = append(keyOrder, k)
		}
	}

	// Column projection: honor opts.Columns, but always keep the
	// geometry column (matches gpkgio's rule).
	if len(opts.Columns) > 0 {
		want := make(map[string]struct{}, len(opts.Columns))
		for _, c := range opts.Columns {
			want[c] = struct{}{}
		}
		filtered := keyOrder[:0]
		for _, k := range keyOrder {
			if _, ok := want[k]; ok {
				filtered = append(filtered, k)
			}
		}
		keyOrder = filtered
	}

	// Infer per-column type by scanning every property value.
	types := make(map[string]arrow.DataType, len(keyOrder))
	for _, k := range keyOrder {
		types[k] = inferPropertyType(b.props, k)
	}

	// Build the schema — geometry column first, then property
	// columns in first-seen order.
	fields := make([]arrow.Field, 0, len(keyOrder)+1)
	fields = append(fields, gobi.GeometryField("geometry", 4326))
	for _, k := range keyOrder {
		fields = append(fields, arrow.Field{Name: k, Type: types[k], Nullable: true})
	}
	schema := arrow.NewSchema(fields, nil)

	// Build one builder per column, fill from the buffered features.
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, g := range b.geoms {
		if g == nil {
			geomB.AppendNull()
			continue
		}
		geomB.Append(geometry.WKB(g))
	}

	propBuilders := make([]array.Builder, len(keyOrder))
	for i, k := range keyOrder {
		bldr, err := builderForArrowType(pool, types[k])
		if err != nil {
			return nil, fmt.Errorf("geojsonio: builder for %q: %w", k, err)
		}
		propBuilders[i] = bldr
	}
	defer func() {
		for _, bldr := range propBuilders {
			bldr.Release()
		}
	}()

	for row := 0; row < b.count; row++ {
		p := b.props[row]
		for i, k := range keyOrder {
			v, ok := p[k]
			if !ok || v == nil {
				propBuilders[i].AppendNull()
				continue
			}
			if err := appendPropertyValue(propBuilders[i], v); err != nil {
				return nil, fmt.Errorf("geojsonio: row %d col %q: %w", row, k, err)
			}
		}
	}

	// Materialize builders into arrow columns.
	geomArr := geomB.NewArray()
	defer geomArr.Release()
	cols := make([]arrow.Column, 0, len(keyOrder)+1)
	cols = append(cols, *arrow.NewColumn(fields[0], arrow.NewChunked(geomArr.DataType(), []arrow.Array{geomArr})))
	for i, bldr := range propBuilders {
		arr := bldr.NewArray()
		cols = append(cols, *arrow.NewColumn(fields[i+1], arrow.NewChunked(arr.DataType(), []arrow.Array{arr})))
		arr.Release()
	}
	return gobi.NewFrame(schema, cols)
}

// inferPropertyType picks the widest arrow type that can hold every
// non-null value at props[*][key]. Follows JSON's promotion order:
// Bool → Int64 → Float64 → String. A key seen only with null values
// defaults to String (the safest destination for an unknown-type
// null column).
func inferPropertyType(props []map[string]any, key string) arrow.DataType {
	seenBool := false
	seenInt := false
	seenFloat := false
	seenString := false
	seenAny := false
	for _, p := range props {
		v, ok := p[key]
		if !ok || v == nil {
			continue
		}
		seenAny = true
		switch x := v.(type) {
		case bool:
			seenBool = true
		case float64:
			// JSON numbers come out as float64 from encoding/json.
			// Distinguish "integer-valued" from "true float" so
			// tight-fitting int columns don't get promoted.
			if x == float64(int64(x)) && !seenFloat {
				seenInt = true
			} else {
				seenFloat = true
			}
		case string:
			seenString = true
		default:
			// Anything else (array, object) → downgrade to string.
			seenString = true
		}
	}
	if !seenAny {
		return arrow.BinaryTypes.String
	}
	if seenString || (seenBool && (seenInt || seenFloat)) {
		return arrow.BinaryTypes.String
	}
	if seenFloat {
		return arrow.PrimitiveTypes.Float64
	}
	if seenInt {
		return arrow.PrimitiveTypes.Int64
	}
	if seenBool {
		return arrow.FixedWidthTypes.Boolean
	}
	return arrow.BinaryTypes.String
}

// appendPropertyValue coerces a JSON-decoded Go value into a typed
// arrow builder. Fallback for anything that doesn't match cleanly
// is a JSON-stringified representation, so a mixed-type column
// still gets valid data.
func appendPropertyValue(b array.Builder, v any) error {
	switch tb := b.(type) {
	case *array.BooleanBuilder:
		if x, ok := v.(bool); ok {
			tb.Append(x)
			return nil
		}
	case *array.Int64Builder:
		if x, ok := v.(float64); ok {
			tb.Append(int64(x))
			return nil
		}
	case *array.Float64Builder:
		if x, ok := v.(float64); ok {
			tb.Append(x)
			return nil
		}
	case *array.StringBuilder:
		switch x := v.(type) {
		case string:
			tb.Append(x)
			return nil
		case float64:
			tb.Append(fmt.Sprintf("%v", x))
			return nil
		case bool:
			if x {
				tb.Append("true")
			} else {
				tb.Append("false")
			}
			return nil
		}
		// Nested arrays / objects — stringify via json.Marshal.
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		tb.Append(string(raw))
		return nil
	}
	// Non-matching builder → append null so the shape stays right;
	// the type-inference step should have picked the right builder,
	// so hitting this branch usually means the property value type
	// changed within a batch (which the inference did notice but
	// this specific append path didn't).
	b.AppendNull()
	return nil
}

func builderForArrowType(pool memory.Allocator, t arrow.DataType) (array.Builder, error) {
	switch t.ID() {
	case arrow.INT64:
		return array.NewInt64Builder(pool), nil
	case arrow.FLOAT64:
		return array.NewFloat64Builder(pool), nil
	case arrow.STRING:
		return array.NewStringBuilder(pool), nil
	case arrow.BOOL:
		return array.NewBooleanBuilder(pool), nil
	}
	return nil, fmt.Errorf("no builder for arrow type %s", t)
}

func emptyFrameWithSchema(schema *arrow.Schema) (*gobi.Frame, error) {
	// A file with no features → return an empty Frame with just
	// the geometry column. Callers who need type info for their
	// property columns should read a non-empty file.
	if schema == nil {
		schema = arrow.NewSchema([]arrow.Field{
			gobi.GeometryField("geometry", 4326),
		}, nil)
	}
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	arr := geomB.NewArray()
	defer arr.Release()
	cols := []arrow.Column{
		*arrow.NewColumn(schema.Field(0), arrow.NewChunked(arr.DataType(), []arrow.Array{arr})),
	}
	return gobi.NewFrame(schema, cols)
}

func allWhitespace(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// WriteFile
// -----------------------------------------------------------------------------

// WriteFile writes df to a GeoJSON file at path. Format is chosen by
// opts.Format (defaults to FeatureCollection). The Frame's geometry
// column identifies the geometry per feature; every other column
// becomes a property.
//
// Column types map back to JSON as expected: Int64/Int32/Uint64/Uint32
// → JSON number, Float64/Float32 → JSON number, Bool → JSON bool,
// String → JSON string, Timestamp → RFC 3339 string. Nulls emit
// as JSON null.
func WriteFile(df *gobi.Frame, path string, opts *WriteOptions) error {
	if opts == nil {
		opts = &WriteOptions{}
	}
	geomIdx := -1
	if opts.GeomCol != "" {
		names := df.ColumnNames()
		for i, n := range names {
			if n == opts.GeomCol {
				geomIdx = i
				break
			}
		}
		if geomIdx < 0 {
			return fmt.Errorf("geojsonio: WriteFile: GeomCol %q not in frame", opts.GeomCol)
		}
	} else {
		for i := 0; i < df.NumCols(); i++ {
			s, _ := df.ColumnAt(i)
			if s.IsGeometry() {
				geomIdx = i
				break
			}
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	defer bw.Flush()

	format := opts.Format
	if format == FormatAuto {
		format = FormatFeatureCollection
	}

	switch format {
	case FormatLineDelimited:
		return writeLines(bw, df, geomIdx)
	default:
		return writeFeatureCollection(bw, df, geomIdx, opts.Indent)
	}
}

// writeFeatureCollection emits a `{"type":"FeatureCollection","features":[...]}`
// document. Indentation controls whitespace: empty for compact,
// non-empty for pretty-printed.
func writeFeatureCollection(w io.Writer, df *gobi.Frame, geomIdx int, indent string) error {
	pretty := indent != ""
	if pretty {
		if _, err := fmt.Fprintf(w, "{\n%[1]s\"type\": \"FeatureCollection\",\n%[1]s\"features\": [\n", indent); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(w, `{"type":"FeatureCollection","features":[`); err != nil {
			return err
		}
	}
	err := forEachFeature(df, geomIdx, func(row int, featJSON []byte) error {
		if row > 0 {
			if pretty {
				if _, err := fmt.Fprintf(w, ",\n%s%s", indent, indent); err != nil {
					return err
				}
			} else {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
		} else if pretty {
			if _, err := fmt.Fprintf(w, "%s%s", indent, indent); err != nil {
				return err
			}
		}
		_, err := w.Write(featJSON)
		return err
	})
	if err != nil {
		return err
	}
	if pretty {
		if _, err := fmt.Fprintf(w, "\n%s]\n}\n", indent); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(w, "]}"); err != nil {
			return err
		}
	}
	return nil
}

// writeLines emits one Feature per line — the `.geojsonl`
// convention. Always compact per line so downstream tooling can
// split on `\n` alone.
func writeLines(w io.Writer, df *gobi.Frame, geomIdx int) error {
	return forEachFeature(df, geomIdx, func(_ int, featJSON []byte) error {
		if _, err := w.Write(featJSON); err != nil {
			return err
		}
		_, err := w.Write([]byte{'\n'})
		return err
	})
}

// forEachFeature iterates rows of df, builds a Feature JSON object
// per row, and invokes fn. Kept as a helper so the FC + line
// writers share a single row-processing loop.
func forEachFeature(df *gobi.Frame, geomIdx int, fn func(row int, featJSON []byte) error) error {
	names := df.ColumnNames()
	// Pre-resolve series so per-row lookups don't repeat the
	// name→column walk.
	series := make([]gobi.Series, df.NumCols())
	for i := 0; i < df.NumCols(); i++ {
		s, err := df.ColumnAt(i)
		if err != nil {
			return err
		}
		series[i] = s
	}

	for row := 0; row < df.NumRows(); row++ {
		props := make(map[string]any)
		for i, s := range series {
			if i == geomIdx {
				continue
			}
			v, err := scalarAt(s, row)
			if err != nil {
				return fmt.Errorf("geojsonio: %s row %d: %w", names[i], row, err)
			}
			props[names[i]] = v
		}
		var g geometry.Geometry
		if geomIdx >= 0 {
			wkb, err := readGeomAt(series[geomIdx], row)
			if err != nil {
				return fmt.Errorf("geojsonio: geometry row %d: %w", row, err)
			}
			if wkb != nil {
				g, err = geometry.ParseWKB(wkb)
				if err != nil {
					return fmt.Errorf("geojsonio: parse geometry row %d: %w", row, err)
				}
			}
		}
		featJSON, err := MarshalFeature(g, props)
		if err != nil {
			return err
		}
		if err := fn(row, featJSON); err != nil {
			return err
		}
	}
	return nil
}

// scalarAt reads one row from s as a Go scalar suitable for
// json.Marshal. Handles the primitive types Frame columns commonly
// carry; unsupported types come back as a nil.
func scalarAt(s gobi.Series, row int) (any, error) {
	chunks := s.Column().Data().Chunks()
	offset := 0
	for _, chunk := range chunks {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return nil, nil
			}
			switch a := chunk.(type) {
			case *array.Int64:
				return a.Value(local), nil
			case *array.Int32:
				return int64(a.Value(local)), nil
			case *array.Uint64:
				return a.Value(local), nil
			case *array.Uint32:
				return uint64(a.Value(local)), nil
			case *array.Float64:
				return a.Value(local), nil
			case *array.Float32:
				return float64(a.Value(local)), nil
			case *array.Boolean:
				return a.Value(local), nil
			case *array.String:
				return a.Value(local), nil
			case *array.LargeString:
				return a.Value(local), nil
			case *array.Binary:
				return a.Value(local), nil
			case *array.Timestamp:
				// Emit as RFC 3339 for round-trip-friendly JSON. Unit
				// comes from the field's arrow type.
				ts := a.Value(local)
				unit := a.DataType().(*arrow.TimestampType).Unit
				return timestampToRFC3339(int64(ts), unit), nil
			}
			return nil, fmt.Errorf("unsupported chunk type %T", chunk)
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("row %d out of range", row)
}

// readGeomAt pulls the raw WKB bytes at row from a geometry column.
// Returns nil for a null row so the caller can emit `"geometry":null`.
func readGeomAt(s gobi.Series, row int) ([]byte, error) {
	chunks := s.Column().Data().Chunks()
	offset := 0
	for _, chunk := range chunks {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return nil, nil
			}
			ba, ok := chunk.(*array.Binary)
			if !ok {
				return nil, fmt.Errorf("geometry column not Binary, got %T", chunk)
			}
			return ba.Value(local), nil
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("row %d out of range", row)
}

// timestampToRFC3339 formats an arrow timestamp integer + unit as
// RFC 3339 UTC. Used for JSON output; RFC 3339 is the
// least-surprising serialization for downstream tools.
func timestampToRFC3339(v int64, unit arrow.TimeUnit) string {
	var sec, nsec int64
	switch unit {
	case arrow.Second:
		sec = v
	case arrow.Millisecond:
		sec = v / 1_000
		nsec = (v % 1_000) * 1_000_000
	case arrow.Microsecond:
		sec = v / 1_000_000
		nsec = (v % 1_000_000) * 1_000
	case arrow.Nanosecond:
		sec = v / 1_000_000_000
		nsec = v % 1_000_000_000
	}
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339Nano)
}

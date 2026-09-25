package gobi

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// StructOption tunes FromStructs / ToStructs behavior. Passed as
// variadic arguments to keep the call site clean when no options are
// needed.
type StructOption func(*structOpts)

// structOpts holds resolved struct-tag options for one FromStructs /
// ToStructs invocation.
type structOpts struct {
	// tagFormat is the primary struct-tag namespace to look up per
	// field. e.g. "parquet" → resolve names / options from
	// `parquet:"col,opt"`. Empty string means "no format-specific
	// tag; use the fallback chain directly."
	tagFormat string
	// copyValues: ToStructs deep-copies strings / []byte (see
	// StructCopyValues).
	copyValues bool
	// interner backs `intern`-tagged fields; nil means a per-call
	// one (see StructInterner).
	interner *StringInterner
	// coerceNumbers / requireColumns: see StructCoerceNumbers and
	// StructRequireColumns.
	coerceNumbers  bool
	requireColumns bool
	// FromStructs: see StructRequiredFields, StructZeroTimeAsValue,
	// StructAllocator.
	requiredFields  bool
	zeroTimeAsValue bool
	allocator       memory.Allocator
}

// StructRequiredFields makes FromStructs mark non-pointer fields as
// non-nullable (parquet REQUIRED), matching parquet-go, which writes
// non-pointer fields as REQUIRED. Pointer fields stay nullable. Per
// field, the `optional` tag option keeps a non-pointer field nullable,
// and `required` marks one REQUIRED without this option (e.g.
// `parquet:"id,required"`).
//
// A REQUIRED field never writes null, so zero values that FromStructs
// would otherwise turn into nulls are written as values: a zero
// time.Time is the zero instant (see StructZeroTimeAsValue) and a nil
// slice is an empty list. Zero values with no non-null form — an
// empty geometry string or nil geometry []byte, an empty time-layout
// string — are an error.
func StructRequiredFields() StructOption {
	return func(o *structOpts) { o.requiredFields = true }
}

// StructZeroTimeAsValue makes FromStructs write a zero time.Time as
// the zero instant (0001-01-01T00:00:00Z) instead of NULL, matching
// parquet-go. The zero instant doesn't fit a nanosecond timestamp
// (range 1677–2262), so pair it with a coarser unit, e.g.
// `parquet:"ts,timestamp(microsecond)"`; on a Timestamp[ns] column it
// is an ErrStructFieldOverflow error.
func StructZeroTimeAsValue() StructOption {
	return func(o *structOpts) { o.zeroTimeAsValue = true }
}

// StructAllocator sets the Arrow allocator FromStructs builds the
// Frame's buffers with. nil (or no option) uses memory.DefaultAllocator.
func StructAllocator(pool memory.Allocator) StructOption {
	return func(o *structOpts) { o.allocator = pool }
}

// StructTagFormat sets the primary struct-tag namespace to consult
// when resolving column names and per-field options. If a field has
// no tag under this namespace, resolution falls back through:
//
//	gobi:"..."  — universal namespace, works for every io
//	csv:"..."   — legacy tag kept working for backward compat
//	<field name> — final fallback
//
// A tag value of "-" (in whichever namespace matches first) skips the
// field entirely. Multi-part tag values (`format:"name,opt1,opt2"`)
// use the first comma-separated element as the column name.
//
// Example usage:
//
//	frame, _ := gobi.FromStructs(rows, gobi.StructTagFormat("parquet"))
//
// Per-io convenience wrappers (parquetio.WriteStructs, etc.) pass
// their own format automatically.
func StructTagFormat(format string) StructOption {
	return func(o *structOpts) { o.tagFormat = format }
}

// resolveStructOpts materializes the options bag from variadic args.
func resolveStructOpts(opts []StructOption) *structOpts {
	o := &structOpts{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// ResolveFieldName returns the column name to use for sf under the
// given io-format tag namespace, with a fallback chain to `gobi:`,
// `csv:` (legacy), and finally the field's Go name. skip is true when
// the tag value is exactly "-", meaning the field should be omitted
// from the output entirely.
//
// io-package convenience wrappers (parquetio.WriteStructs, csvio, etc.)
// use this to keep tag resolution consistent with FromStructs /
// ToStructs. Pass "" as format to skip the per-io namespace lookup.
func ResolveFieldName(sf reflect.StructField, format string) (name string, skip bool) {
	tags := make([]string, 0, 3)
	if format != "" {
		tags = append(tags, format)
	}
	tags = append(tags, "gobi", "csv")
	for _, tag := range tags {
		v := sf.Tag.Get(tag)
		if v == "" {
			continue
		}
		primary := v
		if i := strings.IndexByte(v, ','); i >= 0 {
			primary = v[:i]
		}
		if primary == "-" {
			return "", true
		}
		if primary != "" {
			return primary, false
		}
	}
	return sf.Name, false
}

// resolveFieldName is the unexported internal wrapper — takes an opts
// struct instead of a raw format string so planStructFields doesn't
// leak the option type across the call.
func resolveFieldName(sf reflect.StructField, o *structOpts) (name string, skip bool) {
	format := ""
	if o != nil {
		format = o.tagFormat
	}
	return ResolveFieldName(sf, format)
}

// fieldTagOptions returns the comma-separated options after the name
// part of sf's name tag. Options come from the first tag in the
// resolution chain (<format>, gobi, csv) with a non-empty value, so
// `parquet:",intern"` works with an empty name part.
func fieldTagOptions(sf reflect.StructField, o *structOpts) []string {
	tags := make([]string, 0, 3)
	if o != nil && o.tagFormat != "" {
		tags = append(tags, o.tagFormat)
	}
	tags = append(tags, "gobi", "csv")
	for _, tag := range tags {
		if v := sf.Tag.Get(tag); v != "" {
			parts := strings.Split(v, ",")[1:]
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts
		}
	}
	return nil
}

// hasTagOption reports whether sf's name tag carries option opt.
func hasTagOption(sf reflect.StructField, o *structOpts, opt string) bool {
	for _, x := range fieldTagOptions(sf, o) {
		if x == opt {
			return true
		}
	}
	return false
}

// timestampTypeFromTag reads a parquet-go-style timestamp option off
// sf's name tag: `timestamp`, `timestamp(unit)`, or
// `timestamp(unit:utc|local)`. Options come from the first tag in the
// resolution chain (see fieldTagOptions), so
// `parquet:",timestamp(microsecond)"` works with an empty name part.
// Returns nil when the tag carries no timestamp option.
func timestampTypeFromTag(sf reflect.StructField, o *structOpts) (*arrow.TimestampType, error) {
	for _, opt := range fieldTagOptions(sf, o) {
		if opt != "timestamp" && !strings.HasPrefix(opt, "timestamp(") {
			continue
		}
		// parquet-go defaults: millisecond, adjusted to UTC.
		tt := &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"}
		if opt == "timestamp" {
			return tt, nil
		}
		if !strings.HasSuffix(opt, ")") {
			return nil, fmt.Errorf("%w: malformed timestamp option %q", ErrUnsupportedStructField, opt)
		}
		args := opt[len("timestamp(") : len(opt)-1]
		unit, zone, hasZone := strings.Cut(args, ":")
		switch unit {
		case "millisecond":
			tt.Unit = arrow.Millisecond
		case "microsecond":
			tt.Unit = arrow.Microsecond
		case "nanosecond":
			tt.Unit = arrow.Nanosecond
		default:
			return nil, fmt.Errorf("%w: timestamp unit %q (want millisecond, microsecond, or nanosecond)",
				ErrUnsupportedStructField, unit)
		}
		if hasZone {
			switch zone {
			case "utc":
			case "local":
				tt.TimeZone = ""
			default:
				return nil, fmt.Errorf("%w: timestamp zone %q (want utc or local)",
					ErrUnsupportedStructField, zone)
			}
		}
		return tt, nil
	}
	return nil, nil
}

// ErrUnsupportedStructField is returned when FromStructs / ToStructs
// encounters a struct field type it can't map to an arrow column.
// The error is wrap-friendly: errors.Is(err, ErrUnsupportedStructField)
// is true for every type-mapping failure.
var ErrUnsupportedStructField = errors.New("gobi: unsupported struct field type")

// ErrStructFieldOverflow is returned by ToStructs when a column value
// doesn't fit the struct field's width — e.g. an Int64 value past
// math.MaxInt16 read into an int16 field. Narrower fields are
// accepted (an Int64 column into an int32 field works) as long as
// every value fits; the check is per value, not per type.
var ErrStructFieldOverflow = errors.New("gobi: value overflows struct field")

// FromStructs builds a Frame from a slice of Go structs. Column
// order follows struct-field declaration order; unexported fields
// are ignored.
//
// Struct-tag conventions:
//
//	<format>:"col"      override the column name for the io format
//	                    passed via StructTagFormat (e.g. parquet:"col");
//	                    highest-priority name source when opts include
//	                    StructTagFormat("<format>").
//	gobi:"col"          universal name override, applied for every
//	                    io format when no format-specific tag matches.
//	csv:"col"           legacy fallback name override.
//	<tag>:"-"           skip this field entirely.
//	geom:"true"         geometry column — see below for value handling
//	time:"2006-01-02"   parse string field as time.Time using the layout;
//	                    ignored for time.Time-typed fields
//	<tag>:"col,timestamp(unit[:utc|local])"
//	                    timestamp precision + UTC adjustment, same
//	                    syntax as parquet-go — see below
//
// Timestamp precision. time.Time fields (and time-layout string
// fields) default to Timestamp[ns] with no time zone. A `timestamp`
// option on the name tag — whichever namespace supplies it — picks the
// unit and zone instead, following parquet-go's tag syntax so
// existing parquet-go structs keep their meaning:
//
//	timestamp                      Timestamp[ms, UTC] (parquet-go default)
//	timestamp(microsecond)         Timestamp[us, UTC]
//	timestamp(nanosecond:local)    Timestamp[ns], no zone
//
// Units are millisecond / microsecond / nanosecond; the zone is utc
// (the default: isAdjustedToUTC=true in parquet) or local
// (isAdjustedToUTC=false). Values are truncated to the unit. The
// option also applies to the elements of []time.Time / []*time.Time
// fields. On any other field type it is ignored.
//
// Geometry handling. A field tagged `geom:"true"` becomes a Binary
// arrow column tagged with GeometryField metadata:
//   - string field: value is parsed as WKT via geometry.ParseWKT and
//     emitted as WKB bytes.
//   - []byte field: value is emitted as-is (assumed already WKB).
//   - other field types: error.
//
// Nulls. Pointer-typed fields (*string, *int64, ...) emit null when
// the pointer is nil. Non-pointer fields never emit null — their
// zero value goes in.
//
// Supported non-tagged field types:
//
//	string, bool
//	int, int8, int16, int32, int64
//	uint, uint8, uint16, uint32, uint64
//	float32, float64
//	[]byte           (arrow Binary)
//	time.Time        (arrow Timestamp[ns]; see "Timestamp precision")
//	*T of any above  (nullable)
func FromStructs[T any](rows []T, opts ...StructOption) (*Frame, error) {
	var zero T
	tp := reflect.TypeOf(zero)
	if tp.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: T must be a struct, got %s", ErrUnsupportedStructField, tp.Kind())
	}
	so := resolveStructOpts(opts)
	plan, err := planStructFields(tp, so)
	if err != nil {
		return nil, err
	}
	sw := &structWriter{zeroTimeAsValue: so.zeroTimeAsValue}

	pool := so.allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}
	builders := make([]array.Builder, len(plan))
	for i, p := range plan {
		b, err := builderForType(pool, p.arrowType)
		if err != nil {
			return nil, fmt.Errorf("gobi: FromStructs: builder for %q: %w", p.name, err)
		}
		builders[i] = b
	}
	defer func() {
		for _, b := range builders {
			b.Release()
		}
	}()

	rowsVal := reflect.ValueOf(rows)
	nRows := rowsVal.Len()
	for r := 0; r < nRows; r++ {
		row := rowsVal.Index(r)
		for i, p := range plan {
			fv := row.Field(p.fieldIndex)
			if err := appendFieldValue(builders[i], fv, p, sw); err != nil {
				return nil, fmt.Errorf("gobi: FromStructs: row %d %q: %w", r, p.name, err)
			}
		}
	}

	// Assemble columns + Frame.
	fields := make([]arrow.Field, len(plan))
	cols := make([]arrow.Column, len(plan))
	for i, p := range plan {
		fields[i] = p.arrowField()
		arr := builders[i].NewArray()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		arr.Release()
		chunked.Release()
	}
	schema := arrow.NewSchema(fields, nil)
	return NewFrame(schema, cols)
}

// ToStructs converts a Frame back to a slice of Go structs, using
// the same struct-tag conventions as FromStructs.
//
// Columns are matched to fields by name (resolved via the `csv:"..."`
// tag or field name). A struct field with no matching column stays
// at its zero value. A frame column with no matching struct field
// is ignored.
//
// Null cells populate the zero value for non-pointer fields, or nil
// for pointer fields. Type mismatches between column and field
// return an error.
//
// Geometry columns (Binary with the geometry metadata) can be
// written back to a string field tagged `geom:"true"` (emits WKT
// via geometry.WKT()) or a []byte field tagged `geom:"true"` (raw
// WKB pass-through).
//
// Memory ownership. By default string and []byte fields alias the
// Frame's Arrow buffers (zero-copy). Any surviving struct then keeps
// its whole source buffer reachable, so a handful of long-lived rows
// can pin every batch they were decoded from, and the Frame's memory
// must stay valid while the structs are in use. Pass
// StructCopyValues() when the structs outlive the Frame. Tag
// low-cardinality string fields with the `intern` option
// (`gobi:"os,intern"`) to share one copy per distinct value, and pass
// a StructInterner to share it across calls. The io packages'
// ReadStructs wrappers copy by default.
func ToStructs[T any](f *Frame, opts ...StructOption) ([]T, error) {
	return toStructs[T](f, nil, "ToStructs", opts)
}

// ToStructsInto is ToStructs decoding into dst's backing array when it
// has room for f's rows, so a batch loop can reuse one slice instead of
// allocating per batch:
//
//	var rows []Row
//	err := parquetio.ReadFileChunksFunc(path, nil, func(f *gobi.Frame) error {
//	    var err error
//	    if rows, err = gobi.ToStructsInto(f, rows, gobi.StructCopyValues()); err != nil {
//	        return err
//	    }
//	    return process(rows)
//	})
//
// It returns dst[:f.NumRows()] (a fresh slice when cap(dst) is too
// small). Every returned element is reset to its zero value before
// decoding, so nothing leaks from dst's previous contents, and the
// rows previously in dst are overwritten: copy out anything that must
// outlive the next call. Pointer, slice and string fields still
// allocate as ToStructs would; the saving is the []T itself.
func ToStructsInto[T any](f *Frame, dst []T, opts ...StructOption) ([]T, error) {
	return toStructs(f, dst, "ToStructsInto", opts)
}

// toStructs is the shared body of ToStructs / ToStructsInto. dst, when
// non-nil with enough capacity, provides the output's backing array.
func toStructs[T any](f *Frame, dst []T, op string, opts []StructOption) ([]T, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: %s: nil frame", op)
	}
	var zero T
	tp := reflect.TypeOf(zero)
	if tp.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: T must be a struct, got %s", ErrUnsupportedStructField, tp.Kind())
	}
	so := resolveStructOpts(opts)
	plan, err := planStructFields(tp, so)
	if err != nil {
		return nil, err
	}

	// Map each plan entry to a column cursor (nil if absent). One
	// cursor per column, built once: the row loop below touches every
	// cell, and a per-cell chunk walk would cost rows × fields ×
	// chunks on multi-row-group parquet input.
	cols := make([]*chunkCursor, len(plan))
	for i, p := range plan {
		s, err := f.Column(p.name)
		if err != nil {
			if so.requireColumns {
				return nil, fmt.Errorf("gobi: %s: %w: %q (field %s)", op, ErrColumnNotFound, p.name, tp.Field(p.fieldIndex).Name)
			}
			// Column absent — that's OK, the field stays at zero.
			continue
		}
		cols[i] = newChunkCursor(s)
	}

	rd := &structReader{copyValues: so.copyValues, interner: so.interner, coerce: so.coerceNumbers}
	if rd.interner == nil {
		for _, p := range plan {
			if p.intern {
				rd.interner = NewStringInterner(0)
				break
			}
		}
	}

	nRows := f.NumRows()
	var out []T
	if cap(dst) >= nRows {
		out = dst[:nRows]
		clear(out)
	} else {
		out = make([]T, nRows)
	}
	rowsVal := reflect.ValueOf(out)
	for r := range nRows {
		row := rowsVal.Index(r)
		for i, p := range plan {
			if cols[i] == nil {
				continue
			}
			fv := row.Field(p.fieldIndex)
			if err := readFieldValue(fv, cols[i], r, p, rd); err != nil {
				return nil, fmt.Errorf("gobi: %s: row %d %q: %w", op, r, p.name, err)
			}
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Internals
// -----------------------------------------------------------------------------

// structFieldPlan describes one column derived from a struct field.
// Built once per struct type by planStructFields and reused across
// every row of the FromStructs / ToStructs walk.
type structFieldPlan struct {
	name       string
	fieldIndex int
	arrowType  arrow.DataType
	// Kind flags derived from tags — see planStructFields.
	isGeometry bool
	isTimeTag  bool
	timeLayout string
	// intern: the `intern` tag option — ToStructs interns this
	// string field's values (string, *string, []string, []*string).
	intern bool
	// required: FromStructs marks the column non-nullable and never
	// writes null for it (StructRequiredFields / `required` tag).
	required bool
	// Pointer wrapping: when true, the struct field is `*T`. Read
	// path checks for nil; write path allocates a fresh *T.
	isPointer bool
	// Actual reflect type of the (unwrapped) field. Used by the
	// time.Time detection since the direct field type may be
	// *time.Time under a nullable-time convention.
	valueType reflect.Type
}

func (p structFieldPlan) arrowField() arrow.Field {
	if p.isGeometry {
		// FromStructs doesn't know the caller's SRID — use 0 (unset).
		// Callers who need a specific EPSG on the column can set it
		// via GeometryField manually after construction.
		f := GeometryField(p.name, 0)
		f.Nullable = !p.required
		return f
	}
	return arrow.Field{Name: p.name, Type: p.arrowType, Nullable: !p.required}
}

// planStructFields reflects on tp and builds one structFieldPlan per
// exported field. o controls which struct-tag namespaces are consulted
// for column names — see resolveFieldName for the resolution order.
func planStructFields(tp reflect.Type, o *structOpts) ([]structFieldPlan, error) {
	timeType := reflect.TypeFor[time.Time]()
	out := make([]structFieldPlan, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		sf := tp.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, skip := resolveFieldName(sf, o)
		if skip {
			continue
		}
		ft := sf.Type
		isPtr := ft.Kind() == reflect.Ptr
		if isPtr {
			ft = ft.Elem()
		}

		// Reject *[]T (pointer to non-byte slice). Slices are
		// already nullable via nil, so the pointer wrapping is
		// ambiguous — the empty-list vs. missing-list distinction
		// would need a separate flag to survive round-trip.
		if isPtr && ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.Uint8 {
			return nil, fmt.Errorf("%w: field %q is *[]T; use []T (slices already convey nullability via nil)",
				ErrUnsupportedStructField, name)
		}

		tsType, err := timestampTypeFromTag(sf, o)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		fieldTS := tsType
		if fieldTS == nil {
			fieldTS = &arrow.TimestampType{Unit: arrow.Nanosecond}
		}

		// geom:"true" — geometry column.
		if sf.Tag.Get("geom") == "true" {
			if ft.Kind() != reflect.String && !(ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Uint8) {
				return nil, fmt.Errorf("%w: geom field %q must be string or []byte, got %s",
					ErrUnsupportedStructField, name, ft.Kind())
			}
			out = append(out, structFieldPlan{
				name: name, fieldIndex: i,
				arrowType:  arrow.BinaryTypes.Binary,
				isGeometry: true,
				isPointer:  isPtr,
				valueType:  ft,
			})
			continue
		}

		// time.Time field (with or without time tag).
		if ft == timeType {
			layout := sf.Tag.Get("time")
			out = append(out, structFieldPlan{
				name: name, fieldIndex: i,
				arrowType:  fieldTS,
				isTimeTag:  true,
				timeLayout: layout,
				isPointer:  isPtr,
				valueType:  ft,
			})
			continue
		}

		// String field with time:"..." tag — parse via layout.
		if ft.Kind() == reflect.String && sf.Tag.Get("time") != "" {
			out = append(out, structFieldPlan{
				name: name, fieldIndex: i,
				arrowType:  fieldTS,
				isTimeTag:  true,
				timeLayout: sf.Tag.Get("time"),
				isPointer:  isPtr,
				valueType:  ft,
			})
			continue
		}

		// []time.Time / []*time.Time with a timestamp option.
		if tsType != nil && ft.Kind() == reflect.Slice {
			elem := ft.Elem()
			if elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem == timeType {
				out = append(out, structFieldPlan{
					name: name, fieldIndex: i,
					arrowType: arrow.ListOf(tsType), isPointer: isPtr, valueType: ft,
				})
				continue
			}
		}

		dt, err := arrowTypeForField(ft)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		out = append(out, structFieldPlan{
			name: name, fieldIndex: i,
			arrowType: dt, isPointer: isPtr, valueType: ft,
		})
	}
	// REQUIRED: StructRequiredFields for non-pointer fields, or the
	// per-field `required` / `optional` tag options.
	for i := range out {
		sf := tp.Field(out[i].fieldIndex)
		req, opt := hasTagOption(sf, o, "required"), hasTagOption(sf, o, "optional")
		switch {
		case req && opt:
			return nil, fmt.Errorf("%w: field %q: both required and optional", ErrUnsupportedStructField, out[i].name)
		case req && out[i].isPointer:
			return nil, fmt.Errorf("%w: field %q: a pointer field is nullable by definition and can't be required", ErrUnsupportedStructField, out[i].name)
		case req:
			out[i].required = true
		case opt:
		default:
			out[i].required = o != nil && o.requiredFields && !out[i].isPointer
		}
	}
	// `intern` tag option: string-valued fields only.
	for i := range out {
		if !hasTagOption(tp.Field(out[i].fieldIndex), o, "intern") {
			continue
		}
		if !internable(out[i]) {
			return nil, fmt.Errorf("%w: field %q: intern applies to string, *string, []string and []*string fields",
				ErrUnsupportedStructField, out[i].name)
		}
		out[i].intern = true
	}
	return out, nil
}

// internable reports whether p's field holds plain strings (not WKT
// geometry or time-layout strings, which ToStructs builds fresh).
func internable(p structFieldPlan) bool {
	if p.isGeometry || p.isTimeTag {
		return false
	}
	t := p.valueType
	if t.Kind() == reflect.Slice {
		t = t.Elem()
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	return t.Kind() == reflect.String
}

// arrowTypeForField picks the arrow DataType for a struct field's
// Go type. Broader coverage than csvio's arrowTypeFor because
// FromStructs isn't parsing strings — it can accept every integer
// width + []byte directly.
//
// Slice fields (other than []byte) map to arrow ListType with an
// element type derived from the slice element. Pointer-typed
// elements (e.g. []*int64) preserve nullability of each element.
// Nested slices ([][]T) are not supported — flatten manually before
// calling FromStructs.
func arrowTypeForField(t reflect.Type) (arrow.DataType, error) {
	switch t.Kind() {
	case reflect.String:
		return arrow.BinaryTypes.String, nil
	case reflect.Bool:
		return arrow.FixedWidthTypes.Boolean, nil
	case reflect.Int, reflect.Int64:
		return arrow.PrimitiveTypes.Int64, nil
	case reflect.Int32:
		return arrow.PrimitiveTypes.Int32, nil
	case reflect.Int16:
		return arrow.PrimitiveTypes.Int16, nil
	case reflect.Int8:
		return arrow.PrimitiveTypes.Int8, nil
	case reflect.Uint, reflect.Uint64:
		return arrow.PrimitiveTypes.Uint64, nil
	case reflect.Uint32:
		return arrow.PrimitiveTypes.Uint32, nil
	case reflect.Uint16:
		return arrow.PrimitiveTypes.Uint16, nil
	case reflect.Uint8:
		return arrow.PrimitiveTypes.Uint8, nil
	case reflect.Float32:
		return arrow.PrimitiveTypes.Float32, nil
	case reflect.Float64:
		return arrow.PrimitiveTypes.Float64, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return arrow.BinaryTypes.Binary, nil
		}
		return arrowListType(t.Elem())
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedStructField, t.Kind())
}

// arrowListType builds arrow.ListOf(elem) for a Go slice element
// type. Element pointers (for nullable elements) unwrap to their
// pointee; time.Time elements map to Timestamp[ns].
func arrowListType(elem reflect.Type) (arrow.DataType, error) {
	if elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	if elem == reflect.TypeFor[time.Time]() {
		return arrow.ListOf(&arrow.TimestampType{Unit: arrow.Nanosecond}), nil
	}
	if elem.Kind() == reflect.Slice && elem.Elem().Kind() != reflect.Uint8 {
		return nil, fmt.Errorf("%w: nested slice not supported (flatten manually)", ErrUnsupportedStructField)
	}
	et, err := arrowTypeForField(elem)
	if err != nil {
		return nil, fmt.Errorf("list element: %w", err)
	}
	return arrow.ListOf(et), nil
}

// appendFieldValue writes one row's value for one field into the
// corresponding arrow builder. Handles the pointer-null,
// geometry-WKT, and time-parsing branches; scalar path routes
// through appendCustomValue with a Go-typed scalar.
// structWriter carries FromStructs' per-call value policy into the
// field writers.
type structWriter struct {
	zeroTimeAsValue bool
}

func appendFieldValue(b array.Builder, fv reflect.Value, p structFieldPlan, sw *structWriter) error {
	// Pointer field: null when nil, otherwise dereference.
	if p.isPointer {
		if fv.IsNil() {
			b.AppendNull()
			return nil
		}
		fv = fv.Elem()
	}

	if p.isGeometry {
		return appendGeomField(b, fv, p)
	}
	if p.isTimeTag {
		return appendTimeField(b, fv, p, sw)
	}
	if p.arrowType.ID() == arrow.LIST {
		return appendListField(b, fv, p.required)
	}

	// Scalar fields — extract as Go-typed value + append via
	// appendCustomValue. Widen every integer to int64 for the
	// Int64 builder path, similar for Uint64/Float64.
	switch p.arrowType.ID() {
	case arrow.STRING:
		b.(*array.StringBuilder).Append(fv.String())
	case arrow.BOOL:
		b.(*array.BooleanBuilder).Append(fv.Bool())
	case arrow.INT64:
		b.(*array.Int64Builder).Append(fv.Int())
	case arrow.INT32:
		b.(*array.Int32Builder).Append(int32(fv.Int()))
	case arrow.INT16:
		b.(*array.Int16Builder).Append(int16(fv.Int()))
	case arrow.INT8:
		b.(*array.Int8Builder).Append(int8(fv.Int()))
	case arrow.UINT64:
		b.(*array.Uint64Builder).Append(fv.Uint())
	case arrow.UINT32:
		b.(*array.Uint32Builder).Append(uint32(fv.Uint()))
	case arrow.UINT16:
		b.(*array.Uint16Builder).Append(uint16(fv.Uint()))
	case arrow.UINT8:
		b.(*array.Uint8Builder).Append(uint8(fv.Uint()))
	case arrow.FLOAT32:
		b.(*array.Float32Builder).Append(float32(fv.Float()))
	case arrow.FLOAT64:
		b.(*array.Float64Builder).Append(fv.Float())
	case arrow.BINARY:
		b.(*array.BinaryBuilder).Append(fv.Bytes())
	default:
		return fmt.Errorf("%w: unhandled arrow type %s", ErrUnsupportedStructField, p.arrowType)
	}
	return nil
}

// appendGeomField handles the geom:"true" tag path — string values
// go through WKT parsing; []byte values pass through as-is.
func appendGeomField(b array.Builder, fv reflect.Value, p structFieldPlan) error {
	bb, ok := b.(*array.BinaryBuilder)
	if !ok {
		return fmt.Errorf("geom builder isn't Binary: %T", b)
	}
	if p.valueType.Kind() == reflect.String {
		s := fv.String()
		if s == "" {
			if p.required {
				return fmt.Errorf("%w: empty geometry in a required field", ErrUnsupportedStructField)
			}
			bb.AppendNull()
			return nil
		}
		g, err := geometry.ParseWKT(s)
		if err != nil {
			return fmt.Errorf("parse WKT: %w", err)
		}
		bb.Append(geometry.WKB(g))
		return nil
	}
	// []byte path.
	bs := fv.Bytes()
	if bs == nil {
		if p.required {
			return fmt.Errorf("%w: nil geometry in a required field", ErrUnsupportedStructField)
		}
		bb.AppendNull()
		return nil
	}
	bb.Append(bs)
	return nil
}

// appendTimeField handles the time tag path. Two sub-cases: a
// time.Time-typed field (layout ignored, value used directly) and
// a string field with a layout tag (parse first).
func appendTimeField(b array.Builder, fv reflect.Value, p structFieldPlan, sw *structWriter) error {
	tb, ok := b.(*array.TimestampBuilder)
	if !ok {
		return fmt.Errorf("time builder isn't Timestamp: %T", b)
	}
	unit := p.arrowType.(*arrow.TimestampType).Unit
	timeType := reflect.TypeFor[time.Time]()
	if p.valueType == timeType {
		t := fv.Interface().(time.Time)
		if t.IsZero() && !p.required && !sw.zeroTimeAsValue {
			tb.AppendNull()
			return nil
		}
		ts, err := timestampOf(t, unit)
		if err != nil {
			return err
		}
		tb.Append(ts)
		return nil
	}
	// String field with time:"layout" tag.
	s := fv.String()
	if s == "" {
		if p.required {
			return fmt.Errorf("%w: empty time string in a required field", ErrUnsupportedStructField)
		}
		tb.AppendNull()
		return nil
	}
	t, err := time.Parse(p.timeLayout, s)
	if err != nil {
		return fmt.Errorf("parse time: %w", err)
	}
	ts, err := timestampOf(t, unit)
	if err != nil {
		return err
	}
	tb.Append(ts)
	return nil
}

// Nanosecond timestamps cover 1677-09-21 … 2262-04-11 (int64 ns from
// the epoch); outside that, time.Time.UnixNano is undefined.
var (
	minNanoTime = time.Unix(0, math.MinInt64).UTC()
	maxNanoTime = time.Unix(0, math.MaxInt64).UTC()
)

// timestampOf converts t to unit, failing instead of overflowing when
// t is outside the nanosecond range.
func timestampOf(t time.Time, unit arrow.TimeUnit) (arrow.Timestamp, error) {
	if unit == arrow.Nanosecond && (t.Before(minNanoTime) || t.After(maxNanoTime)) {
		return 0, fmt.Errorf("%w: %s is outside the Timestamp[ns] range (1677–2262); use a microsecond or millisecond unit, e.g. `timestamp(microsecond)`",
			ErrStructFieldOverflow, t.Format(time.RFC3339))
	}
	return arrow.TimestampFromTime(t, unit)
}

// readFieldValue populates one row's value on the struct side by
// reading from the arrow column. Inverse of appendFieldValue.
func readFieldValue(fv reflect.Value, cur *chunkCursor, row int, p structFieldPlan, rd *structReader) error {
	null, err := cur.isNull(row)
	if err != nil {
		return err
	}

	if p.isPointer {
		if null {
			// nil already; leave alone.
			return nil
		}
		// Allocate a fresh *T; then fv becomes the pointee for the
		// scalar-write path below.
		nv := reflect.New(fv.Type().Elem())
		fv.Set(nv)
		fv = nv.Elem()
	} else if null {
		// Non-pointer field, null cell → zero value. That's what
		// fv already is by default (struct-zero-value); no write
		// needed.
		return nil
	}

	if p.isGeometry {
		return readGeomField(fv, cur, row, p, rd)
	}
	if p.isTimeTag {
		return readTimeField(fv, cur, row, p)
	}
	if p.arrowType.ID() == arrow.LIST {
		return readListField(fv, cur, row, rd, p.intern)
	}

	v, err := cur.scalarAt(row)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return rd.assignOwned(fv, v, p.intern)
}

// readGeomField reads a geometry column back into a string field
// (as WKT) or a []byte field (raw WKB). Uses the plan's valueType
// to disambiguate.
func readGeomField(fv reflect.Value, cur *chunkCursor, row int, p structFieldPlan, rd *structReader) error {
	wkb, err := binaryAt(cur, row)
	if err != nil {
		return err
	}
	if p.valueType.Kind() == reflect.String {
		g, err := geometry.ParseWKB(wkb)
		if err != nil {
			return fmt.Errorf("parse WKB: %w", err)
		}
		// WKT is a method on each concrete geometry type, dispatched
		// via a type-switch below. Kept here rather than as a
		// top-level helper because the geometry package's public
		// surface follows the "T.WKT() string" idiom throughout.
		wkt, err := geometryToWKT(g)
		if err != nil {
			return err
		}
		fv.SetString(wkt)
		return nil
	}
	fv.SetBytes(rd.ownBytes(wkb))
	return nil
}

// readTimeField reads a Timestamp cell back to a time.Time field or
// a string field with layout.
func readTimeField(fv reflect.Value, cur *chunkCursor, row int, p structFieldPlan) error {
	chunk, local, err := cur.locate(row)
	if err != nil {
		return err
	}
	arr, i := resolveDictionary(chunk, local)
	ta, ok := arr.(*array.Timestamp)
	if !ok {
		return fmt.Errorf("time column not Timestamp: %T", arr)
	}
	// Honor the column's unit: FromStructs writes ns, but Spark / Trino
	// parquet is typically TIMESTAMP_MICROS.
	t := ta.Value(i).ToTime(ta.DataType().(*arrow.TimestampType).Unit)
	timeType := reflect.TypeFor[time.Time]()
	if p.valueType == timeType {
		fv.Set(reflect.ValueOf(t))
		return nil
	}
	fv.SetString(t.Format(p.timeLayout))
	return nil
}

// assignScalar sets fv to the Go-typed value v extracted from an
// arrow column. Widens integer + float promotions to match the
// struct field's declared kind.
func assignScalar(fv reflect.Value, v any) error {
	switch fv.Kind() {
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("string field got %T", v)
		}
		fv.SetString(s)
	case reflect.Bool:
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("bool field got %T", v)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var x int64
		switch n := v.(type) {
		case int64:
			x = n
		case int32:
			x = int64(n)
		case int16:
			x = int64(n)
		case int8:
			x = int64(n)
		default:
			return fmt.Errorf("int field got %T", v)
		}
		// reflect's SetInt truncates silently on a narrower field.
		if fv.OverflowInt(x) {
			return fmt.Errorf("%w: value %d overflows %s field", ErrStructFieldOverflow, x, fv.Type())
		}
		fv.SetInt(x)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		var x uint64
		switch n := v.(type) {
		case uint64:
			x = n
		case uint32:
			x = uint64(n)
		case uint16:
			x = uint64(n)
		case uint8:
			x = uint64(n)
		default:
			return fmt.Errorf("uint field got %T", v)
		}
		if fv.OverflowUint(x) {
			return fmt.Errorf("%w: value %d overflows %s field", ErrStructFieldOverflow, x, fv.Type())
		}
		fv.SetUint(x)
	case reflect.Float32, reflect.Float64:
		var x float64
		switch n := v.(type) {
		case float64:
			x = n
		case float32:
			x = float64(n)
		default:
			return fmt.Errorf("float field got %T", v)
		}
		// Range only: float64 → float32 rounding is not an error, but
		// a magnitude past float32's range would become ±Inf.
		if fv.OverflowFloat(x) {
			return fmt.Errorf("%w: value %g overflows %s field", ErrStructFieldOverflow, x, fv.Type())
		}
		fv.SetFloat(x)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("%w: slice of %s", ErrUnsupportedStructField, fv.Type().Elem().Kind())
		}
		bs, ok := v.([]byte)
		if !ok {
			return fmt.Errorf("[]byte field got %T", v)
		}
		fv.SetBytes(bs)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedStructField, fv.Kind())
	}
	return nil
}

// geometryToWKT dispatches to the concrete type's WKT() method.
// geometry has no top-level WKT() function — the encoding lives on
// each Geometry type — so this small switch keeps the FromStructs
// code path type-generic.
func geometryToWKT(g geometry.Geometry) (string, error) {
	switch t := g.(type) {
	case geometry.Point:
		return t.WKT(), nil
	case geometry.LineString:
		return t.WKT(), nil
	case geometry.Polygon:
		return t.WKT(), nil
	case geometry.MultiPoint:
		return t.WKT(), nil
	case geometry.MultiLineString:
		return t.WKT(), nil
	case geometry.MultiPolygon:
		return t.WKT(), nil
	case geometry.GeometryCollection:
		return t.WKT(), nil
	}
	return "", fmt.Errorf("%w: geometry type %T has no WKT encoder", ErrUnsupportedStructField, g)
}

// appendListField writes one row of a slice field into a
// ListBuilder. A nil slice becomes a null list; an empty non-nil
// slice becomes a zero-length list. Pointer-typed elements
// (e.g. []*int64) preserve per-element nullability.
func appendListField(b array.Builder, fv reflect.Value, required bool) error {
	lb, ok := b.(*array.ListBuilder)
	if !ok {
		return fmt.Errorf("list builder isn't ListBuilder: %T", b)
	}
	if fv.Kind() != reflect.Slice {
		return fmt.Errorf("list field isn't a slice: %s", fv.Kind())
	}
	if fv.IsNil() && !required {
		lb.AppendNull()
		return nil
	}
	lb.Append(true) // a required nil slice is an empty list
	inner := lb.ValueBuilder()
	elemIsPtr := fv.Type().Elem().Kind() == reflect.Pointer
	n := fv.Len()
	for i := range n {
		elem := fv.Index(i)
		if elemIsPtr {
			if elem.IsNil() {
				inner.AppendNull()
				continue
			}
			elem = elem.Elem()
		}
		if err := appendListElement(inner, elem); err != nil {
			return fmt.Errorf("elem %d: %w", i, err)
		}
	}
	return nil
}

// appendListElement writes one slice element into the inner
// builder of a ListBuilder. Mirrors appendFieldValue's scalar
// switch, plus a time.Time branch.
func appendListElement(b array.Builder, elem reflect.Value) error {
	switch b := b.(type) {
	case *array.StringBuilder:
		b.Append(elem.String())
	case *array.BooleanBuilder:
		b.Append(elem.Bool())
	case *array.Int64Builder:
		b.Append(elem.Int())
	case *array.Int32Builder:
		b.Append(int32(elem.Int()))
	case *array.Int16Builder:
		b.Append(int16(elem.Int()))
	case *array.Int8Builder:
		b.Append(int8(elem.Int()))
	case *array.Uint64Builder:
		b.Append(elem.Uint())
	case *array.Uint32Builder:
		b.Append(uint32(elem.Uint()))
	case *array.Uint16Builder:
		b.Append(uint16(elem.Uint()))
	case *array.Uint8Builder:
		b.Append(uint8(elem.Uint()))
	case *array.Float64Builder:
		b.Append(elem.Float())
	case *array.Float32Builder:
		b.Append(float32(elem.Float()))
	case *array.BinaryBuilder:
		b.Append(elem.Bytes())
	case *array.TimestampBuilder:
		t, ok := elem.Interface().(time.Time)
		if !ok {
			return fmt.Errorf("timestamp element got %T", elem.Interface())
		}
		ts, err := timestampOf(t, b.Type().(*arrow.TimestampType).Unit)
		if err != nil {
			return err
		}
		b.Append(ts)
	default:
		return fmt.Errorf("%w: unsupported list element builder %T", ErrUnsupportedStructField, b)
	}
	return nil
}

// readListField reads a List cell into a Go slice field. Nil-list
// rows already returned via the null-check in readFieldValue, so
// this only handles non-null rows.
func readListField(fv reflect.Value, cur *chunkCursor, row int, rd *structReader, intern bool) error {
	chunk, local, err := cur.locate(row)
	if err != nil {
		return err
	}
	var start, end int64
	var values arrow.Array
	switch la := chunk.(type) {
	case *array.List:
		start, end = la.ValueOffsets(local)
		values = la.ListValues()
	case *array.LargeList:
		start, end = la.ValueOffsets(local)
		values = la.ListValues()
	default:
		return fmt.Errorf("list column not List, got %T", chunk)
	}
	n := int(end - start)

	sliceType := fv.Type()
	elemType := sliceType.Elem()
	elemIsPtr := elemType.Kind() == reflect.Pointer
	sliceVal := reflect.MakeSlice(sliceType, n, n)
	for i := range n {
		idx := int(start) + i
		target := sliceVal.Index(i)
		if isNullArr(values, idx) {
			// Non-pointer elements stay at zero value; pointer
			// elements stay nil (MakeSlice's default).
			continue
		}
		if elemIsPtr {
			nv := reflect.New(elemType.Elem())
			if err := assignListElement(nv.Elem(), values, idx, rd, intern); err != nil {
				return fmt.Errorf("elem %d: %w", i, err)
			}
			target.Set(nv)
			continue
		}
		if err := assignListElement(target, values, idx, rd, intern); err != nil {
			return fmt.Errorf("elem %d: %w", i, err)
		}
	}
	fv.Set(sliceVal)
	return nil
}

// assignListElement reads one element from a list's inner Array
// into fv, dispatching on the array's concrete type.
func assignListElement(fv reflect.Value, arr arrow.Array, idx int, rd *structReader, intern bool) error {
	// Dictionary-encoded elements resolve to their value array.
	arr, idx = resolveDictionary(arr, idx)
	if ta, ok := arr.(*array.Timestamp); ok {
		if fv.Type() != reflect.TypeFor[time.Time]() {
			return fmt.Errorf("timestamp element into %s field", fv.Type())
		}
		t := ta.Value(idx).ToTime(ta.DataType().(*arrow.TimestampType).Unit)
		fv.Set(reflect.ValueOf(t))
		return nil
	}
	// Scalars share the top-level field path, so list elements get the
	// same type checks and overflow errors instead of reflect panics or
	// silent truncation.
	v, err := readArrayScalar(arr, idx)
	if err != nil {
		return fmt.Errorf("%w: unsupported list element array %T", ErrUnsupportedStructField, arr)
	}
	return rd.assignOwned(fv, v, intern)
}

// binaryAt reads a Binary cell's raw bytes at row from a Series.
// Returns nil for null cells.
func binaryAt(cur *chunkCursor, row int) ([]byte, error) {
	chunk, local, err := cur.locate(row)
	if err != nil {
		return nil, err
	}
	arr, i := resolveDictionary(chunk, local)
	if arr.IsNull(i) {
		return nil, nil
	}
	switch ba := arr.(type) {
	case *array.Binary:
		return ba.Value(i), nil
	case *array.LargeBinary:
		return ba.Value(i), nil
	}
	return nil, fmt.Errorf("column not Binary, got %T", arr)
}

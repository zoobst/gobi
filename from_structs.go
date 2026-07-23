package gobi

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// ErrUnsupportedStructField is returned when FromStructs / ToStructs
// encounters a struct field type it can't map to an arrow column.
// The error is wrap-friendly: errors.Is(err, ErrUnsupportedStructField)
// is true for every type-mapping failure.
var ErrUnsupportedStructField = errors.New("gobi: unsupported struct field type")

// FromStructs builds a Frame from a slice of Go structs. Column
// order follows struct-field declaration order; unexported fields
// are ignored.
//
// Struct-tag conventions (shared with csvio):
//
//	csv:"col"           override the column name (default: field name)
//	geom:"true"         geometry column — see below for value handling
//	time:"2006-01-02"   parse string field as time.Time using the layout;
//	                    ignored for time.Time-typed fields
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
//	time.Time        (arrow Timestamp[ns])
//	*T of any above  (nullable)
func FromStructs[T any](rows []T) (*Frame, error) {
	var zero T
	tp := reflect.TypeOf(zero)
	if tp.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: T must be a struct, got %s", ErrUnsupportedStructField, tp.Kind())
	}
	plan, err := planStructFields(tp)
	if err != nil {
		return nil, err
	}

	pool := memory.DefaultAllocator
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
			if err := appendFieldValue(builders[i], fv, p); err != nil {
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
func ToStructs[T any](f *Frame) ([]T, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: ToStructs: nil frame")
	}
	var zero T
	tp := reflect.TypeOf(zero)
	if tp.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: T must be a struct, got %s", ErrUnsupportedStructField, tp.Kind())
	}
	plan, err := planStructFields(tp)
	if err != nil {
		return nil, err
	}

	// Map each plan entry to a column index in f (or -1 if absent).
	cols := make([]Series, len(plan))
	present := make([]bool, len(plan))
	for i, p := range plan {
		s, err := f.Column(p.name)
		if err != nil {
			// Column absent — that's OK, the field stays at zero.
			continue
		}
		cols[i] = s
		present[i] = true
	}

	nRows := f.NumRows()
	out := make([]T, nRows)
	rowsVal := reflect.ValueOf(out)
	for r := 0; r < nRows; r++ {
		row := rowsVal.Index(r)
		for i, p := range plan {
			if !present[i] {
				continue
			}
			fv := row.Field(p.fieldIndex)
			if err := readFieldValue(fv, cols[i], r, p); err != nil {
				return nil, fmt.Errorf("gobi: ToStructs: row %d %q: %w", r, p.name, err)
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
		return GeometryField(p.name, 0)
	}
	return arrow.Field{Name: p.name, Type: p.arrowType, Nullable: true}
}

// planStructFields reflects on tp and builds one structFieldPlan per
// exported field.
func planStructFields(tp reflect.Type) ([]structFieldPlan, error) {
	timeType := reflect.TypeFor[time.Time]()
	out := make([]structFieldPlan, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		sf := tp.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := sf.Tag.Get("csv")
		if name == "" {
			name = sf.Name
		}
		ft := sf.Type
		isPtr := ft.Kind() == reflect.Ptr
		if isPtr {
			ft = ft.Elem()
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
				arrowType:  &arrow.TimestampType{Unit: arrow.Nanosecond},
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
				arrowType:  &arrow.TimestampType{Unit: arrow.Nanosecond},
				isTimeTag:  true,
				timeLayout: sf.Tag.Get("time"),
				isPointer:  isPtr,
				valueType:  ft,
			})
			continue
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
	return out, nil
}

// arrowTypeForField picks the arrow DataType for a struct field's
// Go type. Broader coverage than csvio's arrowTypeFor because
// FromStructs isn't parsing strings — it can accept every integer
// width + []byte directly.
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
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedStructField, t.Kind())
}

// appendFieldValue writes one row's value for one field into the
// corresponding arrow builder. Handles the pointer-null,
// geometry-WKT, and time-parsing branches; scalar path routes
// through appendCustomValue with a Go-typed scalar.
func appendFieldValue(b array.Builder, fv reflect.Value, p structFieldPlan) error {
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
		return appendTimeField(b, fv, p)
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
		bb.AppendNull()
		return nil
	}
	bb.Append(bs)
	return nil
}

// appendTimeField handles the time tag path. Two sub-cases: a
// time.Time-typed field (layout ignored, value used directly) and
// a string field with a layout tag (parse first).
func appendTimeField(b array.Builder, fv reflect.Value, p structFieldPlan) error {
	tb, ok := b.(*array.TimestampBuilder)
	if !ok {
		return fmt.Errorf("time builder isn't Timestamp: %T", b)
	}
	timeType := reflect.TypeFor[time.Time]()
	if p.valueType == timeType {
		t := fv.Interface().(time.Time)
		if t.IsZero() {
			tb.AppendNull()
			return nil
		}
		tb.Append(arrow.Timestamp(t.UnixNano()))
		return nil
	}
	// String field with time:"layout" tag.
	s := fv.String()
	if s == "" {
		tb.AppendNull()
		return nil
	}
	t, err := time.Parse(p.timeLayout, s)
	if err != nil {
		return fmt.Errorf("parse time: %w", err)
	}
	tb.Append(arrow.Timestamp(t.UnixNano()))
	return nil
}

// readFieldValue populates one row's value on the struct side by
// reading from the arrow column. Inverse of appendFieldValue.
func readFieldValue(fv reflect.Value, s Series, row int, p structFieldPlan) error {
	null, err := isNullAtSeries(s, row)
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
		return readGeomField(fv, s, row, p)
	}
	if p.isTimeTag {
		return readTimeField(fv, s, row, p)
	}

	v, err := readScalarAt(s, row)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return assignScalar(fv, v)
}

// readGeomField reads a geometry column back into a string field
// (as WKT) or a []byte field (raw WKB). Uses the plan's valueType
// to disambiguate.
func readGeomField(fv reflect.Value, s Series, row int, p structFieldPlan) error {
	wkb, err := binaryAt(s, row)
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
	fv.SetBytes(wkb)
	return nil
}

// readTimeField reads a Timestamp cell back to a time.Time field or
// a string field with layout.
func readTimeField(fv reflect.Value, s Series, row int, p structFieldPlan) error {
	v, err := readScalarAt(s, row)
	if err != nil {
		return err
	}
	ts, ok := v.(arrow.Timestamp)
	if !ok {
		return fmt.Errorf("time column not Timestamp: %T", v)
	}
	t := time.Unix(0, int64(ts)).UTC()
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
		switch x := v.(type) {
		case int64:
			fv.SetInt(x)
		case int32:
			fv.SetInt(int64(x))
		default:
			return fmt.Errorf("int field got %T", v)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		switch x := v.(type) {
		case uint64:
			fv.SetUint(x)
		case uint32:
			fv.SetUint(uint64(x))
		default:
			return fmt.Errorf("uint field got %T", v)
		}
	case reflect.Float32, reflect.Float64:
		switch x := v.(type) {
		case float64:
			fv.SetFloat(x)
		case float32:
			fv.SetFloat(float64(x))
		default:
			return fmt.Errorf("float field got %T", v)
		}
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

// binaryAt reads a Binary cell's raw bytes at row from a Series.
// Returns nil for null cells.
func binaryAt(s Series, row int) ([]byte, error) {
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return nil, nil
			}
			ba, ok := chunk.(*array.Binary)
			if !ok {
				return nil, fmt.Errorf("column not Binary, got %T", chunk)
			}
			return ba.Value(local), nil
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("row %d out of range", row)
}

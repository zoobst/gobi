// Package csvio reads and writes CSV data as gobi Frames.
//
// The reader infers each column's Arrow type from a user-supplied Go struct
// whose fields carry `csv:"header"` and, optionally, `geom:"true"` tags. This
// approach mirrors encoding/csv but yields a strongly-typed Arrow schema.
package csvio

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// Errors.
var (
	ErrUnsupportedFieldType = errors.New("csvio: unsupported field type")
	ErrHeaderMissing        = errors.New("csvio: expected header row not found in file")
	ErrRowFieldCountMismatch = errors.New("csvio: row field count does not match schema")
)

// Options controls CSV parsing.
type Options struct {
	// HasHeader indicates whether the first row is a header. Defaults to true.
	HasHeader *bool
	// Delimiter overrides the default comma.
	Delimiter rune
	// Comment marks lines that should be skipped when starting with this rune.
	Comment rune
	// SkipRows drops the first N data rows (after any header).
	SkipRows int
	// CRSHint gives geometry columns a CRS when the CSV does not encode one.
	CRSHint int32
	// Allocator overrides the Arrow allocator.
	Allocator memory.Allocator
}

func (o *Options) hasHeader() bool {
	if o == nil || o.HasHeader == nil {
		return true
	}
	return *o.HasHeader
}

// ReadFile reads path into a Frame, inferring the schema from T.
func ReadFile[T any](path string, opts *Options) (*gobi.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read[T](f, opts)
}

// Read reads r into a Frame, inferring the schema from T.
func Read[T any](r io.Reader, opts *Options) (*gobi.Frame, error) {
	if opts == nil {
		opts = &Options{}
	}
	fields, tags, err := reflectStruct[T](opts.CRSHint)
	if err != nil {
		return nil, err
	}
	schema := arrow.NewSchema(fields, nil)

	pool := opts.Allocator
	if pool == nil {
		pool = memory.DefaultAllocator
	}

	builders := make([]array.Builder, len(fields))
	for i, f := range fields {
		b, err := builderFor(pool, f.Type)
		if err != nil {
			return nil, err
		}
		builders[i] = b
	}
	defer func() {
		for _, b := range builders {
			b.Release()
		}
	}()

	cr := csv.NewReader(r)
	if opts.Delimiter != 0 {
		cr.Comma = opts.Delimiter
	}
	if opts.Comment != 0 {
		cr.Comment = opts.Comment
	}
	cr.FieldsPerRecord = len(fields)

	if opts.hasHeader() {
		if _, err := cr.Read(); err != nil {
			if err == io.EOF {
				return nil, ErrHeaderMissing
			}
			return nil, err
		}
	}
	for skipped := 0; skipped < opts.SkipRows; skipped++ {
		if _, err := cr.Read(); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}

	for rowIdx := 0; ; rowIdx++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csvio: read row %d: %w", rowIdx, err)
		}
		if len(rec) != len(fields) {
			return nil, fmt.Errorf("%w: row %d has %d fields, expected %d",
				ErrRowFieldCountMismatch, rowIdx, len(rec), len(fields))
		}
		for i, raw := range rec {
			if err := appendCell(builders[i], tags[i], raw); err != nil {
				return nil, fmt.Errorf("csvio: row %d col %q: %w", rowIdx, fields[i].Name, err)
			}
		}
	}

	arrays := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrays[i] = b.NewArray()
	}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()

	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	return gobi.NewFrame(schema, cols)
}

// fieldTags is the per-column tag data captured from the struct.
type fieldTags struct {
	geometry bool
}

func reflectStruct[T any](crsHint int32) ([]arrow.Field, []fieldTags, error) {
	var zero T
	tp := reflect.TypeOf(zero)
	if tp.Kind() != reflect.Struct {
		return nil, nil, fmt.Errorf("%w: T must be a struct, got %s", ErrUnsupportedFieldType, tp.Kind())
	}
	fields := make([]arrow.Field, 0, tp.NumField())
	tags := make([]fieldTags, 0, tp.NumField())
	for i := 0; i < tp.NumField(); i++ {
		sf := tp.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := sf.Tag.Get("csv")
		if name == "" {
			name = sf.Name
		}
		isGeom := sf.Tag.Get("geom") == "true"
		if isGeom {
			fields = append(fields, gobi.GeometryField(name, crsHint))
			tags = append(tags, fieldTags{geometry: true})
			continue
		}
		dt, err := arrowTypeFor(sf.Type)
		if err != nil {
			return nil, nil, err
		}
		fields = append(fields, arrow.Field{Name: name, Type: dt, Nullable: true})
		tags = append(tags, fieldTags{})
	}
	return fields, tags, nil
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

func builderFor(pool memory.Allocator, t arrow.DataType) (array.Builder, error) {
	switch t.ID() {
	case arrow.STRING:
		return array.NewStringBuilder(pool), nil
	case arrow.BOOL:
		return array.NewBooleanBuilder(pool), nil
	case arrow.INT64:
		return array.NewInt64Builder(pool), nil
	case arrow.INT32:
		return array.NewInt32Builder(pool), nil
	case arrow.FLOAT64:
		return array.NewFloat64Builder(pool), nil
	case arrow.FLOAT32:
		return array.NewFloat32Builder(pool), nil
	case arrow.BINARY:
		return array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFieldType, t)
	}
}

func appendCell(b array.Builder, tags fieldTags, raw string) error {
	if tags.geometry {
		bb := b.(*array.BinaryBuilder)
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			bb.AppendNull()
			return nil
		}
		g, err := geometry.ParseWKT(trimmed)
		if err != nil {
			return err
		}
		bb.Append(geometry.WKB(g))
		return nil
	}
	if raw == "" {
		b.AppendNull()
		return nil
	}
	switch tb := b.(type) {
	case *array.StringBuilder:
		tb.Append(raw)
	case *array.BooleanBuilder:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		tb.Append(v)
	case *array.Int64Builder:
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		tb.Append(v)
	case *array.Int32Builder:
		v, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return err
		}
		tb.Append(int32(v))
	case *array.Float64Builder:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		tb.Append(v)
	case *array.Float32Builder:
		v, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return err
		}
		tb.Append(float32(v))
	default:
		return fmt.Errorf("%w: builder type %T", ErrUnsupportedFieldType, b)
	}
	return nil
}

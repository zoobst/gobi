package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/geometry"
)

// Series is a single named, typed Arrow column.
//
// A Series does not own its column: constructors that receive an *arrow.Column
// treat it as borrowed. Callers wanting shared lifetime should Retain the
// underlying column; DataFrame.Release does not release Series contents.
type Series struct {
	name  string
	field arrow.Field
	col   *arrow.Column
}

// NewSeries returns a Series wrapping col. The Series takes the column's
// name from col.Name(); its field is derived from col.Field().
func NewSeries(col *arrow.Column) Series {
	return Series{name: col.Name(), field: col.Field(), col: col}
}

// Name returns the column name.
func (s Series) Name() string { return s.name }

// Len returns the number of rows.
func (s Series) Len() int {
	if s.col == nil {
		return 0
	}
	return s.col.Len()
}

// DataType returns the Arrow data type.
func (s Series) DataType() arrow.DataType {
	if s.col == nil {
		return nil
	}
	return s.col.DataType()
}

// IsGeometry reports whether the series is tagged as a WKB geometry column.
func (s Series) IsGeometry() bool { return isGeometryField(s.field) }

// Head returns a Series with the first n rows. n<=0 means default 5.
func (s Series) Head(n int) Series {
	if n <= 0 {
		n = 5
	}
	return s.slice(0, min(int64(n), int64(s.Len())))
}

// Tail returns a Series with the last n rows. n<=0 means default 5.
func (s Series) Tail(n int) Series {
	if n <= 0 {
		n = 5
	}
	length := int64(s.Len())
	start := max(length-int64(n), 0)
	return s.slice(start, length)
}

// Row returns a Series containing the single row at index i.
func (s Series) Row(i int) (Series, error) {
	if i < 0 || i >= s.Len() {
		return Series{}, fmt.Errorf("%w: %d not in [0,%d)", ErrRowOutOfRange, i, s.Len())
	}
	return s.slice(int64(i), int64(i+1)), nil
}

func (s Series) slice(start, end int64) Series {
	sliced := array.NewColumnSlice(s.col, start, end)
	return Series{name: s.name, field: s.field, col: sliced}
}

// Column returns the underlying Arrow column. Do not mutate the returned
// value; use Series methods for slicing.
func (s Series) Column() *arrow.Column { return s.col }

// Geometry decodes the WKB at row i into a Geometry. Returns ErrNotGeometry
// if the series is not a geometry column.
func (s Series) Geometry(i int) (geometry.Geometry, error) {
	if !s.IsGeometry() {
		return nil, ErrNotGeometry
	}
	if i < 0 || i >= s.Len() {
		return nil, fmt.Errorf("%w: %d not in [0,%d)", ErrRowOutOfRange, i, s.Len())
	}
	// walk the chunks to find the right one
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if i < offset+chunk.Len() {
			local := i - offset
			bin, ok := chunk.(*array.Binary)
			if !ok {
				return nil, fmt.Errorf("%w: unexpected chunk type %T", ErrColumnTypeMismatch, chunk)
			}
			if bin.IsNull(local) {
				return nil, nil
			}
			return geometry.ParseWKB(bin.Value(local))
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("%w: index %d unreachable", ErrRowOutOfRange, i)
}

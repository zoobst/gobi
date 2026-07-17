package gobi

import (
	"fmt"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
)

// Frame is a columnar dataset: an Arrow schema plus a set of named Series.
//
// The API mirrors GeoPandas / Polars in shape: Shape, Head, Tail, Row,
// Column, and geometry helpers.
type Frame struct {
	schema *arrow.Schema
	series []Series
}

// NewFrame builds a Frame from a schema and a set of arrow columns. The
// number of columns must match the schema.
func NewFrame(schema *arrow.Schema, cols []arrow.Column) (*Frame, error) {
	if len(cols) != len(schema.Fields()) {
		return nil, fmt.Errorf("%w: schema has %d fields, got %d columns",
			ErrColumnLenMismatch, len(schema.Fields()), len(cols))
	}
	if len(cols) > 1 {
		want := cols[0].Len()
		for i := 1; i < len(cols); i++ {
			if cols[i].Len() != want {
				return nil, fmt.Errorf("%w: column %q has %d rows, expected %d",
					ErrColumnLenMismatch, cols[i].Name(), cols[i].Len(), want)
			}
		}
	}
	series := make([]Series, len(cols))
	for i := range cols {
		c := cols[i]
		series[i] = NewSeries(&c)
	}
	return &Frame{schema: schema, series: series}, nil
}

// NewFrameFromTable adopts the columns of t.
func NewFrameFromTable(t arrow.Table) *Frame {
	series := make([]Series, t.NumCols())
	for i := range t.NumCols() {
		series[i] = NewSeries(t.Column(int(i)))
	}
	return &Frame{schema: t.Schema(), series: series}
}

// Schema returns the underlying Arrow schema.
func (f *Frame) Schema() *arrow.Schema { return f.schema }

// NumRows returns the number of rows in the frame (0 if there are no
// columns).
func (f *Frame) NumRows() int {
	if len(f.series) == 0 {
		return 0
	}
	return f.series[0].Len()
}

// NumCols returns the number of columns.
func (f *Frame) NumCols() int { return len(f.series) }

// Shape returns (rows, cols) — matching Pandas convention.
func (f *Frame) Shape() (rows, cols int) { return f.NumRows(), f.NumCols() }

// ColumnNames returns the column names in order.
func (f *Frame) ColumnNames() []string {
	out := make([]string, len(f.series))
	for i, s := range f.series {
		out[i] = s.name
	}
	return out
}

// Column returns the Series named name.
func (f *Frame) Column(name string) (Series, error) {
	for _, s := range f.series {
		if s.name == name {
			return s, nil
		}
	}
	return Series{}, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
}

// ColumnAt returns the Series at position i.
func (f *Frame) ColumnAt(i int) (Series, error) {
	if i < 0 || i >= len(f.series) {
		return Series{}, fmt.Errorf("%w: column %d not in [0,%d)",
			ErrRowOutOfRange, i, len(f.series))
	}
	return f.series[i], nil
}

// Head returns a Frame with the first n rows (default 5).
func (f *Frame) Head(n int) *Frame {
	if n <= 0 {
		n = 5
	}
	if n > f.NumRows() {
		n = f.NumRows()
	}
	return f.slice(0, int64(n))
}

// Tail returns a Frame with the last n rows (default 5).
func (f *Frame) Tail(n int) *Frame {
	if n <= 0 {
		n = 5
	}
	length := int64(f.NumRows())
	start := max(length-int64(n), 0)
	return f.slice(start, length)
}

// Row returns a Frame containing the single row at index i.
func (f *Frame) Row(i int) (*Frame, error) {
	if i < 0 || i >= f.NumRows() {
		return nil, fmt.Errorf("%w: %d not in [0,%d)",
			ErrRowOutOfRange, i, f.NumRows())
	}
	return f.slice(int64(i), int64(i+1)), nil
}

func (f *Frame) slice(start, end int64) *Frame {
	out := &Frame{schema: f.schema, series: make([]Series, len(f.series))}
	for i, s := range f.series {
		out.series[i] = s.slice(start, end)
	}
	return out
}

// Retain increments the reference count of the underlying Arrow columns.
func (f *Frame) Retain() {
	for _, s := range f.series {
		if s.col != nil {
			s.col.Retain()
		}
	}
}

// Release decrements the reference count of the underlying Arrow columns.
// Callers should match every Retain (including the implicit one at
// construction) with exactly one Release.
func (f *Frame) Release() {
	for _, s := range f.series {
		if s.col != nil {
			s.col.Release()
		}
	}
}

// String returns a debug representation of the frame. Not intended for
// pretty printing at scale.
func (f *Frame) String() string {
	rows, cols := f.Shape()
	var b strings.Builder
	fmt.Fprintf(&b, "Frame(%d rows × %d cols)\n", rows, cols)
	for _, s := range f.series {
		fmt.Fprintf(&b, "  %s: %s", s.name, s.DataType())
		if s.IsGeometry() {
			fmt.Fprintf(&b, " [geometry, EPSG:%d]", geometryCRSFromField(s.field))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Geometry decodes the geometry at (row, colName). Returns ErrNotGeometry
// if the column is not a geometry column.
func (f *Frame) Geometry(colName string, row int) (any, error) {
	s, err := f.Column(colName)
	if err != nil {
		return nil, err
	}
	return s.Geometry(row)
}

// Table returns an arrow.Table view of the frame. The returned table shares
// buffers with the frame — releasing one releases the other.
func (f *Frame) Table() arrow.Table {
	cols := make([]arrow.Column, len(f.series))
	for i, s := range f.series {
		cols[i] = *s.col
	}
	return array.NewTable(f.schema, cols, int64(f.NumRows()))
}

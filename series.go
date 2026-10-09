package gobi

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
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

// SeriesFromArray wraps a freshly-built arrow.Array in a Series with
// the given field. Handles the full ref-count dance so callers don't
// have to hand-roll it:
//
//   - `arr.Release()` is called inline after the wrapping. Callers
//     transfer their ref on arr into this function and must NOT
//     release it themselves. The Arrow Chunked constructed here
//     retained arr internally, so arr's buffer survives on the
//     Chunked-held reference.
//   - The intermediate Chunked's constructor reference is also
//     released here (NewColumn retained it separately). After return,
//     only the Column-held reference to the Chunked is alive.
//
// End state: `arr.refCount == 1` (owned by Chunked) and
// `chunked.refCount == 1` (owned by Column). No leaks, no
// double-frees.
//
// Motivating shape: sibling packages that build custom column types
// (geometry columns, hash columns, ML feature columns) previously
// had to write the Chunked+NewColumn ceremony inline and remember
// the two `.Release()` calls that pair with NewChunked's and
// NewColumn's internal retains. This helper folds all of that into
// one function so downstream code becomes:
//
//	arr := b.NewArray()
//	series := gobi.SeriesFromArray(
//	    gobi.GeometryField("points", 4326), arr)
//
// Instead of the 5-line hand-rolled equivalent.
func SeriesFromArray(field arrow.Field, arr arrow.Array) Series {
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	arr.Release()
	chunked.Release()
	return Series{name: field.Name, field: field, col: col}
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

// Typed Series constructors from Go slices. Each takes an optional
// validity mask: nil means every row is valid; otherwise validity[i]
// == false marks row i null and its value is ignored. A non-nil
// validity whose length differs from the values panics, like an
// out-of-range slice index — it's a programming error, not bad data.

// NewStringSeries builds a String (utf8) Series from vals.
func NewStringSeries(name string, vals []string, validity []bool) Series {
	checkValidityLen("NewStringSeries", len(vals), validity)
	b := array.NewStringBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

// NewFloat64Series builds a Float64 Series from vals.
func NewFloat64Series(name string, vals []float64, validity []bool) Series {
	checkValidityLen("NewFloat64Series", len(vals), validity)
	b := array.NewFloat64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

// NewInt64Series builds an Int64 Series from vals.
func NewInt64Series(name string, vals []int64, validity []bool) Series {
	checkValidityLen("NewInt64Series", len(vals), validity)
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

// NewBoolSeries builds a Boolean Series from vals.
func NewBoolSeries(name string, vals []bool, validity []bool) Series {
	checkValidityLen("NewBoolSeries", len(vals), validity)
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

// NewTimestampSeriesUnit builds a Timestamp Series stored in unit, with
// time zone "UTC", from ts. A time.Time is an absolute instant, so the
// column is zone-aware: Parquet writes it isAdjustedToUTC=true, and
// pyarrow / pandas read it as datetime64[unit, UTC]. Use WithTimezone
// to label another zone (the instants don't change).
//
// Unlike NewTimestampSeries, which is
// nanosecond-only and so limited to 1677–2262, a coarser unit covers
// a wider range: microseconds reach ±292,000 years. Sub-unit precision
// is floored (toward the past). A valid time outside the unit's int64
// range is an error rather than a wrapped value.
func NewTimestampSeriesUnit(name string, ts []time.Time, validity []bool, unit arrow.TimeUnit) (Series, error) {
	checkValidityLen("NewTimestampSeriesUnit", len(ts), validity)
	var perSec int64
	switch unit {
	case arrow.Second:
		perSec = 1
	case arrow.Millisecond:
		perSec = 1e3
	case arrow.Microsecond:
		perSec = 1e6
	case arrow.Nanosecond:
		perSec = 1e9
	default:
		return Series{}, fmt.Errorf("gobi: NewTimestampSeriesUnit: unknown unit %v", unit)
	}
	nsPerUnit := int64(1e9) / perSec
	vals := make([]arrow.Timestamp, len(ts))
	for i, t := range ts {
		if validity != nil && !validity[i] {
			continue
		}
		sec, frac := t.Unix(), int64(t.Nanosecond())/nsPerUnit
		if sec > math.MaxInt64/perSec || sec < math.MinInt64/perSec ||
			sec*perSec > math.MaxInt64-frac {
			return Series{}, fmt.Errorf("gobi: NewTimestampSeriesUnit: row %d (%s) overflows Timestamp[%s]",
				i, t.Format(time.RFC3339Nano), unit)
		}
		vals[i] = arrow.Timestamp(sec*perSec + frac)
	}
	b := array.NewTimestampBuilder(memory.DefaultAllocator, &arrow.TimestampType{Unit: unit, TimeZone: "UTC"})
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray()), nil
}

func checkValidityLen(fn string, n int, validity []bool) {
	if validity != nil && len(validity) != n {
		panic(fmt.Sprintf("gobi: %s: validity has %d entries, values have %d", fn, len(validity), n))
	}
}

// NewFrameFromSeries builds a Frame whose columns are series, in
// order, each keeping its own field (type, nullability, geometry
// tags). The Frame takes its own reference on every column, so the
// caller's Series stay valid; Release the Frame when done.
//
// Every Series must have the same length (ErrColumnLenMismatch), and
// names must be unique (ErrDuplicateColumn).
func NewFrameFromSeries(series ...Series) (*Frame, error) {
	fields := make([]arrow.Field, len(series))
	seen := make(map[string]bool, len(series))
	for i, s := range series {
		if s.col == nil {
			return nil, fmt.Errorf("gobi: NewFrameFromSeries: series %d is a zero Series with no column", i)
		}
		if seen[s.name] {
			return nil, fmt.Errorf("%w: NewFrameFromSeries: %q", ErrDuplicateColumn, s.name)
		}
		seen[s.name] = true
		if s.Len() != series[0].Len() {
			return nil, fmt.Errorf("%w: series %q has %d rows, expected %d",
				ErrColumnLenMismatch, s.name, s.Len(), series[0].Len())
		}
		fields[i] = s.field
		fields[i].Name = s.name
	}
	out := make([]Series, len(series))
	for i, s := range series {
		out[i] = NewSeries(arrow.NewColumn(fields[i], s.col.Data()))
	}
	return &Frame{schema: arrow.NewSchema(fields, nil), series: out}, nil
}

// Schema metadata keys used by gobi to tag geometry columns. This follows the
// GeoParquet convention closely enough that files written by gobi should be
// readable by other GeoParquet-aware tools for primitive geometry types.
const (
	MetaGeometryType = "gobi:geometry_type"
	MetaGeometryCRS  = "gobi:crs_epsg"
)

// isGeometryField reports whether f is tagged as a geometry column.
func isGeometryField(f arrow.Field) bool {
	if f.Type.ID() != arrow.BINARY {
		return false
	}
	_, ok := f.Metadata.GetValue(MetaGeometryType)
	return ok
}

// GeometryField returns a schema field tagged as a WKB geometry column.
// Pass epsg=0 to leave the CRS unset.
func GeometryField(name string, epsg int32) arrow.Field {
	md := arrow.NewMetadata(
		[]string{MetaGeometryType, MetaGeometryCRS},
		[]string{"WKB", strconv.FormatInt(int64(epsg), 10)},
	)
	return arrow.Field{
		Name:     name,
		Type:     arrow.BinaryTypes.Binary,
		Nullable: true,
		Metadata: md,
	}
}

// geometryCRSFromField reads the EPSG code stored in a geometry field's
// schema metadata. Returns 0 if unset or unparseable.
func geometryCRSFromField(f arrow.Field) int32 {
	s, ok := f.Metadata.GetValue(MetaGeometryCRS)
	if !ok || s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0
	}
	return int32(v)
}

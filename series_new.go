package gobi

import (
	"fmt"
	"math"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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

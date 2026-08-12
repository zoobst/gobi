package gobi

import (
	"fmt"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Additional datetime Series methods that complement the extractors
// in series_time.go: sub-second nanoseconds, calendar / clock
// truncation, and string formatting. Same null-propagating,
// Timezone-respecting contract as the existing Year/Month/...
// extractors.

// Nanosecond returns each row's sub-second nanosecond component
// [0, 999_999_999] as Int64. Distinct from the raw underlying
// Timestamp value: this pulls the fractional-second part after
// converting to time.Time in the column's declared timezone.
func (s Series) Nanosecond() (Series, error) {
	return s.timeExtract("nanosecond", func(t time.Time) int64 { return int64(t.Nanosecond()) })
}

// DateTruncate returns a Timestamp Series with each non-null value
// truncated down to the start of the named unit. Supported units:
// "year", "month", "day", "hour", "minute", "second". Calendar-
// aware for year/month/day (start-of-year at 00:00:00, etc.);
// wall-clock aligned for hour/minute/second.
//
// The output preserves the source column's TimeUnit and TimeZone —
// truncation happens in the value's local timezone, so
// DateTruncate("day") on a US/Eastern-tagged timestamp gives
// local-midnight, not UTC-midnight.
func (s Series) DateTruncate(unit string) (Series, error) {
	fn, err := timeTruncateFuncFor(unit)
	if err != nil {
		return Series{}, err
	}
	return s.timeMapTimestamp(fn, "trunc_"+unit)
}

// DateFormat returns a String Series with each non-null value
// formatted using the given Go time layout (see time.Layout /
// time.RFC3339 constants). Empty layout defaults to RFC3339.
// Formatting uses the column's declared timezone.
func (s Series) DateFormat(layout string) (Series, error) {
	if layout == "" {
		layout = time.RFC3339
	}
	return s.timeMapString(func(t time.Time) string { return t.Format(layout) }, "format")
}

// timeMapTimestamp is the shared Timestamp→Timestamp driver for
// DateTruncate + any future same-type transforms. Preserves the
// source column's TimeUnit / TimeZone on the output.
func (s Series) timeMapTimestamp(fn func(time.Time) time.Time, suffix string) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	tsType, ok := s.DataType().(*arrow.TimestampType)
	if !ok {
		return Series{}, fmt.Errorf("%w: DateTruncate/Format require *arrow.TimestampType, got %s",
			ErrColumnTypeMismatch, s.DataType())
	}
	pool := memory.DefaultAllocator
	b := array.NewTimestampBuilder(pool, tsType)
	defer b.Release()
	n := s.Len()
	for i := range n {
		t, valid, err := s.TimeAt(i)
		if err != nil {
			return Series{}, err
		}
		if !valid {
			b.AppendNull()
			continue
		}
		out := fn(t)
		b.Append(arrow.Timestamp(out.UnixNano() / unitToNanos(tsType.Unit)))
	}
	return arrayToSeries(pool, s.name+"_"+suffix, tsType, b.NewArray())
}

// timeMapString is the shared Timestamp→String driver for
// DateFormat and any future formatting ops.
func (s Series) timeMapString(fn func(time.Time) string, suffix string) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	pool := memory.DefaultAllocator
	b := array.NewStringBuilder(pool)
	defer b.Release()
	n := s.Len()
	for i := range n {
		t, valid, err := s.TimeAt(i)
		if err != nil {
			return Series{}, err
		}
		if !valid {
			b.AppendNull()
			continue
		}
		b.Append(fn(t))
	}
	return arrayToSeries(pool, s.name+"_"+suffix, arrow.BinaryTypes.String, b.NewArray())
}

// timeTruncateFuncFor returns the time.Time → time.Time truncation
// function for a unit name. Calendar-aware for year/month/day
// (start-of-year etc. at 00:00:00 in the value's timezone); wall-
// clock aligned for smaller units.
func timeTruncateFuncFor(unit string) (func(time.Time) time.Time, error) {
	switch unit {
	case "year":
		return func(t time.Time) time.Time {
			return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
		}, nil
	case "month":
		return func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		}, nil
	case "day":
		return func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		}, nil
	case "hour":
		return func(t time.Time) time.Time { return t.Truncate(time.Hour) }, nil
	case "minute":
		return func(t time.Time) time.Time { return t.Truncate(time.Minute) }, nil
	case "second":
		return func(t time.Time) time.Time { return t.Truncate(time.Second) }, nil
	}
	return nil, fmt.Errorf("gobi: DateTruncate: unknown unit %q (want year|month|day|hour|minute|second)", unit)
}

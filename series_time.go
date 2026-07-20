package gobi

import (
	"fmt"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// -----------------------------------------------------------------------------
// Datetime type
//
// gobi's canonical datetime storage is Arrow Timestamp[ns], optionally with
// a timezone label (IANA tz string like "America/New_York" or "UTC"). Under
// the hood every Timestamp value is a Unix nanosecond count — an absolute
// instant. The timezone tag only changes how the instant is rendered in
// wall-clock terms (year/month/hour extraction, TruncateToCalendar).
//
// Reading a Parquet file (or building a Series manually) at other
// precisions (s/ms/us/ns, Date32, Date64) is supported: the fast-path
// helpers convert to nanoseconds internally so all downstream ops see a
// single representation.
//
// Use WithTimezone to attach or change the tz label without shifting the
// underlying instants. If a Series is tz-naive (empty tz label) every op
// behaves as UTC.
// -----------------------------------------------------------------------------

const (
	UnitNanosecond TimeUnit = iota
	UnitMicrosecond
	UnitMillisecond
	UnitSecond
	UnitMinute
	UnitHour
	UnitDay
)

const (
	CalendarWeek  CalendarUnit = iota // ISO week: truncate to the previous Monday 00:00
	CalendarMonth                     // truncate to the first day of the month at 00:00
	CalendarYear                      // truncate to Jan 1 at 00:00
)

// ErrNotDateTime is returned when a time-only op is attempted on a Series
// that is not a Timestamp / Date32 / Date64 column.
var ErrNotDateTime = fmt.Errorf("gobi: series is not a datetime column")

// TimeUnit selects a truncation granularity for Series.TruncateTo. Values
// intentionally include only sub-day units; week / month / year truncations
// use TruncateToCalendar because they require calendar (not clock)
// arithmetic.
type TimeUnit uint8

// CalendarUnit selects a calendar-aware truncation granularity for
// Series.TruncateToCalendar.
type CalendarUnit uint8

// -----------------------------------------------------------------------------
// Fast-path timestamp view
//
// Component extractors and arithmetic operate on the fast path when the
// series is a single-chunk column. Multi-chunk / mixed-precision columns
// fall back to per-row TimeAt lookups.
// -----------------------------------------------------------------------------

type tsView struct {
	arr  arrow.Array
	kind int // 1=Timestamp, 2=Date32, 3=Date64
	// tsVals / tsUnit are populated when kind==1
	tsVals []arrow.Timestamp
	tsUnit arrow.TimeUnit
	// d32Vals / d64Vals are populated for kind==2/3
	d32Vals []arrow.Date32
	d64Vals []arrow.Date64
	// loc is the location component extractors and truncation should
	// render values in. Nil is treated as UTC.
	loc *time.Location
}

// at returns the row at index i as a time.Time (in this view's location)
// plus validity. Only valid after viewTimestamp returned ok=true.
func (v tsView) at(i int) (time.Time, bool) {
	if v.arr.IsNull(i) {
		return time.Time{}, false
	}
	loc := v.loc
	if loc == nil {
		loc = time.UTC
	}
	switch v.kind {
	case 1:
		return arrowTimestampToTime(v.tsVals[i], v.tsUnit, loc), true
	case 2:
		return time.Unix(int64(v.d32Vals[i])*86400, 0).In(loc), true
	case 3:
		return time.UnixMilli(int64(v.d64Vals[i])).In(loc), true
	}
	return time.Time{}, false
}

// -----------------------------------------------------------------------------
// Type detection
// -----------------------------------------------------------------------------

// IsDateTime reports whether s is a Timestamp / Date32 / Date64 column.
func (s Series) IsDateTime() bool {
	if s.col == nil {
		return false
	}
	switch s.DataType().ID() {
	case arrow.TIMESTAMP, arrow.DATE32, arrow.DATE64:
		return true
	}
	return false
}

// Timezone returns the IANA timezone label on s (e.g. "America/New_York"),
// or "" if the series is tz-naive or not a Timestamp column.
func (s Series) Timezone() string {
	if !s.IsDateTime() {
		return ""
	}
	if dt, ok := s.DataType().(*arrow.TimestampType); ok {
		return dt.TimeZone
	}
	return ""
}

// timeLocation returns the *time.Location a Timestamp series should render
// its values in. Falls back to UTC when the tz label is empty or names an
// unloadable zone. Non-Timestamp series (Date32/Date64) always return UTC.
func timeLocation(s Series) *time.Location {
	if !s.IsDateTime() {
		return time.UTC
	}
	dt, ok := s.DataType().(*arrow.TimestampType)
	if !ok || dt.TimeZone == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(dt.TimeZone); err == nil {
		return loc
	}
	return time.UTC
}

// WithTimezone returns a copy of s that carries the given IANA timezone
// label. The underlying Unix-nanosecond values are NOT shifted — only the
// display timezone changes. This matches pandas' `tz_convert` when the
// input is already tz-aware, or "assume the values are absolute instants
// and now render them in tz" when the input is naive.
//
// Passing tz == "" strips any existing label (result is tz-naive). Passing
// a name that time.LoadLocation cannot resolve returns an error.
func (s Series) WithTimezone(tz string) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return Series{}, fmt.Errorf("gobi: unknown timezone %q: %w", tz, err)
		}
	}
	// We only support timestamp series here; Date32/Date64 don't carry tz.
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return Series{}, fmt.Errorf("gobi: WithTimezone requires a single-chunk Timestamp series")
	}
	tsChunk, ok := chunks[0].(*array.Timestamp)
	if !ok {
		return Series{}, fmt.Errorf("gobi: WithTimezone only applies to Timestamp columns, not %s", s.DataType())
	}
	oldType := tsChunk.DataType().(*arrow.TimestampType)
	newType := &arrow.TimestampType{Unit: oldType.Unit, TimeZone: tz}
	// Reuse the underlying values by building a fresh Timestamp array with
	// the new type — cheap, no allocation of the value buffer.
	pool := memory.DefaultAllocator
	b := array.NewTimestampBuilder(pool, newType)
	defer b.Release()
	vals := tsChunk.TimestampValues()
	validity := make([]bool, tsChunk.Len())
	for i := 0; i < tsChunk.Len(); i++ {
		validity[i] = !tsChunk.IsNull(i)
	}
	b.AppendValues(vals, validity)
	return newSeriesFromArray(s.name, b.NewArray()), nil
}

// TimeAt returns the value at row i as a time.Time, plus a validity flag
// (false for null). The returned time.Time carries the series' timezone
// (UTC if the series is tz-naive). Errors for non-datetime series.
func (s Series) TimeAt(i int) (time.Time, bool, error) {
	if !s.IsDateTime() {
		return time.Time{}, false, ErrNotDateTime
	}
	if i < 0 || i >= s.Len() {
		return time.Time{}, false, fmt.Errorf("%w: %d not in [0,%d)", ErrRowOutOfRange, i, s.Len())
	}
	loc := timeLocation(s)
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if i < offset+chunk.Len() {
			local := i - offset
			if chunk.IsNull(local) {
				return time.Time{}, false, nil
			}
			return arrowTimeAt(chunk, local, loc), true, nil
		}
		offset += chunk.Len()
	}
	return time.Time{}, false, fmt.Errorf("%w: index %d unreachable", ErrRowOutOfRange, i)
}

// arrowTimeAt converts one row of an Arrow date/time chunk into a
// time.Time in the given location. Date32/Date64 columns are always UTC
// (they have no unit / no tz).
func arrowTimeAt(chunk arrow.Array, local int, loc *time.Location) time.Time {
	switch a := chunk.(type) {
	case *array.Timestamp:
		return arrowTimestampToTime(a.Value(local), a.DataType().(*arrow.TimestampType).Unit, loc)
	case *array.Date32:
		// Date32 = days since 1970-01-01
		return time.Unix(int64(a.Value(local))*86400, 0).UTC()
	case *array.Date64:
		// Date64 = ms since 1970-01-01
		return time.UnixMilli(int64(a.Value(local))).UTC()
	}
	return time.Time{}
}

func arrowTimestampToTime(v arrow.Timestamp, unit arrow.TimeUnit, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	switch unit {
	case arrow.Second:
		return time.Unix(int64(v), 0).In(loc)
	case arrow.Millisecond:
		return time.UnixMilli(int64(v)).In(loc)
	case arrow.Microsecond:
		return time.UnixMicro(int64(v)).In(loc)
	case arrow.Nanosecond:
		return time.Unix(0, int64(v)).In(loc)
	}
	return time.Time{}
}

// timeToNanos converts a Go time.Time to Arrow Timestamp[ns].
func timeToNanos(t time.Time) arrow.Timestamp {
	return arrow.Timestamp(t.UnixNano())
}

func viewTimestamp(s Series) (tsView, bool) {
	if !s.IsDateTime() {
		return tsView{}, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return tsView{}, false
	}
	loc := timeLocation(s)
	switch a := chunks[0].(type) {
	case *array.Timestamp:
		return tsView{arr: a, kind: 1, tsVals: a.TimestampValues(), tsUnit: a.DataType().(*arrow.TimestampType).Unit, loc: loc}, true
	case *array.Date32:
		return tsView{arr: a, kind: 2, d32Vals: a.Date32Values(), loc: loc}, true
	case *array.Date64:
		return tsView{arr: a, kind: 3, d64Vals: a.Date64Values(), loc: loc}, true
	}
	return tsView{}, false
}

// -----------------------------------------------------------------------------
// Component extractors
//
// All extractors produce an Int64 series. Nulls in the input propagate to
// nulls in the output. Non-datetime input series return ErrNotDateTime.
// -----------------------------------------------------------------------------

// Year returns each row's calendar year as an Int64 Series (UTC).
func (s Series) Year() (Series, error) {
	return s.timeExtract("year", func(t time.Time) int64 { return int64(t.Year()) })
}

// Month returns each row's calendar month [1..12] as Int64.
func (s Series) Month() (Series, error) {
	return s.timeExtract("month", func(t time.Time) int64 { return int64(t.Month()) })
}

// Day returns each row's day-of-month [1..31] as Int64.
func (s Series) Day() (Series, error) {
	return s.timeExtract("day", func(t time.Time) int64 { return int64(t.Day()) })
}

// Hour returns each row's hour-of-day [0..23] as Int64 (UTC).
func (s Series) Hour() (Series, error) {
	return s.timeExtract("hour", func(t time.Time) int64 { return int64(t.Hour()) })
}

// Minute returns each row's minute-of-hour [0..59] as Int64.
func (s Series) Minute() (Series, error) {
	return s.timeExtract("minute", func(t time.Time) int64 { return int64(t.Minute()) })
}

// Second returns each row's second-of-minute [0..59] as Int64.
func (s Series) Second() (Series, error) {
	return s.timeExtract("second", func(t time.Time) int64 { return int64(t.Second()) })
}

// Weekday returns each row's day-of-week as Int64 with Sunday=0..Saturday=6
// (matches Go's time.Weekday int values).
func (s Series) Weekday() (Series, error) {
	return s.timeExtract("weekday", func(t time.Time) int64 { return int64(t.Weekday()) })
}

// DayOfYear returns each row's day-of-year [1..366] as Int64.
func (s Series) DayOfYear() (Series, error) {
	return s.timeExtract("day_of_year", func(t time.Time) int64 { return int64(t.YearDay()) })
}

// timeExtract is the shared driver for component extractors.
func (s Series) timeExtract(suffix string, fn func(time.Time) int64) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	n := s.Len()
	out := make([]int64, n)
	validity := make([]bool, n)
	if v, ok := viewTimestamp(s); ok {
		for i := range n {
			t, valid := v.at(i)
			if !valid {
				continue
			}
			out[i] = fn(t)
			validity[i] = true
		}
	} else {
		for i := range n {
			t, valid, err := s.TimeAt(i)
			if err != nil {
				return Series{}, err
			}
			if !valid {
				continue
			}
			out[i] = fn(t)
			validity[i] = true
		}
	}
	return buildInt64Series(s.name+"_"+suffix, out, validity), nil
}

// -----------------------------------------------------------------------------
// Duration arithmetic
// -----------------------------------------------------------------------------

// AddDuration returns a new Timestamp[ns] Series with d added to each row.
// Nulls propagate.
func (s Series) AddDuration(d time.Duration) (Series, error) {
	return s.shiftDuration(d, s.name)
}

// SubDuration returns a new Timestamp[ns] Series with d subtracted from
// each row.
func (s Series) SubDuration(d time.Duration) (Series, error) {
	return s.shiftDuration(-d, s.name)
}

func (s Series) shiftDuration(d time.Duration, outName string) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	n := s.Len()
	nanos := int64(d)
	outVals := make([]arrow.Timestamp, n)
	validity := make([]bool, n)
	if v, ok := viewTimestamp(s); ok {
		for i := range n {
			t, valid := v.at(i)
			if !valid {
				continue
			}
			outVals[i] = timeToNanos(t.Add(d))
			validity[i] = true
		}
	} else {
		for i := range n {
			t, valid, err := s.TimeAt(i)
			if err != nil {
				return Series{}, err
			}
			if !valid {
				continue
			}
			outVals[i] = timeToNanos(t.Add(time.Duration(nanos)))
			validity[i] = true
		}
	}
	return buildTimestampNsSeries(outName, outVals, validity), nil
}

// DiffDuration returns an Int64 Series whose row i is
// (s[i] - other[i]) expressed in nanoseconds. Nulls on either side
// produce nulls in the output.
func (s Series) DiffDuration(other Series) (Series, error) {
	if !s.IsDateTime() || !other.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	if s.Len() != other.Len() {
		return Series{}, fmt.Errorf("%w: %d vs %d", ErrColumnLenMismatch, s.Len(), other.Len())
	}
	n := s.Len()
	out := make([]int64, n)
	validity := make([]bool, n)
	for i := range n {
		a, aok, err := s.TimeAt(i)
		if err != nil {
			return Series{}, err
		}
		b, bok, err := other.TimeAt(i)
		if err != nil {
			return Series{}, err
		}
		if !aok || !bok {
			continue
		}
		out[i] = a.Sub(b).Nanoseconds()
		validity[i] = true
	}
	return buildInt64Series(s.name+"_diff", out, validity), nil
}

// -----------------------------------------------------------------------------
// Time comparisons
//
// Datetime series are not routed through the numeric comparison path
// because their underlying Arrow storage is not necessarily int64
// nanoseconds (Date32 is int32 days, Timestamp[us] is int64 microseconds,
// etc.). Explicit *Time methods keep the semantics predictable.
// -----------------------------------------------------------------------------

// EqTime returns a Boolean Series true where s[i] == t.
func (s Series) EqTime(t time.Time) (Series, error) {
	return s.timeCmp(t, cmpEq)
}

// NeTime returns a Boolean Series true where s[i] != t.
func (s Series) NeTime(t time.Time) (Series, error) {
	return s.timeCmp(t, cmpNe)
}

// LtTime returns a Boolean Series true where s[i] < t.
func (s Series) LtTime(t time.Time) (Series, error) {
	return s.timeCmp(t, cmpLt)
}

// LeTime returns a Boolean Series true where s[i] <= t.
func (s Series) LeTime(t time.Time) (Series, error) {
	return s.timeCmp(t, cmpLe)
}

// GtTime returns a Boolean Series true where s[i] > t.
func (s Series) GtTime(t time.Time) (Series, error) {
	return s.timeCmp(t, cmpGt)
}

// GeTime returns a Boolean Series true where s[i] >= t.
func (s Series) GeTime(t time.Time) (Series, error) {
	return s.timeCmp(t, cmpGe)
}

func (s Series) timeCmp(target time.Time, op cmpOp) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	n := s.Len()
	out := make([]bool, n)
	validity := make([]bool, n)
	tgtNs := target.UnixNano()
	if v, ok := viewTimestamp(s); ok {
		for i := range n {
			t, valid := v.at(i)
			if !valid {
				continue
			}
			out[i] = compareInt64(t.UnixNano(), tgtNs, op)
			validity[i] = true
		}
	} else {
		for i := range n {
			t, valid, err := s.TimeAt(i)
			if err != nil {
				return Series{}, err
			}
			if !valid {
				continue
			}
			out[i] = compareInt64(t.UnixNano(), tgtNs, op)
			validity[i] = true
		}
	}
	return buildBoolSeries(s.name, out, validity), nil
}

func compareInt64(a, b int64, op cmpOp) bool {
	switch op {
	case cmpEq:
		return a == b
	case cmpNe:
		return a != b
	case cmpLt:
		return a < b
	case cmpLe:
		return a <= b
	case cmpGt:
		return a > b
	case cmpGe:
		return a >= b
	}
	return false
}

// -----------------------------------------------------------------------------
// Truncate
// -----------------------------------------------------------------------------

// TruncateTo returns a new Timestamp[ns] Series with each row rounded down
// to the nearest boundary of the given unit. UnitDay truncates to
// 00:00:00 UTC of the same date.
func (s Series) TruncateTo(unit TimeUnit) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	step, err := unitDuration(unit)
	if err != nil {
		return Series{}, err
	}
	n := s.Len()
	outVals := make([]arrow.Timestamp, n)
	validity := make([]bool, n)
	stepNs := int64(step)
	if v, ok := viewTimestamp(s); ok {
		for i := range n {
			t, valid := v.at(i)
			if !valid {
				continue
			}
			ns := t.UnixNano()
			floor := (ns / stepNs) * stepNs
			// integer truncation toward zero can leave floor > ns for
			// negative timestamps; correct that by subtracting one step.
			if floor > ns {
				floor -= stepNs
			}
			outVals[i] = arrow.Timestamp(floor)
			validity[i] = true
		}
	} else {
		for i := range n {
			t, valid, err := s.TimeAt(i)
			if err != nil {
				return Series{}, err
			}
			if !valid {
				continue
			}
			ns := t.UnixNano()
			floor := (ns / stepNs) * stepNs
			if floor > ns {
				floor -= stepNs
			}
			outVals[i] = arrow.Timestamp(floor)
			validity[i] = true
		}
	}
	return buildTimestampNsSeries(s.name, outVals, validity), nil
}

// TruncateToCalendar truncates each row to a calendar boundary (week,
// month, year). If s carries a timezone, the boundary is computed in that
// timezone (so "start of month" is midnight local time in the tz, not
// UTC). Week uses ISO semantics: previous Monday at 00:00.
func (s Series) TruncateToCalendar(unit CalendarUnit) (Series, error) {
	if !s.IsDateTime() {
		return Series{}, ErrNotDateTime
	}
	loc := timeLocation(s)
	n := s.Len()
	outVals := make([]arrow.Timestamp, n)
	validity := make([]bool, n)
	trunc := func(t time.Time) time.Time {
		// Interpret the row in the series' tz for boundary computation,
		// then convert back to the same absolute instant (Add's math is
		// preserved via time.Time.UnixNano).
		local := t.In(loc)
		switch unit {
		case CalendarWeek:
			// Go's Weekday: Sunday=0..Saturday=6. Shift so Monday=0.
			shift := (int(local.Weekday()) + 6) % 7
			d := local.AddDate(0, 0, -shift)
			return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
		case CalendarMonth:
			return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
		case CalendarYear:
			return time.Date(local.Year(), 1, 1, 0, 0, 0, 0, loc)
		}
		return t
	}
	if v, ok := viewTimestamp(s); ok {
		for i := range n {
			t, valid := v.at(i)
			if !valid {
				continue
			}
			outVals[i] = timeToNanos(trunc(t))
			validity[i] = true
		}
	} else {
		for i := range n {
			t, valid, err := s.TimeAt(i)
			if err != nil {
				return Series{}, err
			}
			if !valid {
				continue
			}
			outVals[i] = timeToNanos(trunc(t))
			validity[i] = true
		}
	}
	return buildTimestampNsSeries(s.name, outVals, validity), nil
}

func unitDuration(u TimeUnit) (time.Duration, error) {
	switch u {
	case UnitNanosecond:
		return time.Nanosecond, nil
	case UnitMicrosecond:
		return time.Microsecond, nil
	case UnitMillisecond:
		return time.Millisecond, nil
	case UnitSecond:
		return time.Second, nil
	case UnitMinute:
		return time.Minute, nil
	case UnitHour:
		return time.Hour, nil
	case UnitDay:
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("gobi: unknown TimeUnit %d", u)
}

// -----------------------------------------------------------------------------
// Series builders
// -----------------------------------------------------------------------------

// buildTimestampNsSeries wraps a []arrow.Timestamp with the given
// validity into a Timestamp[ns] Series. A nil validity means all valid.
func buildTimestampNsSeries(name string, vals []arrow.Timestamp, validity []bool) Series {
	pool := memory.DefaultAllocator
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond}
	b := array.NewTimestampBuilder(pool, tsType)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

// NewTimestampSeries builds a Timestamp[ns] Series from the given
// time.Time values. Passing a nil validity slice means every row is valid;
// otherwise validity[i]==false marks row i as null (its value is ignored).
func NewTimestampSeries(name string, ts []time.Time, validity []bool) Series {
	vals := make([]arrow.Timestamp, len(ts))
	for i, t := range ts {
		if validity == nil || validity[i] {
			vals[i] = timeToNanos(t)
		}
	}
	return buildTimestampNsSeries(name, vals, validity)
}

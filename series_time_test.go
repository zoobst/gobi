package gobi

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func makeTimeSeries(t *testing.T) Series {
	t.Helper()
	return NewTimestampSeries("when", []time.Time{
		time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC),
		time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
	}, nil)
}

func TestSeries_IsDateTime_And_TimeAt(t *testing.T) {
	s := makeTimeSeries(t)
	if !s.IsDateTime() {
		t.Fatal("Timestamp series should be recognized as datetime")
	}
	got, ok, err := s.TimeAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("row 1 unexpectedly null")
	}
	want := time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("row 1 = %v, want %v", got, want)
	}

	// TimeAt on a non-datetime series errors cleanly.
	iSer := intSeries("i", []int64{1, 2}, nil)
	if _, _, err := iSer.TimeAt(0); !errors.Is(err, ErrNotDateTime) {
		t.Fatalf("want ErrNotDateTime, got %v", err)
	}
}

func TestSeries_ComponentExtractors(t *testing.T) {
	s := makeTimeSeries(t)
	yr, err := s.Year()
	if err != nil {
		t.Fatal(err)
	}
	yrArr := yr.col.Data().Chunks()[0].(*array.Int64)
	if yrArr.Value(0) != 2026 || yrArr.Value(2) != 2026 {
		t.Fatalf("year values: %v", []int64{yrArr.Value(0), yrArr.Value(1), yrArr.Value(2)})
	}

	mo, _ := s.Month()
	moArr := mo.col.Data().Chunks()[0].(*array.Int64)
	if moArr.Value(0) != 1 || moArr.Value(1) != 3 || moArr.Value(2) != 7 {
		t.Fatalf("month values: %v", []int64{moArr.Value(0), moArr.Value(1), moArr.Value(2)})
	}

	hr, _ := s.Hour()
	hrArr := hr.col.Data().Chunks()[0].(*array.Int64)
	if hrArr.Value(0) != 9 || hrArr.Value(1) != 14 || hrArr.Value(2) != 0 {
		t.Fatalf("hour values: %v", []int64{hrArr.Value(0), hrArr.Value(1), hrArr.Value(2)})
	}

	wd, _ := s.Weekday()
	wdArr := wd.col.Data().Chunks()[0].(*array.Int64)
	// 2026-01-15 is a Thursday → 4; 2026-03-22 is a Sunday → 0; 2026-07-04 is a Saturday → 6.
	if wdArr.Value(0) != 4 || wdArr.Value(1) != 0 || wdArr.Value(2) != 6 {
		t.Fatalf("weekday values: %v", []int64{wdArr.Value(0), wdArr.Value(1), wdArr.Value(2)})
	}
}

func TestSeries_NullsPropagateThroughExtractors(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		{},
	}, []bool{true, false})
	yr, err := s.Year()
	if err != nil {
		t.Fatal(err)
	}
	arr := yr.col.Data().Chunks()[0].(*array.Int64)
	if arr.IsNull(0) {
		t.Fatal("row 0 should be non-null")
	}
	if !arr.IsNull(1) {
		t.Fatal("row 1 should be null")
	}
	if arr.Value(0) != 2026 {
		t.Fatalf("year = %d, want 2026", arr.Value(0))
	}
}

func TestSeries_AddSubDuration(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC),
	}, nil)
	plusHour, err := s.AddDuration(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := plusHour.TimeAt(0)
	want := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("+1h = %v, want %v", got, want)
	}
	minusDay, _ := s.SubDuration(24 * time.Hour)
	got, _, _ = minusDay.TimeAt(0)
	want = time.Date(2026, 1, 14, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("-1d = %v, want %v", got, want)
	}
}

func TestSeries_DiffDuration(t *testing.T) {
	a := NewTimestampSeries("a", []time.Time{
		time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
	}, nil)
	b := NewTimestampSeries("b", []time.Time{
		time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC),
	}, nil)
	diff, err := a.DiffDuration(b)
	if err != nil {
		t.Fatal(err)
	}
	arr := diff.col.Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != int64(time.Hour) {
		t.Fatalf("diff ns = %d, want %d", arr.Value(0), int64(time.Hour))
	}
}

func TestSeries_TimeComparisons(t *testing.T) {
	s := makeTimeSeries(t)
	cutoff := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mask, err := s.LtTime(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	arr := mask.col.Data().Chunks()[0].(*array.Boolean)
	if !arr.Value(0) || !arr.Value(1) || arr.Value(2) {
		t.Fatalf("LtTime: %v %v %v", arr.Value(0), arr.Value(1), arr.Value(2))
	}

	// Round-trip via GtTime + Eq.
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	past, _ := s.GtTime(future)
	pastArr := past.col.Data().Chunks()[0].(*array.Boolean)
	for i := 0; i < 3; i++ {
		if pastArr.Value(i) {
			t.Fatalf("row %d should not be after year 2100", i)
		}
	}
}

func TestSeries_TruncateTo(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 1, 15, 9, 37, 42, 123456789, time.UTC),
	}, nil)
	trHour, err := s.TruncateTo(UnitHour)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := trHour.TimeAt(0)
	want := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("TruncateTo(UnitHour) = %v, want %v", got, want)
	}
	trDay, _ := s.TruncateTo(UnitDay)
	got, _, _ = trDay.TimeAt(0)
	want = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("TruncateTo(UnitDay) = %v, want %v", got, want)
	}
}

func TestSeries_TruncateToCalendar(t *testing.T) {
	// 2026-07-22 is a Wednesday; truncate to week → 2026-07-20 (Monday).
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 22, 14, 30, 0, 0, time.UTC),
	}, nil)
	wk, err := s.TruncateToCalendar(CalendarWeek)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := wk.TimeAt(0)
	want := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("week truncate = %v, want %v", got, want)
	}
	mo, _ := s.TruncateToCalendar(CalendarMonth)
	got, _, _ = mo.TimeAt(0)
	want = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("month truncate = %v, want %v", got, want)
	}
	yr, _ := s.TruncateToCalendar(CalendarYear)
	got, _, _ = yr.TimeAt(0)
	want = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("year truncate = %v, want %v", got, want)
	}
}

func TestSeries_NonDateTimeErrors(t *testing.T) {
	iSer := intSeries("i", []int64{1, 2}, nil)
	if _, err := iSer.Year(); !errors.Is(err, ErrNotDateTime) {
		t.Fatalf("Year on int: want ErrNotDateTime, got %v", err)
	}
	if _, err := iSer.AddDuration(time.Second); !errors.Is(err, ErrNotDateTime) {
		t.Fatalf("AddDuration on int: want ErrNotDateTime, got %v", err)
	}
	if _, err := iSer.TruncateTo(UnitHour); !errors.Is(err, ErrNotDateTime) {
		t.Fatalf("TruncateTo on int: want ErrNotDateTime, got %v", err)
	}
}

// tzOrSkip loads a location or skips the test — some minimal Go
// distributions don't ship the tzdata; on those we want a clean skip
// rather than a test failure.
func tzOrSkip(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tz %q not available on this system: %v", name, err)
	}
	return loc
}

func TestTimezone_LabelAndRoundTrip(t *testing.T) {
	tzOrSkip(t, "America/New_York")
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	}, nil)
	if s.Timezone() != "" {
		t.Fatalf("fresh series should be tz-naive, got %q", s.Timezone())
	}

	ny, err := s.WithTimezone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if got := ny.Timezone(); got != "America/New_York" {
		t.Fatalf("tz = %q, want America/New_York", got)
	}
	// The underlying instant should be unchanged — only the display tz.
	orig, _, _ := s.TimeAt(0)
	shifted, _, _ := ny.TimeAt(0)
	if !orig.Equal(shifted) {
		t.Fatalf("underlying instant changed: %v vs %v", orig, shifted)
	}
	// Stripping the tz should give back a naive series.
	back, err := ny.WithTimezone("")
	if err != nil {
		t.Fatal(err)
	}
	if back.Timezone() != "" {
		t.Fatalf("stripped tz = %q, want empty", back.Timezone())
	}
}

func TestTimezone_ComponentExtractorsInLocal(t *testing.T) {
	tzOrSkip(t, "America/New_York")
	// 2026-07-20 08:00 UTC == 2026-07-20 04:00 EDT. Hour extractor should
	// return 4 under America/New_York and 8 under UTC.
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	}, nil)
	utcHour, _ := s.Hour()
	utcArr := utcHour.col.Data().Chunks()[0].(*array.Int64)
	if utcArr.Value(0) != 8 {
		t.Fatalf("UTC hour = %d, want 8", utcArr.Value(0))
	}
	ny, _ := s.WithTimezone("America/New_York")
	nyHour, _ := ny.Hour()
	nyArr := nyHour.col.Data().Chunks()[0].(*array.Int64)
	if nyArr.Value(0) != 4 {
		t.Fatalf("NY hour = %d, want 4 (EDT is UTC-4 in July)", nyArr.Value(0))
	}
}

func TestTimezone_CrossDayBoundary(t *testing.T) {
	tzOrSkip(t, "America/New_York")
	// 2026-07-21 02:00 UTC == 2026-07-20 22:00 EDT. Day extraction should
	// disagree between UTC and NY.
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC),
	}, nil)
	utcDay, _ := s.Day()
	if v := utcDay.col.Data().Chunks()[0].(*array.Int64).Value(0); v != 21 {
		t.Fatalf("UTC day = %d, want 21", v)
	}
	ny, _ := s.WithTimezone("America/New_York")
	nyDay, _ := ny.Day()
	if v := nyDay.col.Data().Chunks()[0].(*array.Int64).Value(0); v != 20 {
		t.Fatalf("NY day = %d, want 20", v)
	}
}

func TestTimezone_TruncateToCalendarHonorsTZ(t *testing.T) {
	tzOrSkip(t, "America/New_York")
	// 2026-07-21 02:00 UTC == 2026-07-20 22:00 EDT.
	// Month truncate in UTC → 2026-07-01 00:00 UTC.
	// Month truncate in NY  → 2026-07-01 00:00 EDT (= 04:00 UTC).
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC),
	}, nil)
	trUTC, _ := s.TruncateToCalendar(CalendarMonth)
	utcT, _, _ := trUTC.TimeAt(0)
	if !utcT.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("UTC month-truncate = %v", utcT)
	}
	ny, _ := s.WithTimezone("America/New_York")
	trNY, _ := ny.TruncateToCalendar(CalendarMonth)
	nyT, _, _ := trNY.TimeAt(0)
	// The instant that is "first-of-month in NY" is 2026-07-01 00:00 EDT
	// which is 2026-07-01 04:00 UTC.
	want := time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC)
	if !nyT.Equal(want) {
		t.Fatalf("NY month-truncate = %v, want %v", nyT, want)
	}
}

func TestWithTimezone_UnknownTzErrors(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{time.Now()}, nil)
	if _, err := s.WithTimezone("Not/A/Zone"); err == nil {
		t.Fatal("expected error for unknown timezone")
	}
}

func TestWithTimezone_RequiresTimestampSeries(t *testing.T) {
	iSer := intSeries("i", []int64{1, 2}, nil)
	if _, err := iSer.WithTimezone("UTC"); err != ErrNotDateTime {
		t.Fatalf("want ErrNotDateTime, got %v", err)
	}
}

func TestSeries_Nanosecond(t *testing.T) {
	s := NewTimestampSeries("when", []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 500_000_000, time.UTC), // 500ms
		time.Date(2026, 1, 1, 0, 0, 0, 123_456_789, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, nil)
	got, err := s.Nanosecond()
	if err != nil {
		t.Fatal(err)
	}
	arr := got.col.Data().Chunks()[0].(*array.Int64)
	want := []int64{500_000_000, 123_456_789, 0}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

func TestSeries_DateTruncate_CalendarUnits(t *testing.T) {
	s := NewTimestampSeries("when", []time.Time{
		time.Date(2026, 3, 22, 14, 30, 45, 123_000_000, time.UTC),
	}, nil)

	cases := []struct {
		unit string
		want time.Time
	}{
		{"year", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"month", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"day", time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC)},
		{"hour", time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC)},
		{"minute", time.Date(2026, 3, 22, 14, 30, 0, 0, time.UTC)},
		{"second", time.Date(2026, 3, 22, 14, 30, 45, 0, time.UTC)},
	}
	for _, c := range cases {
		out, err := s.DateTruncate(c.unit)
		if err != nil {
			t.Fatalf("DateTruncate(%q): %v", c.unit, err)
		}
		got, valid, err := out.TimeAt(0)
		if err != nil || !valid {
			t.Fatalf("TimeAt(0): err=%v valid=%v", err, valid)
		}
		if !got.Equal(c.want) {
			t.Errorf("DateTruncate(%q) = %v, want %v", c.unit, got, c.want)
		}
	}
}

func TestSeries_DateTruncate_UnknownUnitErrors(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{time.Now()}, nil)
	if _, err := s.DateTruncate("week"); err == nil {
		t.Errorf("expected error for unknown unit")
	}
}

func TestSeries_DateFormat(t *testing.T) {
	s := NewTimestampSeries("when", []time.Time{
		time.Date(2026, 3, 22, 14, 30, 45, 0, time.UTC),
	}, nil)
	out, err := s.DateFormat("2006-01-02")
	if err != nil {
		t.Fatal(err)
	}
	arr := out.col.Data().Chunks()[0].(*array.String)
	if got := arr.Value(0); got != "2026-03-22" {
		t.Errorf("DateFormat = %q, want %q", got, "2026-03-22")
	}

	// Empty layout defaults to RFC3339.
	def, err := s.DateFormat("")
	if err != nil {
		t.Fatal(err)
	}
	defArr := def.col.Data().Chunks()[0].(*array.String)
	if got := defArr.Value(0); got != "2026-03-22T14:30:45Z" {
		t.Errorf("DateFormat(\"\") = %q, want RFC3339 %q", got, "2026-03-22T14:30:45Z")
	}
}

func TestSeries_DateTruncate_NullPropagation(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		{},
	}, []bool{true, false})
	out, err := s.DateTruncate("day")
	if err != nil {
		t.Fatal(err)
	}
	arr := out.col.Data().Chunks()[0].(*array.Timestamp)
	if arr.IsNull(0) {
		t.Errorf("row 0 unexpectedly null")
	}
	if !arr.IsNull(1) {
		t.Errorf("row 1 should be null (null input)")
	}
}

func TestSeries_DateTruncate_PreservesMillisecondUnit(t *testing.T) {
	// Build a Millisecond-unit Timestamp directly so preservation
	// actually exercises the rescaling code path (a Nanosecond
	// source through DateTruncate is a trivial no-op).
	s := newTimestampWithType(t, "t", &arrow.TimestampType{Unit: arrow.Millisecond},
		[]time.Time{time.Date(2026, 3, 22, 14, 30, 45, 0, time.UTC)}, nil)
	out, err := s.DateTruncate("day")
	if err != nil {
		t.Fatal(err)
	}
	tsType, ok := out.DataType().(*arrow.TimestampType)
	if !ok {
		t.Fatalf("expected Timestamp output, got %s", out.DataType())
	}
	if tsType.Unit != arrow.Millisecond {
		t.Errorf("output unit = %v, want Millisecond (source unit)", tsType.Unit)
	}
	// Truncation semantics still correct after rescaling.
	got, valid, err := out.TimeAt(0)
	if err != nil || !valid {
		t.Fatalf("TimeAt: err=%v valid=%v", err, valid)
	}
	want := time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("DateTruncate(day) = %v, want %v", got, want)
	}
}

// TestSeries_DateTruncate_TZTaggedGivesLocalMidnight covers the
// documented "midnight in the value's own timezone" behavior:
// truncating a US/Eastern-tagged 2026-03-22T14:00:00 to "day"
// should yield 2026-03-22T00:00:00 EDT (== 04:00 UTC), not
// 2026-03-22T00:00:00 UTC.
func TestSeries_DateTruncate_TZTaggedGivesLocalMidnight(t *testing.T) {
	// Start with UTC noon on 2026-07-04 (mid-day, no DST edge
	// case), then re-tag as America/New_York (which is EDT in
	// July, UTC-4). Values stay the same absolute instant — only
	// the TZ label changes.
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
	}, nil)
	ny, err := s.WithTimezone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	out, err := ny.DateTruncate("day")
	if err != nil {
		t.Fatal(err)
	}
	// Output type must still be TZ-tagged.
	tsType, ok := out.DataType().(*arrow.TimestampType)
	if !ok {
		t.Fatalf("expected Timestamp output, got %s", out.DataType())
	}
	if tsType.TimeZone != "America/New_York" {
		t.Errorf("output timezone = %q, want %q", tsType.TimeZone, "America/New_York")
	}
	// The row-value: 12:00 UTC on 2026-07-04 is 08:00 EDT the same
	// day; midnight EDT is 04:00 UTC (July → EDT is UTC-4).
	got, valid, err := out.TimeAt(0)
	if err != nil || !valid {
		t.Fatalf("TimeAt: err=%v valid=%v", err, valid)
	}
	wantUTC := time.Date(2026, 7, 4, 4, 0, 0, 0, time.UTC)
	if !got.Equal(wantUTC) {
		t.Errorf("TZ-tagged DateTruncate(day) = %v, want %v (local midnight in NY = 04:00 UTC)",
			got.UTC(), wantUTC)
	}
}

// TestSeries_AddDuration_PreservesTypeMetadata proves AddDuration
// keeps the source's TimeUnit + TimeZone on the output. This is
// the concrete regression the code-review fix targets — before
// the fix, Millisecond-unit or TZ-tagged sources came out as
// Nanosecond / UTC-implicit.
func TestSeries_AddDuration_PreservesTypeMetadata(t *testing.T) {
	// Case 1: Millisecond-unit source.
	msSrc := newTimestampWithType(t, "t", &arrow.TimestampType{Unit: arrow.Millisecond},
		[]time.Time{time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC)}, nil)
	msOut, err := msSrc.AddDuration(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	msTS, ok := msOut.DataType().(*arrow.TimestampType)
	if !ok {
		t.Fatalf("AddDuration output type = %s, want Timestamp", msOut.DataType())
	}
	if msTS.Unit != arrow.Millisecond {
		t.Errorf("AddDuration on Millisecond source: output unit = %v, want Millisecond", msTS.Unit)
	}
	// Arithmetic still correct despite the rescaling.
	got, _, _ := msOut.TimeAt(0)
	if want := time.Date(2026, 3, 22, 15, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("AddDuration value = %v, want %v", got, want)
	}

	// Case 2: TZ-tagged source.
	src := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
	}, nil)
	ny, err := src.WithTimezone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	nyOut, err := ny.AddDuration(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nyTS, ok := nyOut.DataType().(*arrow.TimestampType)
	if !ok {
		t.Fatalf("AddDuration output type = %s, want Timestamp", nyOut.DataType())
	}
	if nyTS.TimeZone != "America/New_York" {
		t.Errorf("AddDuration on TZ-tagged source: output tz = %q, want %q",
			nyTS.TimeZone, "America/New_York")
	}
}

// TestSeries_SubDuration_PreservesTypeMetadata is the SubDuration
// mirror of TestSeries_AddDuration_PreservesTypeMetadata — same
// preservation invariant should hold.
func TestSeries_SubDuration_PreservesTypeMetadata(t *testing.T) {
	msSrc := newTimestampWithType(t, "t", &arrow.TimestampType{Unit: arrow.Millisecond},
		[]time.Time{time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC)}, nil)
	out, err := msSrc.SubDuration(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tsType, ok := out.DataType().(*arrow.TimestampType)
	if !ok || tsType.Unit != arrow.Millisecond {
		t.Errorf("SubDuration on Millisecond source: output type = %s, want Timestamp[ms]", out.DataType())
	}
	got, _, _ := out.TimeAt(0)
	if want := time.Date(2026, 3, 22, 13, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("SubDuration value = %v, want %v", got, want)
	}
}

// TestRoundedDivTimestamp_Symmetry: sub-unit rescaling uses
// round-half-away-from-zero, so a +500ns and a -500ns delta on a
// Timestamp[us] source produce +1 μs and -1 μs of rounding
// respectively — same magnitude, opposite sign. Plain integer
// division (Go's `/`) would truncate both toward zero and drop
// the sub-us fraction asymmetrically.
func TestRoundedDivTimestamp_Symmetry(t *testing.T) {
	// div = 1000 corresponds to rescaling ns → μs.
	cases := []struct {
		name string
		v    int64
		want int64
	}{
		{"positive_half_rounds_up", 1_000_500, 1_001},
		{"negative_half_rounds_down", -1_000_500, -1_001},
		{"positive_just_under_half", 1_000_499, 1_000},
		{"negative_just_under_half", -1_000_499, -1_000},
		{"positive_exact_multiple", 1_000_000, 1_000},
		{"negative_exact_multiple", -1_000_000, -1_000},
		{"positive_zero_remainder_after_op", 0, 0},
		{"positive_over_half_rounds_up", 1_000_501, 1_001},
		{"negative_over_half_rounds_down", -1_000_501, -1_001},
	}
	const div arrow.Timestamp = 1000
	for _, c := range cases {
		got := int64(roundedDivTimestamp(arrow.Timestamp(c.v), div))
		if got != c.want {
			t.Errorf("%s: roundedDivTimestamp(%d, %d) = %d, want %d",
				c.name, c.v, div, got, c.want)
		}
	}
}

// TestSeries_AddDuration_SubUnitPrecisionSymmetry: at the Series
// level, adding a +500ns duration to a Timestamp[us] pre-1970
// and post-1970 value produces symmetric rounding (both round
// away from zero, matching the roundedDivTimestamp helper).
//
// Note: what's tested here is that the rounding DIRECTION is
// consistent with the mathematical sign of the input. The
// magnitude of the rounding error is intrinsic to storing sub-us
// precision in a us-precision column — that's expected.
func TestSeries_AddDuration_SubUnitPrecisionSymmetry(t *testing.T) {
	// Pre-1970 and post-1970 anchors, both us-precision:
	// pre1970  = 1969-12-31T23:59:59.500000 UTC = -500_000 μs
	//   (= -500_000_000 ns; +500ns delta = -499_999_500 ns)
	//   Rescaled with round-half-away: -500_000 μs.
	// post1970 = 1970-01-01T00:00:00.500000 UTC = +500_000 μs
	//   (= +500_000_000 ns; +500ns delta = +500_000_500 ns)
	//   Rescaled with round-half-away: +500_001 μs.
	//
	// The test verifies both directions round consistently. With
	// the pre-fix truncate-toward-zero form, pre-1970 would round
	// UP (toward zero) and post-1970 would round DOWN — the
	// asymmetric error direction the reviewer flagged.
	src := newTimestampWithType(t, "t", &arrow.TimestampType{Unit: arrow.Microsecond},
		[]time.Time{
			time.Date(1969, 12, 31, 23, 59, 59, 500_000_000, time.UTC),
			time.Date(1970, 1, 1, 0, 0, 0, 500_000_000, time.UTC),
		}, nil)
	out, err := src.AddDuration(500 * time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	arr := out.col.Data().Chunks()[0].(*array.Timestamp)
	// Values are in microseconds.
	pre := int64(arr.Value(0))
	post := int64(arr.Value(1))

	// Pre-1970 case: source was -500_000 μs. After adding 500ns
	// the true value in ns is -499_999_500 (= -499_999.5 μs);
	// round-half-away-from-zero pushes the magnitude UP, i.e.
	// further from zero → -500_000 μs.
	if pre != -500_000 {
		t.Errorf("pre-1970: got %d μs, want -500_000 μs", pre)
	}
	// Post-1970 case: source was +500_000 μs. After adding 500ns
	// the true value is +500_000_500 ns (= +500_000.5 μs);
	// round-half-away pushes UP again → +500_001 μs.
	if post != 500_001 {
		t.Errorf("post-1970: got %d μs, want 500_001 μs", post)
	}

	// The key invariant: rounding pushes each result AWAY from
	// zero when it hits an exact-half fraction. Both pre and post
	// crossed a half-μs boundary going in the "away from zero"
	// direction (pre got more negative, post got more positive) —
	// truncate-toward-zero (the pre-fix behavior) would have kept
	// pre at -499_999 (toward zero) and post at +500_000 (toward
	// zero), a visibly asymmetric error direction.
	if pre > -500_000 {
		t.Errorf("pre-1970 rounded toward zero (%d μs); round-half-away should push away from zero", pre)
	}
	if post < 500_001 {
		t.Errorf("post-1970 rounded toward zero (%d μs); round-half-away should push away from zero", post)
	}
}

// newTimestampWithType builds a Timestamp Series with a custom
// (Unit, TimeZone) — the constructors in series_time.go only emit
// Timestamp[ns, no-tz] Series, so tests that need non-default
// type metadata go through this helper.
func newTimestampWithType(t *testing.T, name string, tsType *arrow.TimestampType, times []time.Time, validity []bool) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewTimestampBuilder(pool, tsType)
	defer b.Release()
	div := int64(1)
	switch tsType.Unit {
	case arrow.Microsecond:
		div = 1_000
	case arrow.Millisecond:
		div = 1_000_000
	case arrow.Second:
		div = 1_000_000_000
	}
	for i, ts := range times {
		if validity != nil && !validity[i] {
			b.AppendNull()
			continue
		}
		b.Append(arrow.Timestamp(ts.UnixNano() / div))
	}
	arr := b.NewArray()
	field := arrow.Field{Name: name, Type: tsType, Nullable: true}
	chunked := arrow.NewChunked(tsType, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	arr.Release()
	return Series{name: field.Name, field: field, col: col}
}

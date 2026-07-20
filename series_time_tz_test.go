package gobi

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
)

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

package gobi

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
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

package gobi

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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

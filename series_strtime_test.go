package gobi

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// strChunks builds a String Series named "s" with one chunk per
// argument; nil entries are nulls.
func strChunks(t *testing.T, chunks ...[]any) Series {
	t.Helper()
	arrs := make([]arrow.Array, len(chunks))
	for i, vals := range chunks {
		b := array.NewStringBuilder(memory.DefaultAllocator)
		for _, v := range vals {
			if v == nil {
				b.AppendNull()
			} else {
				b.Append(v.(string))
			}
		}
		arrs[i] = b.NewArray()
		b.Release()
	}
	field := arrow.Field{Name: "s", Type: arrow.BinaryTypes.String, Nullable: true}
	ch := arrow.NewChunked(field.Type, arrs)
	for _, a := range arrs {
		a.Release()
	}
	defer ch.Release()
	return NewSeries(arrow.NewColumn(field, ch))
}

// strTimeMicros returns s's values as µs since the epoch, with
// null rows as nil.
func strTimeMicros(t *testing.T, s Series) []any {
	t.Helper()
	if got := s.DataType().String(); got != "timestamp[us, tz=UTC]" {
		t.Fatalf("type %s, want timestamp[us, tz=UTC]", got)
	}
	var out []any
	for _, c := range s.Column().Data().Chunks() {
		a := c.(*array.Timestamp)
		for i := range a.Len() {
			if a.IsNull(i) {
				out = append(out, nil)
			} else {
				out = append(out, int64(a.Value(i)))
			}
		}
	}
	return out
}

func micros(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UnixMicro()
}

func TestStrToTime(t *testing.T) {
	cases := []struct {
		name   string
		layout string
		in     []any
		want   []any
	}{
		{
			name:   "layout without zone reads UTC",
			layout: "2006-01-02 15:04:05",
			in:     []any{"2024-01-02 03:04:05", nil, "1970-01-01 00:00:00"},
			want:   []any{micros("2024-01-02T03:04:05Z"), nil, int64(0)},
		},
		{
			name:   "default RFC3339 converts offsets to UTC",
			layout: "",
			in:     []any{"2024-01-02T03:04:05-05:00", "2024-01-02T03:04:05Z"},
			want:   []any{micros("2024-01-02T08:04:05Z"), micros("2024-01-02T03:04:05Z")},
		},
		{
			// time.Parse accepts a fraction the layout doesn't spell.
			name:   "sub-microsecond digits round toward the past",
			layout: "2006-01-02 15:04:05",
			in:     []any{"1970-01-01 00:00:00.0000019", "1969-12-31 23:59:59.9999999"},
			want:   []any{int64(1), int64(-1)},
		},
		{
			name:   "years nanoseconds can't hold",
			layout: time.DateOnly,
			in:     []any{"0001-01-01", "9999-12-31"},
			want:   []any{micros("0001-01-01T00:00:00Z"), micros("9999-12-31T00:00:00Z")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := strChunks(t, tc.in)
			defer s.col.Release()
			out, err := s.StrToTime(tc.layout)
			if err != nil {
				t.Fatal(err)
			}
			defer out.col.Release()
			got := strTimeMicros(t, out)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestStrToDate(t *testing.T) {
	s := strChunks(t, []any{"2024-01-02T23:00:00-05:00", "2024-01-02T01:00:00+09:00", nil})
	defer s.col.Release()
	out, err := s.StrToDate(time.RFC3339)
	if err != nil {
		t.Fatal(err)
	}
	defer out.col.Release()
	a := out.Column().Data().Chunk(0).(*array.Date32)
	// The date as written, not the UTC date of the instant
	// (2024-01-03 and 2024-01-01 respectively). 19724 = 2024-01-02.
	if a.Value(0) != 19724 || a.Value(1) != 19724 || !a.IsNull(2) {
		t.Errorf("got %v, want [19724 19724 (null)]", a)
	}

	pre := strChunks(t, []any{"1969-12-31", "1900-03-01"})
	defer pre.col.Release()
	out2, err := pre.StrToDate("")
	if err != nil {
		t.Fatal(err)
	}
	defer out2.col.Release()
	b := out2.Column().Data().Chunk(0).(*array.Date32)
	if b.Value(0) != -1 || b.Value(1) != -25508 {
		t.Errorf("pre-epoch dates = %v, want [-1 -25508]", b)
	}
}

func TestStrToTime_Errors(t *testing.T) {
	// The bad value is in the second chunk: the row number counts
	// across chunks.
	s := strChunks(t, []any{"2024-01-02", nil}, []any{"2024-01-03", "02/01/2024"})
	defer s.col.Release()
	_, err := s.StrToTime(time.DateOnly)
	if !errors.Is(err, ErrTimeParse) {
		t.Fatalf("err = %v, want ErrTimeParse", err)
	}
	var pe *time.ParseError
	if !errors.As(err, &pe) || pe.Value != "02/01/2024" {
		t.Errorf("err = %v, want it to wrap the *time.ParseError", err)
	}
	if !strings.Contains(err.Error(), "index 3") {
		t.Errorf("err = %v, want index 3", err)
	}

	empty := strChunks(t, []any{""})
	defer empty.col.Release()
	if _, err := empty.StrToDate(""); !errors.Is(err, ErrTimeParse) {
		t.Errorf("empty string: err = %v, want ErrTimeParse", err)
	}

	ints := NewInt64Series("n", []int64{1}, nil)
	defer ints.col.Release()
	if _, err := ints.StrToTime(""); !errors.Is(err, ErrColumnTypeMismatch) {
		t.Errorf("Int64 column: err = %v, want ErrColumnTypeMismatch", err)
	}
}

func TestExpr_StrToTime(t *testing.T) {
	f := stringExprFrame(t, "dt", []any{"2024-01-02 03:04:05", nil})
	defer f.Release()

	// Replacing the column in place, as a caller converting before a
	// write would; the planned type has to match the evaluated one.
	lf := f.Lazy().
		WithColumn("dt", Col("dt").StrToTime("2006-01-02 15:04:05")).
		WithColumn("day", Col("dt").Year())
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	dt, err := out.Column("dt")
	if err != nil {
		t.Fatal(err)
	}
	if got := strTimeMicros(t, dt); got[0] != micros("2024-01-02T03:04:05Z") || got[1] != nil {
		t.Errorf("dt = %v", got)
	}
	ty, err := Col("dt").StrToDate("").Node().Type(f.Schema())
	if err != nil || ty.ID() != arrow.DATE32 {
		t.Errorf("StrToDate type = %v, %v; want date32", ty, err)
	}
	if got := Col("dt").StrToTime("2006-01-02").String(); got != `col("dt").str_to_time("2006-01-02")` {
		t.Errorf("String() = %s", got)
	}

	// A bad value fails the plan rather than producing nulls.
	bad := stringExprFrame(t, "dt", []any{"yesterday"})
	defer bad.Release()
	if _, err := bad.Lazy().WithColumn("dt", Col("dt").StrToTime("")).Collect(); !errors.Is(err, ErrTimeParse) {
		t.Errorf("bad value: err = %v, want ErrTimeParse", err)
	}

	ints := NewInt64Series("n", []int64{1}, nil)
	defer ints.col.Release()
	nf, err := NewFrameFromSeries(ints)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Release()
	if _, err := Col("n").StrToTime("").Node().Type(nf.Schema()); !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("Int64 column: err = %v, want ErrExprTypeMismatch", err)
	}
}

func TestCast_StringToTimeHint(t *testing.T) {
	f := stringExprFrame(t, "dt", []any{"2024-01-02"})
	defer f.Release()
	_, err := Col("dt").Cast(arrow.FixedWidthTypes.Timestamp_us).Node().Eval(f)
	if !errors.Is(err, ErrExprTypeMismatch) || !strings.Contains(err.Error(), "StrToTime") {
		t.Errorf("err = %v, want a pointer to StrToTime", err)
	}
}

// TestStrToTime_ZoneAbbreviations — the result doesn't depend on the
// host's zone: an abbreviation with no fixed offset is an error
// wherever it runs, rather than its local meaning on one machine and
// UTC on another.
func TestStrToTime_ZoneAbbreviations(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	saved := time.Local
	time.Local = ny // where time.Parse would read "EST" as -05:00
	t.Cleanup(func() { time.Local = saved })

	est := strChunks(t, []any{"2024-01-02 03:04:05 EST"})
	defer est.col.Release()
	if _, err := est.StrToTime("2006-01-02 15:04:05 MST"); !errors.Is(err, ErrTimeParse) ||
		!strings.Contains(err.Error(), "numeric offset") {
		t.Errorf("EST: err = %v, want ErrTimeParse pointing at numeric offsets", err)
	}
	// With the offset spelled out, the abbreviation is just a label.
	withOff := strChunks(t, []any{"2024-01-02 03:04:05 -0500 EST"})
	defer withOff.col.Release()
	out, err := withOff.StrToTime("2006-01-02 15:04:05 -0700 MST")
	if err != nil {
		t.Fatal(err)
	}
	defer out.col.Release()
	if got := strTimeMicros(t, out); got[0] != micros("2024-01-02T08:04:05Z") {
		t.Errorf("-0500 EST = %v, want 08:04:05Z", got)
	}
	// UTC and GMT need no offset; nor does a date.
	ok := strChunks(t, []any{"2024-01-02 03:04:05 UTC", "2024-01-02 03:04:05 GMT"})
	defer ok.col.Release()
	out2, err := ok.StrToTime("2006-01-02 15:04:05 MST")
	if err != nil {
		t.Fatal(err)
	}
	defer out2.col.Release()
	if got := strTimeMicros(t, out2); got[0] != micros("2024-01-02T03:04:05Z") || got[1] != got[0] {
		t.Errorf("UTC/GMT = %v", got)
	}
	d, err := est.StrToDate("2006-01-02 15:04:05 MST")
	if err != nil {
		t.Fatalf("StrToDate with EST: %v", err)
	}
	d.col.Release()
}

// TestStrToTime_RepeatedValues — runs of equal values (a partition
// key) reuse one parse, and a change of value parses again.
func TestStrToTime_RepeatedValues(t *testing.T) {
	s := strChunks(t,
		[]any{"2024-01-02", "2024-01-02", nil, "2024-01-02"},
		[]any{"2024-01-02", "2024-01-03", "2024-01-02", "bad"})
	defer s.col.Release()
	_, err := s.StrToDate("")
	if !errors.Is(err, ErrTimeParse) || !strings.Contains(err.Error(), "index 7") {
		t.Fatalf("err = %v, want ErrTimeParse at index 7", err)
	}
	good := strChunks(t, []any{"2024-01-02", "2024-01-02", "2024-01-03", "2024-01-03", "2024-01-02"})
	defer good.col.Release()
	out, err := good.StrToDate("")
	if err != nil {
		t.Fatal(err)
	}
	defer out.col.Release()
	a := out.Column().Data().Chunk(0).(*array.Date32)
	want := []arrow.Date32{19724, 19724, 19725, 19725, 19724}
	for i, w := range want {
		if a.Value(i) != w {
			t.Errorf("index %d = %d, want %d", i, a.Value(i), w)
		}
	}
}

// TestStrToTime_PlaceholderToNull — the documented way to keep a
// placeholder from failing the parse: null it inside the parse.
func TestStrToTime_PlaceholderToNull(t *testing.T) {
	f := stringExprFrame(t, "dt", []any{"2024-01-02", "__HIVE_DEFAULT_PARTITION__", nil})
	defer f.Release()
	dt := Col("dt")
	out, err := f.Lazy().WithColumn("dt",
		If(dt.Eq(Lit("__HIVE_DEFAULT_PARTITION__")), LitNull(arrow.BinaryTypes.String), dt).StrToDate("")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	col, err := out.Column("dt")
	if err != nil {
		t.Fatal(err)
	}
	a := col.Column().Data().Chunk(0).(*array.Date32)
	if a.Value(0) != 19724 || !a.IsNull(1) || !a.IsNull(2) {
		t.Errorf("dt = %v, want [19724 (null) (null)]", a)
	}
}

// TestStrToTime_StringTypes — LargeString and StringView columns parse
// like String ones, in the plan and at Eval.
func TestStrToTime_StringTypes(t *testing.T) {
	pool := memory.DefaultAllocator
	lb := array.NewLargeStringBuilder(pool)
	lb.AppendValues([]string{"2024-01-02", ""}, []bool{true, false})
	vb := array.NewStringViewBuilder(pool)
	vb.AppendValues([]string{"2024-01-02", ""}, []bool{true, false})
	large, view := lb.NewArray(), vb.NewArray()
	lb.Release()
	vb.Release()
	for _, arr := range []arrow.Array{large, view} {
		t.Run(arr.DataType().String(), func(t *testing.T) {
			field := arrow.Field{Name: "dt", Type: arr.DataType(), Nullable: true}
			ch := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
			col := arrow.NewColumn(field, ch)
			ch.Release()
			f, err := NewFrame(arrow.NewSchema([]arrow.Field{field}, nil), []arrow.Column{*col})
			if err != nil {
				t.Fatal(err)
			}
			defer f.Release()
			if _, err := Col("dt").StrToTime("2006-01-02").Node().Type(f.Schema()); err != nil {
				t.Errorf("Type: %v", err)
			}
			out, err := f.Lazy().WithColumn("dt", Col("dt").StrToTime("2006-01-02")).Collect()
			if err != nil {
				t.Fatal(err)
			}
			defer out.Release()
			s, _ := out.Column("dt")
			if got := strTimeMicros(t, s); got[0] != micros("2024-01-02T00:00:00Z") || got[1] != nil {
				t.Errorf("dt = %v", got)
			}
			_, err = Col("dt").Cast(arrow.FixedWidthTypes.Date32).Node().Eval(f)
			if !strings.Contains(fmt.Sprint(err), "StrToDate") {
				t.Errorf("Cast to date32: err = %v, want the StrToDate hint", err)
			}
		})
		arr.Release()
	}
}

// TestStrToTime_TypeNotShared — each plan gets its own output type,
// so changing one can't relabel another.
func TestStrToTime_TypeNotShared(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{{Name: "dt", Type: arrow.BinaryTypes.String}}, nil)
	a, err := Col("dt").StrToTime("").Node().Type(schema)
	if err != nil {
		t.Fatal(err)
	}
	a.(*arrow.TimestampType).TimeZone = "America/Chicago"
	b, _ := Col("dt").StrToTime("").Node().Type(schema)
	if b.(*arrow.TimestampType).TimeZone != "UTC" {
		t.Errorf("second plan's zone = %q, want UTC", b.(*arrow.TimestampType).TimeZone)
	}
}

// TestTimeOps_BeyondNanoRange — a µs column holding years nanoseconds
// can't (what StrToTime keeps 9999-12-31 for) stays correct through
// the timestamp ops, and an op whose result can't be stored errors
// instead of wrapping.
func TestTimeOps_BeyondNanoRange(t *testing.T) {
	src := strChunks(t, []any{"9999-12-31", "0001-01-01", nil})
	defer src.col.Release()
	s, err := src.StrToTime(time.DateOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer s.col.Release()

	check := func(name string, got Series, err error, want ...string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		t.Cleanup(func() { got.col.Release() })
		if got.DataType().String() != "timestamp[us, tz=UTC]" {
			t.Errorf("%s: type %s", name, got.DataType())
		}
		for i, w := range want {
			tm, valid, _ := got.TimeAt(i)
			if g := tm.UTC().Format(time.RFC3339); valid && g != w || !valid && w != "" {
				t.Errorf("%s[%d] = %s (valid %v), want %q", name, i, g, valid, w)
			}
		}
	}
	added, err := s.AddDuration(90 * time.Minute)
	check("AddDuration", added, err, "9999-12-31T01:30:00Z", "0001-01-01T01:30:00Z", "")
	sub, err := s.SubDuration(time.Hour)
	check("SubDuration", sub, err, "9999-12-30T23:00:00Z", "0000-12-31T23:00:00Z", "")
	tr, err := s.DateTruncate("month")
	check("DateTruncate", tr, err, "9999-12-01T00:00:00Z", "0001-01-01T00:00:00Z", "")
	tt, err := added.TruncateTo(UnitHour)
	check("TruncateTo", tt, err, "9999-12-31T01:00:00Z", "0001-01-01T01:00:00Z", "")
	tc, err := s.TruncateToCalendar(CalendarYear)
	check("TruncateToCalendar", tc, err, "9999-01-01T00:00:00Z", "0001-01-01T00:00:00Z", "")

	eq, err := s.EqTime(time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	defer eq.col.Release()
	if b := eq.Column().Data().Chunk(0).(*array.Boolean); !b.Value(0) || b.Value(1) {
		t.Errorf("EqTime = %v, want [true false (null)]", b)
	}
	lt, err := s.LtTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	defer lt.col.Release()
	if b := lt.Column().Data().Chunk(0).(*array.Boolean); b.Value(0) || !b.Value(1) {
		t.Errorf("LtTime = %v, want [false true (null)]", b)
	}

	f, err := NewFrameFromSeries(renameSeries(s, "dt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if _, err := Col("dt").UnixNano().Node().Eval(f); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("UnixNano: err = %v, want ErrTimeOutOfRange", err)
	}

	// A nanosecond column at its last representable day: one more day
	// can't be stored.
	ns := NewTimestampSeries("ns", []time.Time{time.Unix(0, math.MaxInt64-int64(time.Hour))}, nil)
	defer ns.col.Release()
	if _, err := ns.AddDuration(24 * time.Hour); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("ns AddDuration past 2262: err = %v, want ErrTimeOutOfRange", err)
	}
}

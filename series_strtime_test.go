package gobi

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
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
			// 1.9 µs, -0.1 µs, 0.5 µs, -0.5 µs: nearest, halves away
			// from zero, as AddDuration rounds.
			name:   "sub-microsecond digits round to nearest",
			layout: "2006-01-02 15:04:05",
			in: []any{"1970-01-01 00:00:00.0000019", "1969-12-31 23:59:59.9999999",
				"1970-01-01 00:00:00.0000005", "1969-12-31 23:59:59.9999995"},
			want: []any{int64(2), int64(0), int64(1), int64(-1)},
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
//
// The assertions run twice: here, under whatever zone the host has,
// and in a child process with TZ=America/New_York, where time.Parse
// would read "EST" as -05:00. (A child, because setting time.Local in
// this process would race with anything else reading it.)
func TestStrToTime_ZoneAbbreviations(t *testing.T) {
	if os.Getenv("GOBI_TZ_CHILD") == "" {
		if _, err := time.LoadLocation("America/New_York"); err != nil {
			t.Skip("no tzdata:", err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestStrToTime_ZoneAbbreviations$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TZ=America/New_York", "GOBI_TZ_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("under TZ=America/New_York: %v\n%s", err, out)
		}
	} else if name, _ := time.Now().Zone(); name != "EST" && name != "EDT" {
		t.Fatalf("child's zone is %s, want New York's", name)
	}

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

	dtSeries := renameSeries(s, "dt")
	defer dtSeries.col.Release()
	f, err := NewFrameFromSeries(dtSeries)
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

// TestDiffDuration_BeyondNanoRange — instants further apart than
// Int64 nanoseconds reach are an error, not a saturated ±292 years.
func TestDiffDuration_BeyondNanoRange(t *testing.T) {
	a := strChunks(t, []any{"9999-12-31", "2000-01-01"})
	defer a.col.Release()
	b := strChunks(t, []any{"1970-01-01", "1970-01-01"})
	defer b.col.Release()
	at, err := a.StrToTime(time.DateOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer at.col.Release()
	bt, err := b.StrToTime(time.DateOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer bt.col.Release()
	if _, err := at.DiffDuration(bt); !errors.Is(err, ErrTimeOutOfRange) || !strings.Contains(err.Error(), "index 0") {
		t.Errorf("err = %v, want ErrTimeOutOfRange at index 0", err)
	}
	// In range, the difference is exact.
	near, err := at.SubDuration(0)
	if err != nil {
		t.Fatal(err)
	}
	defer near.col.Release()
	d, err := near.DiffDuration(at)
	if err != nil {
		t.Fatal(err)
	}
	defer d.col.Release()
	if v := d.Column().Data().Chunk(0).(*array.Int64); v.Value(0) != 0 || v.Value(1) != 0 {
		t.Errorf("self-diff = %v, want [0 0]", v)
	}
}

// TestLitTime_BeyondNanoRange — Lit(time) is always Timestamp[ns]
// (its type mustn't depend on the date), so a date outside 1677-2262
// is an error pointing at LitTime; LitTime(t, µs) holds it and compares
// correctly through the scalar fast path (WithColumn) and the fused
// filter (Filter).
func TestLitTime_BeyondNanoRange(t *testing.T) {
	f := stringExprFrame(t, "dt", []any{"1500-06-01", "1700-01-01", "9999-12-31", nil})
	defer f.Release()
	parsed := Col("dt").StrToTime(time.DateOnly)
	d1600 := time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)
	d9999 := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

	for _, tm := range []time.Time{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Unix(0, 1)} {
		if ty, err := Lit(tm).Node().Type(nil); err != nil || ty.String() != "timestamp[ns]" {
			t.Errorf("Lit(%s) type = %v, %v; want timestamp[ns]", tm, ty, err)
		}
	}
	if _, err := Lit(d9999).Node().Type(nil); !errors.Is(err, ErrTimeOutOfRange) || !strings.Contains(err.Error(), "LitTime") {
		t.Errorf("Lit(9999): err = %v, want ErrTimeOutOfRange pointing at LitTime", err)
	}
	if ty, err := LitTime(d1600, arrow.Microsecond).Node().Type(nil); err != nil || ty.String() != "timestamp[us]" {
		t.Errorf("LitTime(1600, us) type = %v, %v; want timestamp[us]", ty, err)
	}

	out, err := f.Lazy().
		WithColumn("lt1600", parsed.Lt(LitTime(d1600, arrow.Microsecond))).
		WithColumn("eq9999", parsed.Eq(LitTime(d9999, arrow.Second))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	for col, want := range map[string]string{
		"lt1600": "[true false false (null)]",
		"eq9999": "[false false true (null)]",
	} {
		s, err := out.Column(col)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(s.Column().Data().Chunk(0)); got != want {
			t.Errorf("%s = %s, want %s", col, got, want)
		}
	}

	kept, err := f.Lazy().WithColumn("dt", parsed).Filter(Col("dt").Lt(LitTime(d1600, arrow.Microsecond))).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Release()
	if kept.NumRows() != 1 {
		t.Errorf("Filter(dt < 1600) kept %d rows, want 1", kept.NumRows())
	}

	// A unit that can't hold t exactly is an error, not a rounded literal.
	if _, err := LitTime(d1600.Add(time.Nanosecond), arrow.Microsecond).Node().Type(nil); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("LitTime(1600 + 1ns, us): err = %v, want ErrTimeOutOfRange", err)
	}
	if _, err := LitTime(d1600, arrow.TimeUnit(9)).Node().Type(nil); !errors.Is(err, ErrUnsupportedLiteral) {
		t.Errorf("LitTime(bad unit): err = %v, want ErrUnsupportedLiteral", err)
	}
}

// TestLitTime_StringNamesInstant — a timestamp literal prints as its
// instant and unit, so equal raw counts in different units (different
// instants) don't print alike: String() is an expression's identity.
func TestLitTime_StringNamesInstant(t *testing.T) {
	ns := Lit(time.Unix(0, 9_000_000_000_000_000)) // 1970-04-15 in ns
	us := LitTime(time.UnixMicro(9_000_000_000_000_000), arrow.Microsecond)
	if ns.String() == us.String() {
		t.Errorf("both print %s", ns)
	}
	if got := ns.String(); got != "lit(timestamp[ns] 1970-04-15T04:00:00Z)" {
		t.Errorf("ns literal = %s", got)
	}
}

// TestTimeToUnit_Bounds — the ends of the int64 range convert in every
// mode: the seconds part of Timestamp[ns] MinInt64 overflows on its
// own, but the value doesn't.
func TestTimeToUnit_Bounds(t *testing.T) {
	for _, v := range []int64{math.MinInt64, math.MinInt64 + 1, -1, 0, math.MaxInt64} {
		for _, mode := range []unitRounding{timeRoundHalfAway, timeRoundFloor, timeRoundExact} {
			got, ok := timeToUnit(time.Unix(0, v), arrow.Nanosecond, mode)
			if !ok || int64(got) != v {
				t.Errorf("timeToUnit(%d ns, ns, %d) = %d, %v", v, mode, got, ok)
			}
		}
	}
	// (Not seconds: time.Time itself can't hold ±2^63 seconds.)
	for _, unit := range []arrow.TimeUnit{arrow.Millisecond, arrow.Microsecond} {
		per := unitToNanos(unit)
		// The smallest and largest values of the unit round-trip...
		for _, v := range []int64{math.MinInt64, math.MaxInt64} {
			tm := arrowTimestampToTime(arrow.Timestamp(v), unit, time.UTC)
			if got, ok := timeToUnit(tm, unit, timeRoundExact); !ok || int64(got) != v {
				t.Errorf("%s: %d round-trips to %d, %v", unit, v, got, ok)
			}
		}
		// ...and one unit past each end doesn't fit.
		minT := arrowTimestampToTime(math.MinInt64, unit, time.UTC)
		if _, ok := timeToUnit(minT.Add(-time.Duration(per)), unit, timeRoundFloor); ok {
			t.Errorf("%s: one below MinInt64 fit", unit)
		}
		maxT := arrowTimestampToTime(math.MaxInt64, unit, time.UTC)
		if _, ok := timeToUnit(maxT.Add(time.Duration(per)), unit, timeRoundFloor); ok {
			t.Errorf("%s: one above MaxInt64 fit", unit)
		}
	}
	// Modes on a sub-unit remainder: 1.5 µs and -1.5 µs.
	for _, c := range []struct {
		ns   int64
		mode unitRounding
		want int64
		ok   bool
	}{
		{1500, timeRoundHalfAway, 2, true}, {-1500, timeRoundHalfAway, -2, true},
		{1500, timeRoundFloor, 1, true}, {-1500, timeRoundFloor, -2, true},
		{1500, timeRoundExact, 0, false}, {2000, timeRoundExact, 2, true},
	} {
		got, ok := timeToUnit(time.Unix(0, c.ns), arrow.Microsecond, c.mode)
		if ok != c.ok || ok && int64(got) != c.want {
			t.Errorf("timeToUnit(%d ns, us, %d) = %d, %v; want %d, %v", c.ns, c.mode, got, ok, c.want, c.ok)
		}
	}
}

// TestTimeOps_NanoRangeEnds — ns values at the very ends of the range
// pass through no-op shifts and truncations, and an op that leaves
// the range names itself.
func TestTimeOps_NanoRangeEnds(t *testing.T) {
	ends := []time.Time{time.Unix(0, math.MinInt64), time.Unix(0, math.MaxInt64)}
	s := NewTimestampSeries("ns", ends, nil)
	defer s.col.Release()
	same := func(name string, out Series, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		defer out.col.Release()
		a := out.Column().Data().Chunk(0).(*array.Timestamp)
		if a.Value(0) != math.MinInt64 || a.Value(1) != math.MaxInt64 {
			t.Errorf("%s = %v, want the inputs back", name, a)
		}
	}
	out, err := s.AddDuration(0)
	same("AddDuration(0)", out, err)
	out, err = s.AddDuration(time.Nanosecond / 2) // 0: not the integer path
	same("AddDuration(0.5ns)", out, err)
	out, err = s.TruncateTo(UnitNanosecond)
	same("TruncateTo(ns)", out, err)

	if _, err := s.SubDuration(time.Hour); !errors.Is(err, ErrTimeOutOfRange) ||
		!strings.Contains(err.Error(), "SubDuration") {
		t.Errorf("SubDuration past MinInt64: err = %v, want ErrTimeOutOfRange naming SubDuration", err)
	}
	if _, err := s.TruncateTo(UnitHour); !errors.Is(err, ErrTimeOutOfRange) ||
		!strings.Contains(err.Error(), "TruncateTo") {
		t.Errorf("TruncateTo(hour) below MinInt64: err = %v, want ErrTimeOutOfRange", err)
	}
	// Both shift paths agree: a whole number of units (integer) and
	// not (time.Time), on a µs column.
	us, err := NewTimestampSeriesUnit("us", []time.Time{time.Unix(0, 1_000_500)}, nil, arrow.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	defer us.col.Release()
	whole, err := us.AddDuration(time.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	defer whole.col.Release()
	part, err := us.AddDuration(1000*time.Nanosecond + 499)
	if err != nil {
		t.Fatal(err)
	}
	defer part.col.Release()
	w := whole.Column().Data().Chunk(0).(*array.Timestamp).Value(0)
	p := part.Column().Data().Chunk(0).(*array.Timestamp).Value(0)
	if w != 1001 || p != 1001 {
		t.Errorf("AddDuration(1µs) = %d, AddDuration(1.499µs) = %d; want 1001, 1001", w, p)
	}
}

// TestTimestampOf — floors finer precision, rejects what doesn't fit.
func TestTimestampOf(t *testing.T) {
	got, err := TimestampOf(time.Unix(0, -1500), arrow.Microsecond)
	if err != nil || got != -2 {
		t.Errorf("-1.5µs → %d, %v; want -2 (floored)", got, err)
	}
	if _, err := TimestampOf(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), arrow.Nanosecond); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("9999 in ns: err = %v, want ErrTimeOutOfRange", err)
	}
	if v, err := TimestampOf(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), arrow.Microsecond); err != nil ||
		!v.ToTime(arrow.Microsecond).Equal(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("9999 in µs: %d, %v", v, err)
	}
}

// TestNewTimestampSeries_OutOfRangePanics — no silently wrapped date.
func TestNewTimestampSeries_OutOfRangePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "NewTimestampSeriesUnit") {
			t.Errorf("recover() = %v, want a panic pointing at NewTimestampSeriesUnit", r)
		}
	}()
	NewTimestampSeries("t", []time.Time{time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)}, nil)
}

// TestFromStructs_MicrosecondOverflow — a µs struct field past its
// range is ErrStructFieldOverflow too (only ns used to be checked).
func TestFromStructs_MicrosecondOverflow(t *testing.T) {
	type row struct {
		T time.Time `gobi:"T,timestamp(microsecond)"`
	}
	_, err := FromStructs([]row{{time.Date(300000, 1, 1, 0, 0, 0, 0, time.UTC)}})
	if !errors.Is(err, ErrStructFieldOverflow) || !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("err = %v, want ErrStructFieldOverflow wrapping ErrTimeOutOfRange", err)
	}
}

// TestTimeCmp_FastPathRounding — the EqTime family on a raw-value fast
// path rounds a target between two column ticks the way each op needs,
// and agrees with the instant-by-instant path (two chunks).
func TestTimeCmp_FastPathRounding(t *testing.T) {
	ms := []time.Time{time.UnixMilli(1), time.UnixMilli(2), time.UnixMilli(-2)}
	one, err := NewTimestampSeriesUnit("t", ms, nil, arrow.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer one.col.Release()
	ch := arrow.NewChunked(one.DataType(), []arrow.Array{one.Column().Data().Chunk(0), one.Column().Data().Chunk(0)})
	two := NewSeries(arrow.NewColumn(arrow.Field{Name: "t", Type: one.DataType(), Nullable: true}, ch))
	ch.Release()
	defer two.col.Release()
	target := time.Unix(0, 1_500_000) // 1.5 ms
	for _, c := range []struct {
		name string
		fn   func(Series, time.Time) (Series, error)
		want string
	}{
		{"Lt", Series.LtTime, "[true false true]"},
		{"Le", Series.LeTime, "[true false true]"},
		{"Gt", Series.GtTime, "[false true false]"},
		{"Ge", Series.GeTime, "[false true false]"},
		{"Eq", Series.EqTime, "[false false false]"},
		{"Ne", Series.NeTime, "[true true true]"},
	} {
		for name, s := range map[string]Series{"single": one, "chunked": two} {
			out, err := c.fn(s, target)
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			if name == "chunked" { // the same three rows twice
				in := strings.Trim(c.want, "[]")
				want = "[" + in + " " + in + "]"
			}
			if got := fmt.Sprint(out.Column().Data().Chunk(0)); got != want {
				t.Errorf("%s %s 1.5ms = %s, want %s", name, c.name, got, want)
			}
			out.col.Release()
		}
	}
}

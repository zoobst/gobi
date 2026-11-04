package gobi

import (
	"errors"
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
	if !strings.Contains(err.Error(), "row 3") {
		t.Errorf("err = %v, want row 3", err)
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

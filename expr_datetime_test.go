package gobi

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// timeExprFrame wraps a Timestamp Series into a single-column Frame
// for Expr-level testing.
func timeExprFrame(t *testing.T, s Series) *Frame {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{s.field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*s.col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// evalInt64 extracts the Int64 output of an Expr.
func evalInt64(t *testing.T, f *Frame, e Expr) []int64 {
	t.Helper()
	s, err := e.Node().Eval(f)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	out := make([]int64, 0, s.Len())
	for _, chunk := range s.Column().Data().Chunks() {
		a := chunk.(*array.Int64)
		for i := range a.Len() {
			out = append(out, a.Value(i))
		}
	}
	return out
}

func TestExpr_DatetimeExtractors(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 3, 22, 14, 30, 45, 123_000_000, time.UTC),
	}, nil)
	f := timeExprFrame(t, s)
	defer f.Release()

	if v := evalInt64(t, f, Col("t").Year())[0]; v != 2026 {
		t.Errorf("Year = %d, want 2026", v)
	}
	if v := evalInt64(t, f, Col("t").Month())[0]; v != 3 {
		t.Errorf("Month = %d, want 3", v)
	}
	if v := evalInt64(t, f, Col("t").Day())[0]; v != 22 {
		t.Errorf("Day = %d, want 22", v)
	}
	if v := evalInt64(t, f, Col("t").Hour())[0]; v != 14 {
		t.Errorf("Hour = %d, want 14", v)
	}
	if v := evalInt64(t, f, Col("t").Minute())[0]; v != 30 {
		t.Errorf("Minute = %d, want 30", v)
	}
	if v := evalInt64(t, f, Col("t").Second())[0]; v != 45 {
		t.Errorf("Second = %d, want 45", v)
	}
	if v := evalInt64(t, f, Col("t").Nanosecond())[0]; v != 123_000_000 {
		t.Errorf("Nanosecond = %d, want 123_000_000", v)
	}
	// 2026-03-22 is a Sunday → weekday = 0.
	if v := evalInt64(t, f, Col("t").Weekday())[0]; v != 0 {
		t.Errorf("Weekday = %d, want 0 (Sunday)", v)
	}
	// Day 81 of 2026 (Jan 31 + Feb 28 + Mar 22 = 81).
	if v := evalInt64(t, f, Col("t").DayOfYear())[0]; v != 81 {
		t.Errorf("DayOfYear = %d, want 81", v)
	}
}

func TestExpr_DateTruncate_ComposesWithFilter(t *testing.T) {
	// A rank of timestamps across January + February 2026; filter
	// to just those whose day-of-month is in the first week.
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC),
	}, nil)
	f := timeExprFrame(t, s)
	defer f.Release()

	out, err := f.Lazy().Filter(
		Col("t").Day().Le(Lit(int64(7))),
	).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	defer out.Release()
	if got := out.NumRows(); got != 2 {
		t.Errorf("row count = %d, want 2 (Jan 3, Feb 5)", got)
	}
}

func TestExpr_DateFormat_ProducesString(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC),
	}, nil)
	f := timeExprFrame(t, s)
	defer f.Release()
	out, err := Col("t").DateFormat("2006-01-02").Node().Eval(f)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	arr := out.Column().Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "2026-03-22" {
		t.Errorf("format = %q, want %q", arr.Value(0), "2026-03-22")
	}
}

func TestExpr_AddDuration(t *testing.T) {
	s := NewTimestampSeries("t", []time.Time{
		time.Date(2026, 3, 22, 12, 0, 0, 0, time.UTC),
	}, nil)
	f := timeExprFrame(t, s)
	defer f.Release()
	out, err := Col("t").AddDuration(24 * time.Hour).Node().Eval(f)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	got, valid, err := out.TimeAt(0)
	if err != nil || !valid {
		t.Fatalf("TimeAt: err=%v valid=%v", err, valid)
	}
	want := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("AddDuration = %v, want %v", got, want)
	}
}

func TestExpr_Datetime_NonTimestampErrors(t *testing.T) {
	// Build a String frame; try to call Year() on it → should error
	// at Type-check.
	f := stringExprFrame(t, "s", []any{"hello"})
	defer f.Release()

	_, err := Col("s").Year().Node().Type(f.Schema())
	if err == nil {
		t.Errorf("Year on String column should error at Type-check")
	}
	_, err = Col("s").Year().Node().Eval(f)
	if err == nil {
		t.Errorf("Year on String column should error at Eval")
	}
}

func TestExpr_Datetime_ReflectionSurface(t *testing.T) {
	// Table-driven walk over every datetime op — Children() count,
	// String() non-empty, Type() returns the expected shape.
	s := NewTimestampSeries("t", []time.Time{time.Now()}, nil)
	f := timeExprFrame(t, s)
	defer f.Release()

	cases := []struct {
		name string
		e    Expr
		want arrow.Type // arrow.INT64 / arrow.TIMESTAMP / arrow.STRING
	}{
		{"Year", Col("t").Year(), arrow.INT64},
		{"Month", Col("t").Month(), arrow.INT64},
		{"Day", Col("t").Day(), arrow.INT64},
		{"Hour", Col("t").Hour(), arrow.INT64},
		{"Minute", Col("t").Minute(), arrow.INT64},
		{"Second", Col("t").Second(), arrow.INT64},
		{"Nanosecond", Col("t").Nanosecond(), arrow.INT64},
		{"Weekday", Col("t").Weekday(), arrow.INT64},
		{"DayOfYear", Col("t").DayOfYear(), arrow.INT64},
		{"DateTruncate", Col("t").DateTruncate("day"), arrow.TIMESTAMP},
		{"AddDuration", Col("t").AddDuration(time.Hour), arrow.TIMESTAMP},
		{"SubDuration", Col("t").SubDuration(time.Hour), arrow.TIMESTAMP},
		{"DateFormat", Col("t").DateFormat("2006"), arrow.STRING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			children := tc.e.Node().Children()
			if len(children) != 1 {
				t.Errorf("Children() len = %d, want 1", len(children))
			}
			str := tc.e.String()
			if str == "" || str == "<nil-expr>" {
				t.Errorf("String() = %q", str)
			}
			dt, err := tc.e.Node().Type(f.Schema())
			if err != nil {
				t.Fatalf("Type: %v", err)
			}
			if dt.ID() != tc.want {
				t.Errorf("Type() = %s, want %s", dt, tc.want)
			}
		})
	}
}

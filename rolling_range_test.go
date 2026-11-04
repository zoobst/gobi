package gobi

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// unitTimeFrame builds a Frame of a Timestamp[unit, UTC] column "t"
// (nil entries null) and a Float64 column "a".
func unitTimeFrame(t *testing.T, unit arrow.TimeUnit, ts []*time.Time, a []float64) *Frame {
	t.Helper()
	vals := make([]time.Time, len(ts))
	valid := make([]bool, len(ts))
	for i, p := range ts {
		if p != nil {
			vals[i], valid[i] = *p, true
		}
	}
	tsS, err := NewTimestampSeriesUnit("t", vals, valid, unit)
	if err != nil {
		t.Fatal(err)
	}
	defer tsS.col.Release()
	aS := NewFloat64Series("a", a, nil)
	defer aS.col.Release()
	f, err := NewFrameFromSeries(tsS, aS)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func tp(y int, mo time.Month, d, h, mi, s int) *time.Time {
	t := time.Date(y, mo, d, h, mi, s, 0, time.UTC)
	return &t
}

// TestRollingBy_BeyondNanoRange — windows over a µs column reaching
// past 1677-2262 are computed on the instants, not on wrapped
// nanosecond counts.
func TestRollingBy_BeyondNanoRange(t *testing.T) {
	f := unitTimeFrame(t, arrow.Microsecond, []*time.Time{
		tp(1500, 1, 1, 0, 0, 0), tp(1500, 1, 1, 0, 0, 1), tp(1500, 1, 1, 0, 0, 2), tp(9999, 12, 31, 0, 0, 0),
	}, []float64{1, 2, 3, 4})
	defer f.Release()
	r, err := f.RollingBy("t", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := r.Agg("a", AggSum)
	if err != nil {
		t.Fatal(err)
	}
	defer sum.col.Release()
	// (t-1s, t]: the row itself and one a second before.
	if got := fmt.Sprint(sum.Column().Data().Chunk(0)); got != "[1 3 5 4]" {
		t.Errorf("sums = %s, want [1 3 5 4]", got)
	}
}

// TestRollingBy_LeadingNullBeforeEpoch — a null first row doesn't make
// pre-1970 times look out of order (they used to be compared with a
// zero standing in for the null).
func TestRollingBy_LeadingNullBeforeEpoch(t *testing.T) {
	f := unitTimeFrame(t, arrow.Nanosecond, []*time.Time{
		nil, tp(1960, 1, 1, 0, 0, 0), tp(1960, 1, 1, 0, 0, 1),
	}, []float64{1, 2, 3})
	defer f.Release()
	if _, err := f.RollingBy("t", time.Minute); err != nil {
		t.Errorf("RollingBy: %v", err)
	}
}

// TestResampleEvery_Ranges — bucketing works in the column's unit;
// a bucket start the ns output can't hold is ErrTimeOutOfRange rather
// than a wrapped one, including at the very bottom of the ns range.
func TestResampleEvery_Ranges(t *testing.T) {
	agg := Aggregation{Column: "a", Kind: AggSum, Alias: "s"}

	// µs column inside the ns range: same buckets as a ns column.
	in := unitTimeFrame(t, arrow.Microsecond, []*time.Time{
		tp(2024, 1, 1, 8, 5, 0), tp(2024, 1, 1, 8, 55, 0), tp(2024, 1, 1, 9, 15, 0), tp(1960, 1, 1, 0, 30, 0),
	}, []float64{1, 2, 4, 8})
	defer in.Release()
	r, err := in.ResampleEvery("t", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Agg(agg)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	starts, _ := out.Column("t")
	sums, _ := out.Column("s")
	var got []string
	for i := range out.NumRows() {
		tm, _, _ := starts.TimeAt(i)
		got = append(got, fmt.Sprintf("%s=%v", tm.UTC().Format(time.RFC3339), sums.Column().Data().Chunk(0).(*array.Float64).Value(i)))
	}
	if fmt.Sprint(got) != "[1960-01-01T00:00:00Z=8 2024-01-01T08:00:00Z=3 2024-01-01T09:00:00Z=4]" {
		t.Errorf("buckets = %v", got)
	}
	if ty := starts.DataType().String(); ty != "timestamp[ns, tz=UTC]" {
		t.Errorf("bucket type = %s, want timestamp[ns, tz=UTC]", ty)
	}

	// µs column past 2262: no ns bucket start.
	far := unitTimeFrame(t, arrow.Microsecond, []*time.Time{tp(9999, 12, 31, 0, 0, 0)}, []float64{1})
	defer far.Release()
	r, _ = far.ResampleEvery("t", time.Hour)
	if _, err := r.Agg(agg); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("9999: err = %v, want ErrTimeOutOfRange", err)
	}
	// An interval that isn't a whole number of microseconds takes the
	// nanosecond path, with the same check.
	r, _ = far.ResampleEvery("t", 1500*time.Nanosecond)
	if _, err := r.Agg(agg); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("9999, 1.5µs: err = %v, want ErrTimeOutOfRange", err)
	}

	// ns MinInt64: its hour bucket starts before the ns range.
	low := unitTimeFrame(t, arrow.Nanosecond, []*time.Time{new(time.Unix(0, math.MinInt64))}, []float64{1})
	defer low.Release()
	r, _ = low.ResampleEvery("t", time.Hour)
	if _, err := r.Agg(agg); !errors.Is(err, ErrTimeOutOfRange) {
		t.Errorf("MinInt64: err = %v, want ErrTimeOutOfRange", err)
	}
}

// TestResampleEvery_Date32 — a Date32 time column buckets by day
// instead of panicking on the output type.
func TestResampleEvery_Date32(t *testing.T) {
	d := array.NewDate32Builder(memory.DefaultAllocator)
	d.AppendValues([]arrow.Date32{19724, 19724, 19725}, nil)
	ds := SeriesFromArray(arrow.Field{Name: "t", Type: arrow.FixedWidthTypes.Date32}, d.NewArray())
	d.Release()
	defer ds.col.Release()
	as := NewFloat64Series("a", []float64{1, 2, 4}, nil)
	defer as.col.Release()
	f, err := NewFrameFromSeries(ds, as)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	r, err := f.ResampleEvery("t", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Agg(Aggregation{Column: "a", Kind: AggSum, Alias: "s"})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	s, _ := out.Column("s")
	if got := fmt.Sprint(s.Column().Data().Chunk(0)); got != "[3 4]" {
		t.Errorf("daily sums = %s, want [3 4]", got)
	}
}

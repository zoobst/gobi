package gobi

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestRollingSum_WindowOfThree(t *testing.T) {
	s := floatSeries("v", []float64{1, 2, 3, 4, 5}, nil)
	out, err := s.RollingSum(3)
	if err != nil {
		t.Fatal(err)
	}
	arr := out.col.Data().Chunks()[0].(*array.Float64)
	// Rows 0 and 1 are null (window not yet full).
	if !arr.IsNull(0) || !arr.IsNull(1) {
		t.Fatalf("rows 0/1 should be null")
	}
	// Rows 2..4: 1+2+3=6, 2+3+4=9, 3+4+5=12.
	want := []float64{6, 9, 12}
	for i, w := range want {
		if arr.Value(i+2) != w {
			t.Errorf("row %d = %v, want %v", i+2, arr.Value(i+2), w)
		}
	}
}

func TestRollingMean_WithNullsInWindow(t *testing.T) {
	s := floatSeries("v", []float64{1, 2, 3, 4, 5}, []bool{true, false, true, true, true})
	out, err := s.RollingMean(3)
	if err != nil {
		t.Fatal(err)
	}
	arr := out.col.Data().Chunks()[0].(*array.Float64)
	// Row 2: values are 1, null, 3 → mean (1+3)/2 = 2.
	if math.Abs(arr.Value(2)-2) > 1e-12 {
		t.Fatalf("row 2 mean = %v, want 2", arr.Value(2))
	}
	// Row 3: null, 3, 4 → mean (3+4)/2 = 3.5.
	if math.Abs(arr.Value(3)-3.5) > 1e-12 {
		t.Fatalf("row 3 mean = %v, want 3.5", arr.Value(3))
	}
}

func TestRollingMinMax(t *testing.T) {
	s := floatSeries("v", []float64{5, 2, 4, 1, 3}, nil)
	minOut, _ := s.RollingMin(3)
	maxOut, _ := s.RollingMax(3)
	minArr := minOut.col.Data().Chunks()[0].(*array.Float64)
	maxArr := maxOut.col.Data().Chunks()[0].(*array.Float64)
	// Windows starting at row 2:
	//   row 2: {5,2,4} min=2, max=5
	//   row 3: {2,4,1} min=1, max=4
	//   row 4: {4,1,3} min=1, max=4
	if minArr.Value(2) != 2 || minArr.Value(3) != 1 || minArr.Value(4) != 1 {
		t.Fatalf("min: %v %v %v", minArr.Value(2), minArr.Value(3), minArr.Value(4))
	}
	if maxArr.Value(2) != 5 || maxArr.Value(3) != 4 || maxArr.Value(4) != 4 {
		t.Fatalf("max: %v %v %v", maxArr.Value(2), maxArr.Value(3), maxArr.Value(4))
	}
}

func TestRollingCount_NullsIgnored(t *testing.T) {
	s := floatSeries("v", []float64{1, 2, 3, 4, 5}, []bool{true, false, true, true, false})
	out, err := s.RollingCount(3)
	if err != nil {
		t.Fatal(err)
	}
	arr := out.col.Data().Chunks()[0].(*array.Int64)
	// Row 2: {valid, null, valid} → 2
	// Row 3: {null, valid, valid} → 2
	// Row 4: {valid, valid, null} → 2
	for i := 2; i <= 4; i++ {
		if arr.Value(i) != 2 {
			t.Errorf("row %d count = %d, want 2", i, arr.Value(i))
		}
	}
}

func TestRollingWindowBoundary(t *testing.T) {
	s := floatSeries("v", []float64{1, 2, 3}, nil)
	// Window == length: only the last row has a full window.
	out, _ := s.RollingSum(3)
	arr := out.col.Data().Chunks()[0].(*array.Float64)
	if !arr.IsNull(0) || !arr.IsNull(1) {
		t.Fatalf("rows 0/1 should be null")
	}
	if arr.Value(2) != 6 {
		t.Fatalf("row 2 = %v, want 6", arr.Value(2))
	}

	// Window > length: every row null.
	out, _ = s.RollingSum(5)
	arr = out.col.Data().Chunks()[0].(*array.Float64)
	for i := 0; i < 3; i++ {
		if !arr.IsNull(i) {
			t.Errorf("row %d should be null when window > len", i)
		}
	}
}

func TestRollingInvalidWindowErrors(t *testing.T) {
	s := floatSeries("v", []float64{1, 2, 3}, nil)
	if _, err := s.RollingSum(0); err == nil {
		t.Fatal("expected error for window=0")
	}
	if _, err := s.RollingSum(-1); err == nil {
		t.Fatal("expected error for negative window")
	}
}

func TestRollingBy_TimeBasedWindow(t *testing.T) {
	// Times 1s apart; 3-second window means each row's window covers
	// [t-3s, t] which contains up to 4 rows (the current + 3 preceding
	// within 3 seconds). We use closed-right, exclusive-past-3s semantics
	// — see rolling.go docs.
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ts := make([]time.Time, 6)
	for i := range ts {
		ts[i] = base.Add(time.Duration(i) * time.Second)
	}
	df := timeSeriesFrame(t, ts,
		[]float64{1, 2, 3, 4, 5, 6},
		[]float64{0, 0, 0, 0, 0, 0},
	)
	r, err := df.RollingBy("t", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sumS, err := r.Agg("a", AggSum)
	if err != nil {
		t.Fatal(err)
	}
	arr := sumS.col.Data().Chunks()[0].(*array.Float64)
	// Row i covers rows with t in (t_i - 3s, t_i], i.e. j such that
	// (t_i.UnixNano - t_j.UnixNano) <= 3s. Since spacing is 1s, that's
	// the current row + at most 3 preceding rows.
	//   i=0: {1}         → 1
	//   i=1: {1,2}       → 3
	//   i=2: {1,2,3}     → 6
	//   i=3: {1,2,3,4}   → 10   (t_3 - t_0 = 3s, included)
	//   i=4: {2,3,4,5}   → 14
	//   i=5: {3,4,5,6}   → 18
	want := []float64{1, 3, 6, 10, 14, 18}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d sum = %v, want %v", i, arr.Value(i), w)
		}
	}
}

func TestRollingBy_NonMonotonicErrors(t *testing.T) {
	base := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	ts := []time.Time{base, base.Add(time.Second), base} // decreasing at row 2
	df := timeSeriesFrame(t, ts, []float64{1, 2, 3}, []float64{0, 0, 0})
	_, err := df.RollingBy("t", time.Second)
	if !errors.Is(err, ErrNotMonotonic) {
		t.Fatalf("want ErrNotMonotonic, got %v", err)
	}
}

func TestRollingBy_NonDateTimeColumnErrors(t *testing.T) {
	f := smallFrame(t)
	_, err := f.RollingBy("pop", time.Second)
	if !errors.Is(err, ErrNotDateTime) {
		t.Fatalf("want ErrNotDateTime, got %v", err)
	}
}

func TestRollingBy_ZeroPeriodErrors(t *testing.T) {
	base := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	df := timeSeriesFrame(t, []time.Time{base}, []float64{1}, []float64{0})
	if _, err := df.RollingBy("t", 0); err == nil {
		t.Fatal("expected error for zero period")
	}
}

// timeSeriesFrame builds a frame with a Timestamp column named "t" and
// two float64 measurement columns "a", "b".
func timeSeriesFrame(t *testing.T, ts []time.Time, a, b []float64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	tsB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Nanosecond})
	defer tsB.Release()
	for _, x := range ts {
		tsB.Append(arrow.Timestamp(x.UnixNano()))
	}
	aB := array.NewFloat64Builder(pool)
	defer aB.Release()
	aB.AppendValues(a, nil)
	bB := array.NewFloat64Builder(pool)
	defer bB.Release()
	bB.AppendValues(b, nil)

	fields := []arrow.Field{
		{Name: "t", Type: &arrow.TimestampType{Unit: arrow.Nanosecond}, Nullable: false},
		{Name: "a", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "b", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{tsB.NewArray(), aB.NewArray(), bB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestResampleEvery_HourlyBucketsAlignToUTCHour(t *testing.T) {
	// Six rows spanning ~2 hours, with irregular spacing.
	ts := []time.Time{
		time.Date(2026, 7, 20, 8, 5, 0, 0, time.UTC),   // bucket 08:00
		time.Date(2026, 7, 20, 8, 55, 0, 0, time.UTC),  // bucket 08:00
		time.Date(2026, 7, 20, 9, 15, 0, 0, time.UTC),  // bucket 09:00
		time.Date(2026, 7, 20, 9, 45, 0, 0, time.UTC),  // bucket 09:00
		time.Date(2026, 7, 20, 9, 59, 0, 0, time.UTC),  // bucket 09:00
		time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC), // bucket 10:00
	}
	a := []float64{1, 2, 4, 5, 6, 10}
	b := []float64{10, 20, 40, 50, 60, 100}
	df := timeSeriesFrame(t, ts, a, b)

	r, err := df.ResampleEvery("t", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Agg(
		Aggregation{Column: "a", Kind: AggSum, Alias: "a_sum"},
		Aggregation{Column: "b", Kind: AggMean, Alias: "b_mean"},
		Aggregation{Kind: AggCount, Alias: "count"},
	)
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := out.Shape()
	if rows != 3 || cols != 4 {
		t.Fatalf("shape (%d, %d), want (3, 4)", rows, cols)
	}

	// Verify bucket-start timestamps: 08:00, 09:00, 10:00.
	tsCol, _ := out.Column("t")
	tsArr := tsCol.col.Data().Chunks()[0].(*array.Timestamp)
	wantStarts := []time.Time{
		time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
	}
	for i, w := range wantStarts {
		got := time.Unix(0, int64(tsArr.Value(i))).UTC()
		if !got.Equal(w) {
			t.Errorf("bucket %d start = %v, want %v", i, got, w)
		}
	}

	// Verify a_sum: bucket 0 = 1+2=3, bucket 1 = 4+5+6=15, bucket 2 = 10.
	aSum, _ := out.Column("a_sum")
	aArr := aSum.col.Data().Chunks()[0].(*array.Float64)
	if aArr.Value(0) != 3 || aArr.Value(1) != 15 || aArr.Value(2) != 10 {
		t.Fatalf("a_sum: %v %v %v", aArr.Value(0), aArr.Value(1), aArr.Value(2))
	}
	// Verify b_mean for bucket 1: (40+50+60)/3 = 50.
	bMean, _ := out.Column("b_mean")
	bArr := bMean.col.Data().Chunks()[0].(*array.Float64)
	if bArr.Value(1) != 50 {
		t.Fatalf("b_mean[1] = %v, want 50", bArr.Value(1))
	}
	// Verify count.
	countCol, _ := out.Column("count")
	countArr := countCol.col.Data().Chunks()[0].(*array.Int64)
	if countArr.Value(0) != 2 || countArr.Value(1) != 3 || countArr.Value(2) != 1 {
		t.Fatalf("counts: %v %v %v", countArr.Value(0), countArr.Value(1), countArr.Value(2))
	}
}

func TestResampleEvery_EmptyBucketsExcluded(t *testing.T) {
	// Two rows with a large gap. Only the two buckets that contain rows
	// should appear in the output.
	ts := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC),
	}
	df := timeSeriesFrame(t, ts, []float64{1, 1}, []float64{2, 2})
	r, _ := df.ResampleEvery("t", time.Hour)
	out, err := r.Agg(Aggregation{Column: "a", Kind: AggSum})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("rows = %d, want 2", r)
	}
}

func TestResampleEvery_ZeroIntervalErrors(t *testing.T) {
	df := timeSeriesFrame(t, []time.Time{time.Now()}, []float64{1}, []float64{2})
	if _, err := df.ResampleEvery("t", 0); err == nil {
		t.Fatal("expected error for zero interval")
	}
}

func TestResampleEvery_NonDateTimeErrors(t *testing.T) {
	df := smallFrame(t) // this fixture has no timestamp columns
	_, err := df.ResampleEvery("pop", time.Hour)
	if !errors.Is(err, ErrNotDateTime) {
		t.Fatalf("want ErrNotDateTime, got %v", err)
	}
}

func TestResampleEvery_TimezoneMetadataPreserved(t *testing.T) {
	tzOrSkip(t, "America/New_York")
	ts := []time.Time{
		time.Date(2026, 7, 20, 8, 5, 0, 0, time.UTC),
		time.Date(2026, 7, 20, 8, 55, 0, 0, time.UTC),
	}
	df := timeSeriesFrame(t, ts, []float64{1, 2}, []float64{3, 4})
	tCol, _ := df.Column("t")
	tNY, err := tCol.WithTimezone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild frame swapping in the tz-aware column.
	fields := []arrow.Field{tNY.field, df.series[1].field, df.series[2].field}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{*tNY.col, *df.series[1].col, *df.series[2].col}
	dfNY, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := dfNY.ResampleEvery("t", time.Hour)
	out, err := r.Agg(Aggregation{Column: "a", Kind: AggSum})
	if err != nil {
		t.Fatal(err)
	}
	outT, _ := out.Column("t")
	if outT.Timezone() != "America/New_York" {
		t.Fatalf("output timestamp tz = %q, want America/New_York", outT.Timezone())
	}
}

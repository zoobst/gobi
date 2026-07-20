package gobi

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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

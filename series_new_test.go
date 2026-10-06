package gobi

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestNewTypedSeries(t *testing.T) {
	valid := []bool{true, false, true}
	wantNulls := []bool{false, true, false}

	s := NewStringSeries("id", []string{"00123", "ignored", ""}, valid)
	if got, err := s.Strings(); err != nil || !slices.Equal(got, []string{"00123", "", ""}) {
		t.Errorf("Strings = %q, %v", got, err)
	}
	if !slices.Equal(s.Nulls(), wantNulls) || s.Name() != "id" {
		t.Errorf("string: name %q nulls %v", s.Name(), s.Nulls())
	}

	f := NewFloat64Series("x", []float64{1.5, 9, -2}, valid)
	if got, err := f.Float64s(); err != nil || !slices.Equal(got, []float64{1.5, 0, -2}) {
		t.Errorf("Float64s = %v, %v", got, err)
	}
	if !slices.Equal(f.Nulls(), wantNulls) {
		t.Errorf("float nulls %v", f.Nulls())
	}

	i := NewInt64Series("n", []int64{1 << 60, 7, -3}, nil)
	if got, err := i.Int64s(); err != nil || !slices.Equal(got, []int64{1 << 60, 7, -3}) {
		t.Errorf("Int64s = %v, %v", got, err)
	}
	if i.NullCount() != 0 {
		t.Errorf("nil validity: %d nulls", i.NullCount())
	}

	b := NewBoolSeries("ok", []bool{true, true, false}, valid)
	if got, err := b.Bools(); err != nil || !slices.Equal(got, []bool{true, false, false}) {
		t.Errorf("Bools = %v, %v", got, err)
	}
	if !slices.Equal(b.Nulls(), wantNulls) {
		t.Errorf("bool nulls %v", b.Nulls())
	}

	if e := NewStringSeries("e", nil, nil); e.Len() != 0 || e.DataType().ID() != arrow.STRING {
		t.Errorf("empty: len %d type %s", e.Len(), e.DataType())
	}
}

func TestNewTypedSeries_ValidityLenPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "validity has 1 entries, values have 2") {
			t.Errorf("recover = %v", r)
		}
	}()
	NewInt64Series("n", []int64{1, 2}, []bool{true})
}

func TestNewTimestampSeries_ValidityLenPanics(t *testing.T) {
	defer func() {
		if r, _ := recover().(string); !strings.Contains(r, "NewTimestampSeries: validity has 1 entries, values have 2") {
			t.Errorf("recover = %q", r)
		}
	}()
	NewTimestampSeries("t", []time.Time{{}, {}}, []bool{true})
}

func TestNewTimestampSeriesUnit(t *testing.T) {
	far := time.Date(3000, 6, 1, 12, 0, 0, 123456789, time.UTC)
	for unit, want := range map[arrow.TimeUnit]int64{
		arrow.Second:      far.Unix(),
		arrow.Millisecond: far.UnixMilli(),
		arrow.Microsecond: far.UnixMicro(),
	} {
		s, err := NewTimestampSeriesUnit("t", []time.Time{far}, nil, unit)
		if err != nil {
			t.Fatalf("%s: %v", unit, err)
		}
		if tt := s.DataType().(*arrow.TimestampType); tt.Unit != unit || tt.TimeZone != "UTC" {
			t.Errorf("%s: type %s", unit, tt)
		}
		if got, _ := s.Timestamps(); got[0] != arrow.Timestamp(want) {
			t.Errorf("%s: got %d, want %d", unit, got[0], want)
		}
	}

	// Nanoseconds can't hold year 3000: error, not a wrapped value.
	if _, err := NewTimestampSeriesUnit("t", []time.Time{far}, nil, arrow.Nanosecond); err == nil ||
		!strings.Contains(err.Error(), "row 0") {
		t.Errorf("ns overflow err = %v", err)
	}
	// ...unless that row is null.
	s, err := NewTimestampSeriesUnit("t", []time.Time{far, time.Unix(0, 5)}, []bool{false, true}, arrow.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Timestamps(); got[1] != 5 || !s.Nulls()[0] {
		t.Errorf("null overflow row: got %v nulls %v", got, s.Nulls())
	}

	// Sub-unit precision floors toward the past, before 1970 too.
	pre := time.Unix(-1, 999_500_000) // 1969-12-31T23:59:59.9995Z
	s, err = NewTimestampSeriesUnit("t", []time.Time{pre}, nil, arrow.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Timestamps(); got[0] != -1 {
		t.Errorf("floor: got %d ms, want -1", got[0])
	}

	if _, err := NewTimestampSeriesUnit("t", nil, nil, arrow.TimeUnit(9)); err == nil {
		t.Error("unknown unit: no error")
	}
}

func TestNewFrameFromSeries(t *testing.T) {
	id := NewStringSeries("id", []string{"a", "b"}, nil)
	x := NewFloat64Series("x", []float64{1, 2}, []bool{true, false})
	f, err := NewFrameFromSeries(id, x)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.ColumnNames(), []string{"id", "x"}) || f.NumRows() != 2 {
		t.Fatalf("frame %v rows %d", f.ColumnNames(), f.NumRows())
	}
	got, _ := f.Column("x")
	if !slices.Equal(got.Nulls(), []bool{false, true}) {
		t.Errorf("x nulls %v", got.Nulls())
	}
	f.Release()
	// The Frame held its own refs: the caller's Series outlive it.
	if v, err := x.Float64s(); err != nil || v[0] != 1 {
		t.Errorf("after Release: %v, %v", v, err)
	}

	// Field-level attributes (here, metadata) carry over.
	md := arrow.NewMetadata([]string{"k"}, []string{"v"})
	ab := array.NewInt64Builder(memory.DefaultAllocator)
	ab.AppendValues([]int64{1, 2}, nil)
	tagged := SeriesFromArray(arrow.Field{Name: "tag", Type: arrow.PrimitiveTypes.Int64, Metadata: md}, ab.NewArray())
	ab.Release()
	f, err = NewFrameFromSeries(tagged)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Schema().Field(0).Metadata.GetValue("k"); v != "v" {
		t.Errorf("field metadata lost: %v", f.Schema().Field(0).Metadata)
	}
	f.Release()

	if _, err := NewFrameFromSeries(id, NewInt64Series("n", []int64{1}, nil)); !errors.Is(err, ErrColumnLenMismatch) {
		t.Errorf("length mismatch err = %v", err)
	}
	if _, err := NewFrameFromSeries(id, id); !errors.Is(err, ErrDuplicateColumn) {
		t.Errorf("duplicate err = %v", err)
	}
	if _, err := NewFrameFromSeries(id, Series{}); err == nil || errors.Is(err, ErrColumnLenMismatch) || !strings.Contains(err.Error(), "zero Series") {
		t.Errorf("zero Series err = %v", err)
	}
	empty, err := NewFrameFromSeries()
	if err != nil || empty.NumCols() != 0 {
		t.Errorf("no series: %v, %v", empty, err)
	}
}

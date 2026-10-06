package gobi

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// chunkedSeries wraps arrs (one chunk each) in a Series; it takes
// over the caller's refs on arrs.
func chunkedSeries(name string, arrs ...arrow.Array) Series {
	chunked := arrow.NewChunked(arrs[0].DataType(), arrs)
	for _, a := range arrs {
		a.Release()
	}
	col := arrow.NewColumn(arrow.Field{Name: name, Type: arrs[0].DataType(), Nullable: true}, chunked)
	chunked.Release()
	return NewSeries(col)
}

func stringDict(t *testing.T, vals ...string) Series {
	t.Helper()
	dt := &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}
	b := array.NewDictionaryBuilder(memory.DefaultAllocator, dt).(*array.BinaryDictionaryBuilder)
	defer b.Release()
	for _, v := range vals {
		if err := b.AppendString(v); err != nil {
			t.Fatal(err)
		}
	}
	return chunkedSeries("d", b.NewArray())
}

func TestAsFloat64s(t *testing.T) {
	ib := array.NewInt64Builder(memory.DefaultAllocator)
	ib.AppendValues([]int64{1, 2}, nil)
	first := ib.NewArray()
	ib.AppendNull()
	ib.Append(-4)
	second := ib.NewArray()
	ib.Release()

	for name, tc := range map[string]struct {
		s         Series
		want      []float64
		wantNulls []bool
	}{
		"multi-chunk int": {chunkedSeries("i", first, second), []float64{1, 2, 0, -4}, []bool{false, false, true, false}},
		"clean strings":   {NewStringSeries("s", []string{"1.5", "2", "-1e3"}, nil), []float64{1.5, 2, -1000}, []bool{false, false, false}},
		// Arrow's cast fails on these, so this is the row-by-row path.
		"dirty strings": {
			NewStringSeries("s", []string{" 3 ", "n/a", "", "x", "1e400", "1,234"}, []bool{true, true, true, false, true, true}),
			[]float64{3, 0, 0, 0, math.Inf(1), 0}, []bool{false, true, true, true, false, true},
		},
		"bool":        {NewBoolSeries("b", []bool{true, false}, nil), []float64{1, 0}, []bool{false, false}},
		"uint64 max":  {chunkedSeries("u", uint64Array(math.MaxUint64)), []float64{math.MaxUint64}, []bool{false}},
		"dictionary":  {stringDict(t, "1", "x", "1"), []float64{1, 0, 1}, []bool{false, true, false}},
		"empty":       {NewFloat64Series("f", nil, nil), []float64{}, []bool{}},
		"float nulls": {NewFloat64Series("f", []float64{9, 2}, []bool{false, true}), []float64{0, 2}, []bool{true, false}},
	} {
		got, nulls, err := tc.s.AsFloat64s()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !slices.Equal(got, tc.want) || !slices.Equal(nulls, tc.wantNulls) {
			t.Errorf("%s: got %v nulls %v, want %v nulls %v", name, got, nulls, tc.want, tc.wantNulls)
		}
	}

	ts := NewTimestampSeries("t", []time.Time{time.Unix(0, 0)}, nil)
	if _, _, err := ts.AsFloat64s(); !errors.Is(err, ErrColumnTypeMismatch) {
		t.Errorf("timestamp: err = %v", err)
	}
}

func float32Array(vals ...float32) arrow.Array {
	b := array.NewFloat32Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, nil)
	return b.NewArray()
}

func uint64Array(vals ...uint64) arrow.Array {
	b := array.NewUint64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, nil)
	return b.NewArray()
}

func TestAsStrings(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	tsUTC := time.Date(2024, 1, 2, 3, 4, 5, 600, time.UTC)
	tz, err := NewTimestampSeries("t", []time.Time{tsUTC}, nil).WithTimezone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	gb.Append(geometry.WKB(geometry.NewPoint(1, 2.5, geometry.CRS{})))
	gb.AppendNull()
	geom := SeriesFromArray(GeometryField("g", 4326), gb.NewArray())
	gb.Release()

	bb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	bb.Append([]byte("hi"))
	bin := chunkedSeries("b", bb.NewArray())
	bb.Release()

	db := array.NewDate32Builder(memory.DefaultAllocator)
	db.Append(arrow.Date32FromTime(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)))
	date := chunkedSeries("d", db.NewArray())
	db.Release()

	lb := array.NewListBuilder(memory.DefaultAllocator, arrow.PrimitiveTypes.Int64)
	lb.Append(true)
	lb.ValueBuilder().(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
	list := chunkedSeries("l", lb.NewArray())
	lb.Release()

	for name, tc := range map[string]struct {
		s         Series
		want      []string
		wantNulls []bool
	}{
		"float": {
			NewFloat64Series("f", []float64{0.1, 1e21, 3, 366999001, 123456789012345, 1e-7, 1e-8, -2.5e20, 0, math.NaN()}, []bool{true, true, false, true, true, true, true, true, true, true}),
			[]string{"0.1", "1e+21", "", "366999001", "123456789012345", "0.0000001", "1e-08", "-250000000000000000000", "0", "NaN"},
			[]bool{false, false, true, false, false, false, false, false, false, false},
		},
		"float32":    {chunkedSeries("f", float32Array(16777216, 0.1)), []string{"16777216", "0.1"}, []bool{false, false}},
		"int":        {NewInt64Series("i", []int64{-7, 1 << 62}, nil), []string{"-7", "4611686018427387904"}, []bool{false, false}},
		"uint64":     {chunkedSeries("u", uint64Array(math.MaxUint64)), []string{"18446744073709551615"}, []bool{false}},
		"bool":       {NewBoolSeries("b", []bool{true, false}, nil), []string{"true", "false"}, []bool{false, false}},
		"string":     {NewStringSeries("s", []string{" 00123 "}, nil), []string{" 00123 "}, []bool{false}},
		"dictionary": {stringDict(t, "a", "b", "a"), []string{"a", "b", "a"}, []bool{false, false, false}},
		"date":       {date, []string{"2024-01-02"}, []bool{false}},
		"timestamp":  {tz, []string{tsUTC.In(ny).Format(time.RFC3339Nano)}, []bool{false}},
		"utc ts":     {NewTimestampSeries("t", []time.Time{tsUTC}, nil), []string{"2024-01-02T03:04:05.0000006Z"}, []bool{false}},
		"geometry":   {geom, []string{"POINT (1 2.5)", ""}, []bool{false, true}},
		"binary":     {bin, []string{"aGk="}, []bool{false}},
		"list":       {list, []string{"[1,2]"}, []bool{false}},
	} {
		got, nulls, err := tc.s.AsStrings()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !slices.Equal(got, tc.want) || !slices.Equal(nulls, tc.wantNulls) {
			t.Errorf("%s: got %q nulls %v, want %q nulls %v", name, got, nulls, tc.want, tc.wantNulls)
		}
	}
}

func TestAsTimes(t *testing.T) {
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	s := NewStringSeries("s", []string{
		"2024-01-02T03:04:05Z",
		"2024-01-02T05:04:05.000+02:00",
		" 2024-01-02 03:04:05 ",
		"2024-01-02T03:04:05",
		"2024-01-02",
		"02/01/2024",
		"",
	}, nil)
	got, nulls, err := s.AsTimes()
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for i, w := range []time.Time{want, want, want, want, day, {}, {}} {
		if !got[i].Equal(w) {
			t.Errorf("row %d: got %v, want %v", i, got[i], w)
		}
	}
	if !slices.Equal(nulls, []bool{false, false, false, false, false, true, true}) {
		t.Errorf("nulls %v", nulls)
	}

	// Custom layouts replace the defaults.
	got, nulls, err = s.AsTimes("02/01/2006")
	if err != nil {
		t.Fatal(err)
	}
	if !got[5].Equal(day) || !nulls[0] {
		t.Errorf("custom layout: got %v nulls %v", got[5], nulls)
	}

	// Timestamp columns keep their instant and zone; units other than ns.
	ms, err := NewTimestampSeriesUnit("t", []time.Time{want}, nil, arrow.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ms, err = ms.WithTimezone("Asia/Tokyo")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	got, _, err = ms.AsTimes()
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Equal(want) || got[0].Location().String() != "Asia/Tokyo" {
		t.Errorf("timestamp: got %v", got[0])
	}

	if _, _, err := NewInt64Series("n", []int64{1}, nil).AsTimes(); !errors.Is(err, ErrColumnTypeMismatch) {
		t.Errorf("int64: err = %v", err)
	}
}

// BenchmarkAsFloat64s — 1M rows. Int64 and clean strings take
// compute.CastArray; one bad string sends the whole column row by row.
func BenchmarkAsFloat64s(b *testing.B) {
	const n = 1 << 20
	ints := make([]int64, n)
	strs := make([]string, n)
	for i := range n {
		ints[i] = int64(i)
		strs[i] = "12.5"
	}
	dirty := slices.Clone(strs)
	dirty[n/2] = "n/a"
	for name, s := range map[string]Series{
		"int64":         NewInt64Series("i", ints, nil),
		"clean_strings": NewStringSeries("s", strs, nil),
		"dirty_strings": NewStringSeries("s", dirty, nil),
	} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				if _, _, err := s.AsFloat64s(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestAs_DictionaryTimestampZone — a dictionary-encoded timestamp
// column keeps its zone through AsTimes / AsStrings.
func TestAs_DictionaryTimestampZone(t *testing.T) {
	if _, err := time.LoadLocation("America/New_York"); err != nil {
		t.Skip("no tzdata:", err)
	}
	vt := &arrow.TimestampType{Unit: arrow.Second, TimeZone: "America/New_York"}
	dt := &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: vt}
	b := array.NewDictionaryBuilder(memory.DefaultAllocator, dt).(*array.TimestampDictionaryBuilder)
	instant := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	if err := b.Append(arrow.Timestamp(instant.Unix())); err != nil {
		t.Fatal(err)
	}
	s := chunkedSeries("t", b.NewArray())
	b.Release()

	times, _, err := s.AsTimes()
	if err != nil {
		t.Fatal(err)
	}
	if !times[0].Equal(instant) || times[0].Location().String() != "America/New_York" {
		t.Errorf("AsTimes = %v", times[0])
	}
	strs, _, err := s.AsStrings()
	if err != nil {
		t.Fatal(err)
	}
	if strs[0] != "2024-01-02T07:00:00-05:00" {
		t.Errorf("AsStrings = %q", strs[0])
	}
}

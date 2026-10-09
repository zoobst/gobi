package gobi

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi/geometry"
)

// smallFrame returns a 5-row frame: (name string, pop int64, geom Binary WKB).
func smallFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	names := array.NewStringBuilder(pool)
	defer names.Release()
	names.AppendValues([]string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}, nil)
	pops := array.NewInt64Builder(pool)
	defer pops.Release()
	pops.AppendValues([]int64{10, 20, 30, 40, 50}, nil)
	geoms := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geoms.Release()
	for range 5 {
		geoms.Append([]byte{0x01, 0x01, 0x00, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	}
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "pop", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		GeometryField("geom", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{names.NewArray(), pops.NewArray(), geoms.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 3)
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFrame_Filter_Basic(t *testing.T) {
	f := smallFrame(t)
	pops, _ := f.Column("pop")
	mask, _ := pops.GtScalar(20)
	out, err := f.Filter(mask)
	if err != nil {
		t.Fatal(err)
	}
	if r, c := out.Shape(); r != 3 || c != 3 {
		t.Fatalf("shape: (%d, %d) want (3, 3)", r, c)
	}
	// Verify names preserved: Charlie, Delta, Echo
	names, _ := out.Column("name")
	arr := names.col.Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "Charlie" || arr.Value(1) != "Delta" || arr.Value(2) != "Echo" {
		t.Fatalf("names = %v", []string{arr.Value(0), arr.Value(1), arr.Value(2)})
	}
}

func TestFrame_Filter_NullMask(t *testing.T) {
	f := smallFrame(t)
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues([]bool{true, true, true, true, true}, []bool{true, false, true, true, true})
	mask := newSeriesFromArray("m", b.NewArray())

	out, err := f.Filter(mask)
	if err != nil {
		t.Fatal(err)
	}
	// Null mask entry (row 1) treated as false → 4 rows kept.
	if r, _ := out.Shape(); r != 4 {
		t.Fatalf("rows = %d, want 4", r)
	}
}

func TestFrame_Filter_LengthMismatch(t *testing.T) {
	f := smallFrame(t)
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues([]bool{true, false}, nil)
	mask := newSeriesFromArray("m", b.NewArray())
	_, err := f.Filter(mask)
	if !errors.Is(err, ErrColumnLenMismatch) {
		t.Fatalf("want ErrColumnLenMismatch, got %v", err)
	}
}

func TestFrame_Filter_NonBoolean(t *testing.T) {
	f := smallFrame(t)
	pops, _ := f.Column("pop")
	_, err := f.Filter(pops)
	if !errors.Is(err, ErrMaskNotBoolean) {
		t.Fatalf("want ErrMaskNotBoolean, got %v", err)
	}
}

func TestFrame_Take_OrderAndDuplicates(t *testing.T) {
	f := smallFrame(t)
	out, err := f.Take([]int{4, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := out.Shape(); r != 3 {
		t.Fatalf("rows = %d", r)
	}
	names, _ := out.Column("name")
	arr := names.col.Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "Echo" || arr.Value(1) != "Charlie" || arr.Value(2) != "Echo" {
		t.Fatalf("names: %v", []string{arr.Value(0), arr.Value(1), arr.Value(2)})
	}
}

func TestFrame_Take_OutOfRange(t *testing.T) {
	f := smallFrame(t)
	_, err := f.Take([]int{0, 99})
	if !errors.Is(err, ErrRowOutOfRange) {
		t.Fatalf("want ErrRowOutOfRange, got %v", err)
	}
}

// zonedTS builds a timestamp[ns, tz=UTC] column of the given seconds,
// one chunk per group, so it takes the multi-chunk path.
func zonedTS(name string, groups ...[]int64) Series {
	dt := &arrow.TimestampType{Unit: arrow.Nanosecond, TimeZone: "UTC"}
	arrs := make([]arrow.Array, len(groups))
	for i, g := range groups {
		b := array.NewTimestampBuilder(memory.DefaultAllocator, dt)
		for _, sec := range g {
			b.Append(arrow.Timestamp(sec * 1e9))
		}
		arrs[i] = b.NewArray()
		b.Release()
	}
	return chunkedSeries(name, arrs...)
}

func unixSeconds(t *testing.T, s Series) []int64 {
	t.Helper()
	times, nulls, err := s.AsTimes()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, len(times))
	for i, tm := range times {
		out[i] = -1
		if !nulls[i] {
			out[i] = tm.Unix()
		}
	}
	return out
}

func TestTake_MultiChunkZonedTimestamp(t *testing.T) {
	f, err := NewFrameFromSeries(zonedTS("t", []int64{10, 20}, []int64{30}))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	got, err := f.Take([]int{2, 0, 2})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Release()
	s, _ := got.Column("t")
	if tt := s.DataType().(*arrow.TimestampType); tt.TimeZone != "UTC" || tt.Unit != arrow.Nanosecond {
		t.Errorf("type %s", tt)
	}
	if v := unixSeconds(t, s); !slices.Equal(v, []int64{30, 10, 30}) {
		t.Errorf("values %v", v)
	}
	if _, err := f.Take([]int{3}); err == nil {
		t.Error("out of range: no error")
	}
}

func TestJoin_LeftWithZonedTimestamp(t *testing.T) {
	left, _ := NewFrameFromSeries(NewInt64Series("id", []int64{1, 2, 3}, nil))
	defer left.Release()
	right, _ := NewFrameFromSeries(
		NewInt64Series("id", []int64{3, 1}, nil),
		zonedTS("t", []int64{300}, []int64{100}),
	)
	defer right.Release()
	j, err := left.Join(right, "id", "id", JoinLeft)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Release()
	s, err := j.Column("t")
	if err != nil {
		t.Fatal(err)
	}
	if v := unixSeconds(t, s); !slices.Equal(v, []int64{100, -1, 300}) {
		t.Errorf("t = %v (-1 = null)", v)
	}
}

func TestTakeCoalescedKey_Compute(t *testing.T) {
	date := func(days ...int32) Series {
		b := array.NewDate32Builder(memory.DefaultAllocator)
		defer b.Release()
		for _, d := range days {
			b.Append(arrow.Date32(d))
		}
		return chunkedSeries("d", b.NewArray())
	}
	arr, err := takeCoalescedKey(memory.DefaultAllocator, date(1, 2), date(7, 8, 9), []int{1, -1, -1}, []int{-1, 2, -1})
	if err != nil {
		t.Fatal(err)
	}
	defer arr.Release()
	d := arr.(*array.Date32)
	if d.Value(0) != 2 || d.Value(1) != 9 || !d.IsNull(2) {
		t.Errorf("coalesced = %v", d)
	}
}

func TestSortByHilbert_ZonedTimestampColumn(t *testing.T) {
	gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	for _, x := range []float64{50, 0, 25} {
		gb.Append(geometry.WKB(geometry.NewPoint(x, x, geometry.CRS{})))
	}
	geom := SeriesFromArray(GeometryField("geometry", 4326), gb.NewArray())
	gb.Release()
	f, err := NewFrameFromSeries(geom, zonedTS("t", []int64{50}, []int64{0, 25}))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatal(err)
	}
	defer sorted.Release()
	// Rows move together: each timestamp still matches its point's x.
	gs, _ := sorted.Column("geometry")
	ts, _ := sorted.Column("t")
	secs := unixSeconds(t, ts)
	for i := range sorted.NumRows() {
		g, err := gs.Geometry(i)
		if err != nil {
			t.Fatal(err)
		}
		if x := int64(g.(geometry.Point).X); x != secs[i] {
			t.Errorf("row %d: point x %d, timestamp %d", i, x, secs[i])
		}
	}
}

// TestTakeChunks — the per-chunk gather: rows from several chunks in
// any order, nulls, a single referenced chunk, and errors.
func TestTakeChunks(t *testing.T) {
	s := zonedTS("t", []int64{0, 1}, []int64{2}, []int64{3, 4, 5})
	pool := memory.DefaultAllocator
	for name, tc := range map[string]struct {
		idx  []int
		want []int64 // -1 = null
	}{
		"across chunks":      {[]int{5, 0, 2, 3, 1}, []int64{5, 0, 2, 3, 1}},
		"with nulls":         {[]int{-1, 4, -1, 2}, []int64{-1, 4, -1, 2}},
		"one chunk only":     {[]int{4, 3, 3}, []int64{4, 3, 3}},
		"one chunk and null": {[]int{4, -1}, []int64{4, -1}},
		"all null":           {[]int{-1, -1}, []int64{-1, -1}},
		"empty":              {nil, []int64{}},
	} {
		arr, err := takeArrayWithNulls(pool, s, tc.idx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := unixSeconds(t, chunkedSeries("t", arr))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}

	if _, err := takeArray(pool, s, []int{6}); !errors.Is(err, ErrRowOutOfRange) {
		t.Errorf("out of range: err = %v", err)
	}
	if _, err := takeArray(pool, s, []int{-1}); !errors.Is(err, ErrRowOutOfRange) {
		t.Errorf("negative without nulls: err = %v", err)
	}
}

// TestTakeCoalescedKey_TypeMismatch — same type ID, different
// parameters: an error, not an arrow NewChunked panic.
func TestTakeCoalescedKey_TypeMismatch(t *testing.T) {
	a := zonedTS("t", []int64{1})
	b := NewTimestampSeries("t", []time.Time{time.Unix(2, 0)}, nil) // timestamp[ns], no zone
	if _, err := takeCoalescedKey(memory.DefaultAllocator, a, b, []int{-1}, []int{0}); !errors.Is(err, ErrColumnTypeMismatch) {
		t.Errorf("err = %v", err)
	}
}

// TestJoin_TimestampKeysMustMatchExactly — keys sharing the TIMESTAMP
// ID but not unit or zone are a type error up front, not raw int64s
// of different scales hashed and coalesced together.
func TestJoin_TimestampKeysMustMatchExactly(t *testing.T) {
	at := []time.Time{time.Unix(1, 0)}
	us, _ := NewTimestampSeriesUnit("t", at, nil, arrow.Microsecond)
	ns, _ := NewTimestampSeriesUnit("t", at, nil, arrow.Nanosecond)
	naive := NewTimestampSeries("t", at, nil)
	for name, pair := range map[string][2]Series{"unit": {us, ns}, "zone": {ns, naive}} {
		l, _ := NewFrameFromSeries(pair[0])
		r, _ := NewFrameFromSeries(pair[1])
		for _, k := range []JoinType{JoinInner, JoinFull, JoinRight} {
			if _, err := l.Join(r, "t", "t", k); !errors.Is(err, ErrColumnTypeMismatch) {
				t.Errorf("%s, kind %d: err = %v", name, k, err)
			}
		}
		l.Release()
		r.Release()
	}
}

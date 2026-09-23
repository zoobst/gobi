package gobi

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

var dictStringType = &arrow.DictionaryType{
	IndexType: arrow.PrimitiveTypes.Int32,
	ValueType: arrow.BinaryTypes.String,
}

// newDictStringArray builds a dictionary<int32, string> array. A nil
// entry in idx is a null index; a nil entry in dict is a null
// dictionary value (reachable through a valid index).
func newDictStringArray(t *testing.T, dict []*string, idx []*int32) arrow.Array {
	t.Helper()
	pool := memory.DefaultAllocator
	db := array.NewStringBuilder(pool)
	defer db.Release()
	for _, v := range dict {
		if v == nil {
			db.AppendNull()
		} else {
			db.Append(*v)
		}
	}
	ib := array.NewInt32Builder(pool)
	defer ib.Release()
	for _, v := range idx {
		if v == nil {
			ib.AppendNull()
		} else {
			ib.Append(*v)
		}
	}
	d := db.NewArray()
	defer d.Release()
	ix := ib.NewArray()
	defer ix.Release()
	return array.NewDictionaryArray(dictStringType, ix, d)
}

func newInt64Array(vals ...int64) arrow.Array {
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, nil)
	return b.NewArray()
}

// frameFromChunks builds a Frame whose columns are the given chunk
// lists. Takes ownership of every chunk (Releases them); the caller
// Releases the Frame.
func frameFromChunks(t *testing.T, names []string, chunks [][]arrow.Array) *Frame {
	t.Helper()
	fields := make([]arrow.Field, len(names))
	cols := make([]arrow.Column, len(names))
	for i, name := range names {
		dt := chunks[i][0].DataType()
		fields[i] = arrow.Field{Name: name, Type: dt, Nullable: true}
		ch := arrow.NewChunked(dt, chunks[i])
		for _, a := range chunks[i] {
			a.Release()
		}
		col := arrow.NewColumn(fields[i], ch)
		ch.Release()
		cols[i] = *col
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func sp(s string) *string { return &s }
func ip(i int32) *int32   { return &i }

// TestToStructs_DictionaryString — Spark / Trino write low-cardinality
// strings as dictionary<int32, string>. Before v0.4.8 every row failed
// with "readScalarAt: unsupported type *array.Dictionary".
func TestToStructs_DictionaryString(t *testing.T) {
	dict := []*string{sp("us-east-1"), sp("eu-west-1"), nil}
	// Two chunks (two row groups), each dictionary-encoded.
	c1 := newDictStringArray(t, dict, []*int32{ip(0), ip(1), nil})
	c2 := newDictStringArray(t, dict, []*int32{ip(1), ip(2), ip(0)})
	f := frameFromChunks(t,
		[]string{"id", "region"},
		[][]arrow.Array{
			{newInt64Array(1, 2, 3), newInt64Array(4, 5, 6)},
			{c1, c2},
		})
	defer f.Release()

	type row struct {
		ID     int64  `csv:"id"`
		Region string `csv:"region"`
	}
	got, err := ToStructs[row](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	want := []row{
		{1, "us-east-1"}, {2, "eu-west-1"}, {3, ""},
		{4, "eu-west-1"}, {5, ""}, {6, "us-east-1"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Pointer field: both a null index (row 2) and a valid index
	// pointing at a null dictionary entry (row 4) must come back nil.
	type prow struct {
		Region *string `csv:"region"`
	}
	pgot, err := ToStructs[prow](f)
	if err != nil {
		t.Fatalf("ToStructs[*string]: %v", err)
	}
	for i, w := range []*string{sp("us-east-1"), sp("eu-west-1"), nil, sp("eu-west-1"), nil, sp("us-east-1")} {
		g := pgot[i].Region
		switch {
		case w == nil && g != nil:
			t.Errorf("row %d = %q, want nil", i, *g)
		case w != nil && (g == nil || *g != *w):
			t.Errorf("row %d = %v, want %q", i, g, *w)
		}
	}
}

// TestToStructs_DictionaryListElements — list<dictionary<int32,
// string>> resolves element values through the dictionary.
func TestToStructs_DictionaryListElements(t *testing.T) {
	pool := memory.DefaultAllocator
	vals := newDictStringArray(t, []*string{sp("a"), sp("b")}, []*int32{ip(1), ip(0), ip(1)})
	defer vals.Release()
	ob := array.NewInt32Builder(pool)
	defer ob.Release()
	ob.AppendValues([]int32{0, 2, 3}, nil)
	offs := ob.NewArray()
	defer offs.Release()
	lt := arrow.ListOf(dictStringType)
	data := array.NewData(lt, 2, []*memory.Buffer{nil, offs.Data().Buffers()[1]},
		[]arrow.ArrayData{vals.Data()}, 0, 0)
	list := array.NewListData(data)
	data.Release()

	f := frameFromChunks(t, []string{"tags"}, [][]arrow.Array{{list}})
	defer f.Release()

	type row struct {
		Tags []string `csv:"tags"`
	}
	got, err := ToStructs[row](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	if len(got) != 2 || len(got[0].Tags) != 2 || got[0].Tags[0] != "b" || got[0].Tags[1] != "a" ||
		len(got[1].Tags) != 1 || got[1].Tags[0] != "b" {
		t.Errorf("got %+v, want [{[b a]} {[b]}]", got)
	}
}

func TestReadScalarAt_Dictionary(t *testing.T) {
	arr := newDictStringArray(t, []*string{sp("x"), nil}, []*int32{ip(0), nil, ip(1)})
	f := frameFromChunks(t, []string{"s"}, [][]arrow.Array{{arr}})
	defer f.Release()
	s, _ := f.Column("s")

	if v, err := readScalarAt(s, 0); err != nil || v != "x" {
		t.Errorf("row 0 = (%v, %v), want (x, nil)", v, err)
	}
	for _, r := range []int{1, 2} {
		if v, err := readScalarAt(s, r); err != nil || v != nil {
			t.Errorf("row %d = (%v, %v), want (nil, nil)", r, v, err)
		}
	}
}

// TestChunkCursor_Locate — sequential, backward, and random access
// across chunks including zero-length ones, plus range errors.
func TestChunkCursor_Locate(t *testing.T) {
	f := frameFromChunks(t, []string{"v"}, [][]arrow.Array{{
		newInt64Array(),        // empty leading chunk
		newInt64Array(0, 1, 2), // rows 0-2
		newInt64Array(),        // empty middle chunk
		newInt64Array(),        // two in a row
		newInt64Array(3),       // row 3
		newInt64Array(4, 5),    // rows 4-5
		newInt64Array(),        // empty trailing chunk
	}})
	defer f.Release()
	s, _ := f.Column("v")
	cur := newChunkCursor(s)
	order := []int{0, 1, 2, 3, 4, 5, 5, 3, 0, 4, 2, 1}
	for _, r := range order {
		v, err := cur.scalarAt(r)
		if err != nil {
			t.Fatalf("row %d: %v", r, err)
		}
		if v != int64(r) {
			t.Errorf("row %d = %v", r, v)
		}
	}
	for _, r := range []int{-1, 6, 100} {
		if _, _, err := cur.locate(r); err == nil {
			t.Errorf("locate(%d): want out-of-range error", r)
		}
	}
}

func TestChunkCursor_EmptyColumn(t *testing.T) {
	f := frameFromChunks(t, []string{"v"}, [][]arrow.Array{{newInt64Array()}})
	defer f.Release()
	s, _ := f.Column("v")
	cur := newChunkCursor(s)
	if _, _, err := cur.locate(0); err == nil {
		t.Error("locate(0) on empty column: want error")
	}
}

// TestToStructs_TimestampUnits — Spark / Trino parquet is typically
// TIMESTAMP_MICROS; the reader must honor the column's unit rather
// than assume nanoseconds.
func TestToStructs_TimestampUnits(t *testing.T) {
	want := time.Date(2024, 3, 15, 9, 30, 0, 123456000, time.UTC)
	for _, unit := range []arrow.TimeUnit{arrow.Second, arrow.Millisecond, arrow.Microsecond, arrow.Nanosecond} {
		tt := &arrow.TimestampType{Unit: unit}
		tb := array.NewTimestampBuilder(memory.DefaultAllocator, tt)
		ts, err := arrow.TimestampFromTime(want, unit)
		if err != nil {
			t.Fatal(err)
		}
		tb.Append(ts)
		scalar := tb.NewArray()
		tb.Release()

		// Same value as a one-element list<timestamp>.
		lb := array.NewListBuilder(memory.DefaultAllocator, tt)
		lb.Append(true)
		lb.ValueBuilder().(*array.TimestampBuilder).Append(ts)
		list := lb.NewArray()
		lb.Release()

		f := frameFromChunks(t, []string{"ts", "tss"}, [][]arrow.Array{{scalar}, {list}})
		type row struct {
			TS  time.Time   `csv:"ts"`
			TSS []time.Time `csv:"tss"`
		}
		got, err := ToStructs[row](f)
		f.Release()
		if err != nil {
			t.Fatalf("%s: %v", unit, err)
		}
		exp := want.Truncate(unit.Multiplier())
		if !got[0].TS.Equal(exp) {
			t.Errorf("%s: TS = %v, want %v", unit, got[0].TS, exp)
		}
		if len(got[0].TSS) != 1 || !got[0].TSS[0].Equal(exp) {
			t.Errorf("%s: TSS = %v, want [%v]", unit, got[0].TSS, exp)
		}
	}
}

// TestToStructs_LargeListAndLargeBinary — writers that emit 64-bit
// offsets (LargeList, LargeBinary) read the same as their 32-bit
// counterparts.
func TestToStructs_LargeListAndLargeBinary(t *testing.T) {
	pool := memory.DefaultAllocator
	llb := array.NewLargeListBuilder(pool, arrow.PrimitiveTypes.Int64)
	vb := llb.ValueBuilder().(*array.Int64Builder)
	llb.Append(true)
	vb.AppendValues([]int64{1, 2}, nil)
	llb.Append(true)
	vb.Append(3)
	ll := llb.NewArray()
	llb.Release()

	wkb := geometry.WKB(geometry.Point{X: 1, Y: 2})
	bb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.LargeBinary)
	bb.Append(wkb)
	bb.AppendNull()
	lbin := bb.NewArray()
	bb.Release()

	f := frameFromChunks(t, []string{"xs", "geom"}, [][]arrow.Array{{ll}, {lbin}})
	defer f.Release()
	type row struct {
		XS   []int64 `csv:"xs"`
		Geom []byte  `csv:"geom" geom:"true"`
	}
	got, err := ToStructs[row](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	if len(got[0].XS) != 2 || got[0].XS[0] != 1 || got[0].XS[1] != 2 || len(got[1].XS) != 1 || got[1].XS[0] != 3 {
		t.Errorf("XS = %v / %v, want [1 2] / [3]", got[0].XS, got[1].XS)
	}
	if string(got[0].Geom) != string(wkb) || got[1].Geom != nil {
		t.Errorf("Geom = %x / %x, want %x / nil", got[0].Geom, got[1].Geom, wkb)
	}
}

// TestToStructs_DictionaryGeometry — a dictionary<int32, binary> WKB
// column, including a valid index at a null dictionary entry.
func TestToStructs_DictionaryGeometry(t *testing.T) {
	pool := memory.DefaultAllocator
	wkb := geometry.WKB(geometry.Point{X: 3, Y: 4})
	db := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	db.Append(wkb)
	db.AppendNull()
	d := db.NewArray()
	db.Release()
	defer d.Release()
	ib := array.NewInt32Builder(pool)
	ib.AppendValues([]int32{0, 1, 0}, nil)
	ix := ib.NewArray()
	ib.Release()
	defer ix.Release()
	dt := &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.Binary}
	arr := array.NewDictionaryArray(dt, ix, d)

	f := frameFromChunks(t, []string{"geom"}, [][]arrow.Array{{arr}})
	defer f.Release()
	type row struct {
		Geom string `csv:"geom" geom:"true"`
	}
	got, err := ToStructs[row](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	if got[0].Geom == "" || got[1].Geom != "" || got[2].Geom != got[0].Geom {
		t.Errorf("Geom = %q, want [WKT, \"\", WKT]", []string{got[0].Geom, got[1].Geom, got[2].Geom})
	}
}

// TestIsNullAtSeries_DictionaryNullEntry — isNullAtSeries must agree
// with readScalarAt, or check-then-read callers (First/Last, Pivot
// headers) see "non-null" and then read nil.
func TestIsNullAtSeries_DictionaryNullEntry(t *testing.T) {
	arr := newDictStringArray(t, []*string{sp("x"), nil}, []*int32{ip(0), ip(1), nil})
	f := frameFromChunks(t, []string{"s"}, [][]arrow.Array{{arr}})
	defer f.Release()
	s, _ := f.Column("s")
	for row, want := range []bool{false, true, true} {
		got, err := isNullAtSeries(s, row)
		if err != nil || got != want {
			t.Errorf("row %d = (%v, %v), want (%v, nil)", row, got, err, want)
		}
	}
}

func TestChunkCursor_NilColumn(t *testing.T) {
	cur := newChunkCursor(Series{})
	if _, _, err := cur.locate(0); err == nil {
		t.Error("locate(0) on nil column: want error")
	}
}

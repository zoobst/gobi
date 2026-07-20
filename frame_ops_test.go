package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
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

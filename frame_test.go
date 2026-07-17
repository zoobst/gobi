package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// buildFrame builds a small frame with (name string, pop int64, geometry WKB-Point).
func buildFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.NewGoAllocator()

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "pop", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)

	names := array.NewStringBuilder(pool)
	defer names.Release()
	names.AppendValues([]string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}, nil)

	pops := array.NewInt64Builder(pool)
	defer pops.Release()
	pops.AppendValues([]int64{1, 2, 3, 4, 5}, nil)

	geoms := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geoms.Release()
	for i, x := range []float64{0, 1, 2, 3, 4} {
		wkb := geometry.WKB(geometry.Point{X: x, Y: float64(i * 10)})
		geoms.Append(wkb)
	}

	arrays := []arrow.Array{names.NewArray(), pops.NewArray(), geoms.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()

	cols := make([]arrow.Column, len(fields))
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

func TestFrame_Shape(t *testing.T) {
	f := buildFrame(t)
	rows, cols := f.Shape()
	if rows != 5 || cols != 3 {
		t.Fatalf("shape got (%d, %d) want (5, 3)", rows, cols)
	}
}

func TestFrame_ColumnNames(t *testing.T) {
	f := buildFrame(t)
	names := f.ColumnNames()
	if len(names) != 3 || names[0] != "name" || names[2] != "geometry" {
		t.Fatalf("column names: %v", names)
	}
}

func TestFrame_HeadTail(t *testing.T) {
	f := buildFrame(t)
	head := f.Head(2)
	if head.NumRows() != 2 {
		t.Fatalf("head rows = %d want 2", head.NumRows())
	}
	tail := f.Tail(2)
	if tail.NumRows() != 2 {
		t.Fatalf("tail rows = %d want 2", tail.NumRows())
	}
	// Tail should include the last row's population = 5
	pops, _ := tail.Column("pop")
	pRow, _ := pops.Row(pops.Len() - 1)
	// grab the actual int64 through the chunk
	chunk := pRow.Column().Data().Chunks()[0].(*array.Int64)
	if chunk.Value(0) != 5 {
		t.Fatalf("tail last pop = %d want 5", chunk.Value(0))
	}
}

func TestFrame_HeadDefaultAndOverflow(t *testing.T) {
	f := buildFrame(t)
	if f.Head(0).NumRows() != 5 { // default is 5, table has 5 rows
		t.Fatal("Head(0) default should equal min(5, rows)")
	}
	if f.Head(100).NumRows() != 5 { // clamp to available rows
		t.Fatal("Head(100) should be clamped to available rows")
	}
}

func TestFrame_ColumnNotFound(t *testing.T) {
	f := buildFrame(t)
	_, err := f.Column("nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestFrame_RowOutOfRange(t *testing.T) {
	f := buildFrame(t)
	_, err := f.Row(99)
	if !errors.Is(err, ErrRowOutOfRange) {
		t.Fatalf("want ErrRowOutOfRange, got %v", err)
	}
}

func TestSeries_GeometryDecode(t *testing.T) {
	f := buildFrame(t)
	g, err := f.Geometry("geometry", 2)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("got %T, want Point", g)
	}
	if p.X != 2 || p.Y != 20 {
		t.Fatalf("point = %+v want (2, 20)", p)
	}
}

func TestSeries_NotGeometry(t *testing.T) {
	f := buildFrame(t)
	s, _ := f.Column("name")
	_, err := s.Geometry(0)
	if !errors.Is(err, ErrNotGeometry) {
		t.Fatalf("want ErrNotGeometry, got %v", err)
	}
}

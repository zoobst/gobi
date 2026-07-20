package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// makeUint64Series is a helper: builds a single-chunk Uint64 Series with
// the given values. Useful for simulating an H3-cell UDF result.
func makeUint64Series(t *testing.T, name string, vals []uint64) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewUint64Builder(pool)
	defer b.Release()
	b.AppendValues(vals, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Uint64, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked))
}

func TestFrame_WithColumn_Append(t *testing.T) {
	f := buildFrame(t)
	// H3-style derived column: 5 uint64 cells, one per row.
	h3 := makeUint64Series(t, "h3", []uint64{100, 101, 102, 103, 104})

	out, err := f.WithColumn("h3", h3)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 4 {
		t.Fatalf("cols = %d, want 4", got)
	}
	if got := out.NumRows(); got != 5 {
		t.Fatalf("rows = %d, want 5", got)
	}
	// The original frame is unchanged.
	if f.NumCols() != 3 {
		t.Fatalf("source frame mutated: cols = %d, want 3", f.NumCols())
	}
	// New column is last.
	names := out.ColumnNames()
	if names[len(names)-1] != "h3" {
		t.Fatalf("last col = %q, want h3", names[len(names)-1])
	}
	// Values survived.
	col, err := out.Column("h3")
	if err != nil {
		t.Fatal(err)
	}
	arr := col.Column().Data().Chunks()[0].(*array.Uint64)
	if arr.Value(0) != 100 || arr.Value(4) != 104 {
		t.Fatalf("unexpected h3 values: %d..%d", arr.Value(0), arr.Value(4))
	}
}

func TestFrame_WithColumn_Replace(t *testing.T) {
	f := buildFrame(t)
	// Replace the existing "pop" column with a derived one.
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues([]int64{10, 20, 30, 40, 50}, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "pop", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	newPop := NewSeries(arrow.NewColumn(field, chunked))

	out, err := f.WithColumn("pop", newPop)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 3 {
		t.Fatalf("cols = %d, want 3 (replaced, not appended)", got)
	}
	// The order must be preserved.
	names := out.ColumnNames()
	if names[0] != "name" || names[1] != "pop" || names[2] != "geometry" {
		t.Fatalf("column order broken: %v", names)
	}
	// New values in the "pop" column.
	col, _ := out.Column("pop")
	chunk := col.Column().Data().Chunks()[0].(*array.Int64)
	if chunk.Value(2) != 30 {
		t.Fatalf("row 2 pop = %d, want 30", chunk.Value(2))
	}
}

func TestFrame_WithColumn_LenMismatch(t *testing.T) {
	f := buildFrame(t)
	// Only 3 rows — mismatch with the 5-row frame.
	short := makeUint64Series(t, "h3", []uint64{1, 2, 3})
	if _, err := f.WithColumn("h3", short); !errors.Is(err, ErrColumnLenMismatch) {
		t.Fatalf("want ErrColumnLenMismatch, got %v", err)
	}
}

func TestFrame_DropColumn(t *testing.T) {
	f := buildFrame(t)
	out, err := f.DropColumn("pop")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 2 {
		t.Fatalf("cols = %d, want 2", got)
	}
	names := out.ColumnNames()
	if names[0] != "name" || names[1] != "geometry" {
		t.Fatalf("cols = %v, want [name geometry]", names)
	}
	if _, err := out.Column("pop"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("dropped column still queryable: %v", err)
	}
	// Original untouched.
	if _, err := f.Column("pop"); err != nil {
		t.Fatalf("source frame lost pop: %v", err)
	}
}

func TestFrame_DropColumn_Missing(t *testing.T) {
	f := buildFrame(t)
	if _, err := f.DropColumn("does_not_exist"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestFrame_WithColumn_PreservesSchemaMetadata(t *testing.T) {
	// Build a frame whose schema carries file-level metadata (e.g. a
	// GeoParquet "geo" key). WithColumn must not drop it.
	f := buildFrame(t)
	md := arrow.NewMetadata([]string{"geo"}, []string{`{"primary_column":"geometry"}`})
	f.schema = arrow.NewSchema(f.schema.Fields(), &md)

	h3 := makeUint64Series(t, "h3", []uint64{1, 2, 3, 4, 5})
	out, err := f.WithColumn("h3", h3)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.Schema().Metadata().GetValue("geo")
	if !ok {
		t.Fatal("schema metadata dropped by WithColumn")
	}
	if got != `{"primary_column":"geometry"}` {
		t.Fatalf("metadata mutated: %s", got)
	}
}

package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestListColumn_Construction verifies that a Frame can be
// constructed with a List<String> column and that basic
// operations (Shape, Column access, ColumnAt, ColumnNames)
// work. Serves as the phase-1a smoke test — any explosion here
// tells us what else in the codebase assumes a scalar column.
func TestListColumn_Construction(t *testing.T) {
	pool := memory.DefaultAllocator

	// Build a List<String> column with 3 rows:
	//   row 0: ["a", "b"]
	//   row 1: []          (empty list — non-null)
	//   row 2: null        (list itself is null)
	lb := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer lb.Release()
	sb := lb.ValueBuilder().(*array.StringBuilder)
	// Row 0
	lb.Append(true)
	sb.Append("a")
	sb.Append("b")
	// Row 1 — empty (Append(true) with no values pushed to inner
	// builder = zero-length list at this row).
	lb.Append(true)
	// Row 2 — null.
	lb.AppendNull()
	arr := lb.NewArray()
	defer arr.Release()

	// Also a scalar id column so we can verify the Frame carries
	// both column shapes correctly.
	ib := array.NewInt64Builder(pool)
	defer ib.Release()
	ib.AppendValues([]int64{1, 2, 3}, nil)
	idArr := ib.NewArray()
	defer idArr.Release()

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "tags", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(arr.DataType(), []arrow.Array{arr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if r, c := f.Shape(); r != 3 || c != 2 {
		t.Fatalf("Shape = (%d, %d), want (3, 2)", r, c)
	}
	names := f.ColumnNames()
	if names[0] != "id" || names[1] != "tags" {
		t.Fatalf("column names = %v", names)
	}
	tagsCol, err := f.Column("tags")
	if err != nil {
		t.Fatal(err)
	}
	if tagsCol.DataType().ID() != arrow.LIST {
		t.Errorf("tags column type = %s, want LIST", tagsCol.DataType())
	}
	// The element type should be preserved.
	lt, ok := tagsCol.DataType().(*arrow.ListType)
	if !ok || lt.Elem().ID() != arrow.STRING {
		t.Errorf("tags element type wrong: %v", tagsCol.DataType())
	}
}

// TestListColumn_BuilderForType verifies builderForType (used by
// custom aggregator + FromStructs paths) can construct a List
// builder from a ListType. Regression guard for the phase-1a
// builder-switch update.
func TestListColumn_BuilderForType(t *testing.T) {
	pool := memory.DefaultAllocator
	lt := arrow.ListOf(arrow.PrimitiveTypes.Int64)
	b, err := builderForType(pool, lt)
	if err != nil {
		t.Fatalf("builderForType: %v", err)
	}
	defer b.Release()
	lb, ok := b.(*array.ListBuilder)
	if !ok {
		t.Fatalf("got %T, want *array.ListBuilder", b)
	}
	// Element builder type is what NewListBuilder configured.
	if _, ok := lb.ValueBuilder().(*array.Int64Builder); !ok {
		t.Fatalf("inner builder = %T, want Int64Builder", lb.ValueBuilder())
	}
}

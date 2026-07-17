package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

func stringFrame(t *testing.T, colName, valuesName string, vals []string, extraName string, extra []int64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	kb := array.NewStringBuilder(pool)
	defer kb.Release()
	kb.AppendValues(vals, nil)
	xb := array.NewInt64Builder(pool)
	defer xb.Release()
	xb.AppendValues(extra, nil)

	fields := []arrow.Field{
		{Name: colName, Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: extraName, Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{kb.NewArray(), xb.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
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

func TestJoin_Inner(t *testing.T) {
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c", "d"}, "leftv", []int64{1, 2, 3, 4})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c", "e"}, "rightv", []int64{20, 30, 50})

	out, err := left.Join(right, "id", "id", JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 2 {
		t.Fatalf("inner join rows = %d, want 2", r)
	}
	// left: (id, leftv), right (minus id): (rightv). Total = 3 cols.
	if c != 3 {
		t.Fatalf("cols = %d, want 3", c)
	}

	ids, _ := out.Column("id")
	idsArr := ids.col.Data().Chunks()[0].(*array.String)
	got := []string{idsArr.Value(0), idsArr.Value(1)}
	if got[0] != "b" || got[1] != "c" {
		t.Fatalf("inner-join ids = %v, want [b c]", got)
	}
	rv, _ := out.Column("rightv")
	rvArr := rv.col.Data().Chunks()[0].(*array.Int64)
	if rvArr.Value(0) != 20 || rvArr.Value(1) != 30 {
		t.Fatalf("rightv = %v, %v", rvArr.Value(0), rvArr.Value(1))
	}
}

func TestJoin_Left(t *testing.T) {
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c", "d"}, "leftv", []int64{1, 2, 3, 4})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c", "e"}, "rightv", []int64{20, 30, 50})

	out, err := left.Join(right, "id", "id", JoinLeft)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := out.Shape()
	if r != 4 {
		t.Fatalf("left join rows = %d, want 4", r)
	}
	rv, _ := out.Column("rightv")
	rvArr := rv.col.Data().Chunks()[0].(*array.Int64)
	// Order: a (null), b (20), c (30), d (null)
	if !rvArr.IsNull(0) || rvArr.Value(1) != 20 || rvArr.Value(2) != 30 || !rvArr.IsNull(3) {
		t.Fatalf("left-join rightv incorrect")
	}
}

func TestJoin_ColumnNameCollisionRenames(t *testing.T) {
	// Both frames have a column called "value" — collision should rename right side.
	left := stringFrame(t, "id", "id", []string{"a"}, "value", []int64{1})
	right := stringFrame(t, "id", "id", []string{"a"}, "value", []int64{10})
	out, err := left.Join(right, "id", "id", JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	found := false
	for _, n := range names {
		if n == "value_right" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected value_right column, got %v", names)
	}
}

func TestJoin_KeyTypeMismatch(t *testing.T) {
	left := stringFrame(t, "id", "id", []string{"a"}, "v", []int64{1})
	// Right frame with an int64 "id" key
	pool := memory.DefaultAllocator
	kb := array.NewInt64Builder(pool)
	defer kb.Release()
	kb.AppendValues([]int64{1}, nil)
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	vb.AppendValues([]int64{2}, nil)
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "rv", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{kb.NewArray(), vb.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	right, _ := NewFrame(schema, cols)

	_, err := left.Join(right, "id", "id", JoinInner)
	if !errors.Is(err, ErrColumnTypeMismatch) {
		t.Fatalf("want ErrColumnTypeMismatch, got %v", err)
	}
}

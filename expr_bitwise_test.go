package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// bitFlagsFrame builds a one-column Int64 Frame of packed-flag
// values for exercising BitAnd/BitOr/BitXor with a scalar mask.
func bitFlagsFrame(t testing.TB) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	// Bit 0 set: 1, 3, 5. Bit 1 set: 2, 3, 6, 7.
	b.AppendValues([]int64{0, 1, 2, 3, 4, 5, 6, 7}, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "flags", Type: arrow.PrimitiveTypes.Int64, Nullable: false}
	col := arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestExpr_BitAnd_Scalar — Col & Lit(bit) unpacks a single flag,
// output stays Int64.
func TestExpr_BitAnd_Scalar(t *testing.T) {
	f := bitFlagsFrame(t)
	out, err := f.WithColumnExpr("bit0", Col("flags").BitAnd(Lit(int64(1))))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("bit0")
	if col.DataType().ID() != arrow.INT64 {
		t.Fatalf("dtype = %s, want INT64", col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	want := []int64{0, 1, 0, 1, 0, 1, 0, 1}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

// TestExpr_BitOr_BitXor_Scalar — sanity for the other two ops on the
// same fixture.
func TestExpr_BitOr_BitXor_Scalar(t *testing.T) {
	f := bitFlagsFrame(t)
	out, err := f.WithColumnExpr("or8", Col("flags").BitOr(Lit(int64(8))))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.mustCol("or8").col.Data().Chunks()[0].(*array.Int64)
	// Every value gets bit 3 set → 8, 9, 10, 11, 12, 13, 14, 15.
	want := []int64{8, 9, 10, 11, 12, 13, 14, 15}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("or8 row %d = %d, want %d", i, arr.Value(i), w)
		}
	}

	out, err = f.WithColumnExpr("xor5", Col("flags").BitXor(Lit(int64(5))))
	if err != nil {
		t.Fatal(err)
	}
	arr = out.mustCol("xor5").col.Data().Chunks()[0].(*array.Int64)
	xorWant := []int64{5, 4, 7, 6, 1, 0, 3, 2}
	for i, w := range xorWant {
		if arr.Value(i) != w {
			t.Errorf("xor5 row %d = %d, want %d", i, arr.Value(i), w)
		}
	}
}

// TestExpr_BitAnd_ColCol — col & col path (falls through the scalar
// fast path when both operands are ExprNodes rather than literals).
func TestExpr_BitAnd_ColCol(t *testing.T) {
	pool := memory.DefaultAllocator
	aB := array.NewInt64Builder(pool)
	defer aB.Release()
	aB.AppendValues([]int64{0xF0, 0xF0, 0xFF, 0x0F}, nil)
	bB := array.NewInt64Builder(pool)
	defer bB.Release()
	bB.AppendValues([]int64{0x0F, 0xFF, 0xAA, 0xF0}, nil)
	arrA := aB.NewArray()
	defer arrA.Release()
	arrB := bB.NewArray()
	defer arrB.Release()
	fields := []arrow.Field{
		{Name: "a", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "b", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(arrA.DataType(), []arrow.Array{arrA})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(arrB.DataType(), []arrow.Array{arrB})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.WithColumnExpr("and", Col("a").BitAnd(Col("b")))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.mustCol("and").col.Data().Chunks()[0].(*array.Int64)
	want := []int64{0x00, 0xF0, 0xAA, 0x00}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d = %x, want %x", i, arr.Value(i), w)
		}
	}
}

// TestExpr_Bitwise_RejectsFloat — bitwise on Float column errors
// at Type() time.
func TestExpr_Bitwise_RejectsFloat(t *testing.T) {
	pool := memory.DefaultAllocator
	fb := array.NewFloat64Builder(pool)
	defer fb.Release()
	fb.AppendValues([]float64{1.5, 2.5}, nil)
	arr := fb.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	col := arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, _ := NewFrame(schema, []arrow.Column{*col})
	_, err := f.WithColumnExpr("bad", Col("x").BitAnd(Lit(int64(1))))
	if err == nil {
		t.Fatal("expected error for BitAnd on Float64 column")
	}
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("error should wrap ErrExprTypeMismatch, got %v", err)
	}
}

// mustCol returns the named column or panics — test-only helper for
// tighter assertions.
func (f *Frame) mustCol(name string) Series {
	s, err := f.Column(name)
	if err != nil {
		panic(err)
	}
	return s
}

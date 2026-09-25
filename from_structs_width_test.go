package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestStructs_AllWidthsRoundTrip — every integer width FromStructs
// writes, plus []byte, must read back through ToStructs. Before, the
// 8- and 16-bit widths and plain []byte failed with "unsupported type".
func TestStructs_AllWidthsRoundTrip(t *testing.T) {
	type row struct {
		I8   int8
		I16  int16
		I32  int32
		I64  int64
		U8   uint8
		U16  uint16
		U32  uint32
		U64  uint64
		P16  *int16
		PU8  *uint8
		Raw  []byte
		L8   []int8
		LU16 []uint16
	}
	p16, pu8 := int16(-7), uint8(9)
	rows := []row{
		{
			I8: math.MinInt8, I16: math.MaxInt16, I32: math.MinInt32, I64: math.MaxInt64,
			U8: math.MaxUint8, U16: math.MaxUint16, U32: math.MaxUint32, U64: math.MaxUint64,
			P16: &p16, PU8: &pu8, Raw: []byte{0, 1, 2},
			L8: []int8{-1, 2}, LU16: []uint16{3, math.MaxUint16},
		},
		{}, // zero values; nil pointers and slices
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	back, err := ToStructs[row](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	g, w := back[0], rows[0]
	if g.I8 != w.I8 || g.I16 != w.I16 || g.I32 != w.I32 || g.I64 != w.I64 ||
		g.U8 != w.U8 || g.U16 != w.U16 || g.U32 != w.U32 || g.U64 != w.U64 {
		t.Errorf("scalars: got %+v, want %+v", g, w)
	}
	if g.P16 == nil || *g.P16 != p16 || g.PU8 == nil || *g.PU8 != pu8 {
		t.Errorf("pointers: got %v %v", g.P16, g.PU8)
	}
	if string(g.Raw) != string(w.Raw) {
		t.Errorf("Raw = %v, want %v", g.Raw, w.Raw)
	}
	if len(g.L8) != 2 || g.L8[0] != -1 || g.L8[1] != 2 || len(g.LU16) != 2 || g.LU16[1] != math.MaxUint16 {
		t.Errorf("lists: got %v %v", g.L8, g.LU16)
	}
	if z := back[1]; z.P16 != nil || z.PU8 != nil || z.I16 != 0 || z.U8 != 0 {
		t.Errorf("zero row: got %+v", z)
	}
}

// TestToStructs_NarrowingOverflowErrors — a column value that doesn't
// fit the field errors instead of wrapping (1<<30 into int16 used to
// come back as 0). Values that fit still read into narrower fields.
func TestToStructs_NarrowingOverflowErrors(t *testing.T) {
	type wide struct {
		I int64
		U uint64
		F float64
		L []int64
	}
	f, err := FromStructs([]wide{{I: 1 << 30, U: 1 << 40, F: 1e300, L: []int64{1, 1 << 20}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	type narrowI struct{ I int16 }
	type narrowU struct{ U uint32 }
	type narrowF struct{ F float32 }
	type narrowL struct{ L []int8 }
	checks := map[string]error{}
	_, checks["int64→int16"] = ToStructs[narrowI](f)
	_, checks["uint64→uint32"] = ToStructs[narrowU](f)
	_, checks["float64→float32"] = ToStructs[narrowF](f)
	_, checks["[]int64→[]int8"] = ToStructs[narrowL](f)
	for name, err := range checks {
		if !errors.Is(err, ErrStructFieldOverflow) {
			t.Errorf("%s: err = %v, want ErrStructFieldOverflow", name, err)
		}
	}

	// In-range values into a narrower field are fine.
	small, err := FromStructs([]wide{{I: -300, U: 70_000, F: 1.5, L: []int64{-5}}})
	if err != nil {
		t.Fatal(err)
	}
	defer small.Release()
	type fits struct {
		I int16
		U uint32
		F float32
		L []int8
	}
	got, err := ToStructs[fits](small)
	if err != nil {
		t.Fatalf("in-range narrowing: %v", err)
	}
	if got[0].I != -300 || got[0].U != 70_000 || got[0].F != 1.5 || len(got[0].L) != 1 || got[0].L[0] != -5 {
		t.Errorf("in-range narrowing: got %+v", got[0])
	}
}

// TestToStructs_ListElementTypeMismatchErrors — a list whose element
// type doesn't match the slice field errors rather than panicking in
// reflect.
func TestToStructs_ListElementTypeMismatchErrors(t *testing.T) {
	lb := array.NewListBuilder(memory.DefaultAllocator, arrow.PrimitiveTypes.Uint8)
	lb.Append(true)
	lb.ValueBuilder().(*array.Uint8Builder).Append(3)
	list := lb.NewArray()
	lb.Release()
	f := frameFromChunks(t, []string{"xs"}, [][]arrow.Array{{list}})
	defer f.Release()
	type row struct {
		XS []string `gobi:"xs"`
	}
	if _, err := ToStructs[row](f); err == nil {
		t.Error("uint8 list into []string: want error, got nil")
	}
}

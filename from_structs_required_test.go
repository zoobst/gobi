package gobi

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func nullableOf(t *testing.T, f *Frame) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, fld := range f.Schema().Fields() {
		out[fld.Name] = fld.Nullable
	}
	return out
}

// TestStructRequiredFields_Nullability — non-pointer fields become
// REQUIRED; pointers stay nullable; tags override per field.
func TestStructRequiredFields_Nullability(t *testing.T) {
	type row struct {
		A int64
		B *int64
		C string `gobi:"C,optional"`
		L []int32
	}
	f, err := FromStructs([]row{{}}, StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	want := map[string]bool{"A": false, "B": true, "C": true, "L": false}
	if got := nullableOf(t, f); !mapsEqual(got, want) {
		t.Errorf("nullable = %v, want %v", got, want)
	}
	// Required nil slice is an empty list, not null.
	l, _ := f.Column("L")
	if la := l.Column().Data().Chunk(0).(*array.List); la.IsNull(0) || la.Len() != 1 {
		t.Error("required nil slice should be an empty non-null list")
	}

	// `required` tag alone, without the option.
	type tagged struct {
		ID   int64 `gobi:"ID,required"`
		Name string
	}
	g, err := FromStructs([]tagged{{}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release()
	if got := nullableOf(t, g); got["ID"] || !got["Name"] {
		t.Errorf("tag-only required: nullable = %v", got)
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestStructRequiredFields_Errors(t *testing.T) {
	type ptrReq struct {
		P *int64 `gobi:"P,required"`
	}
	type both struct {
		X int64 `gobi:"X,required,optional"`
	}
	type geomReq struct {
		G string `gobi:"G" geom:"true"`
	}
	if _, err := FromStructs([]ptrReq{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("required pointer: err = %v", err)
	}
	if _, err := FromStructs([]both{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("required+optional: err = %v", err)
	}
	if _, err := FromStructs([]geomReq{{}}, StructRequiredFields()); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("empty required geometry: err = %v", err)
	}
}

// TestStructZeroTime — zero time.Time is null by default, the zero
// instant with StructZeroTimeAsValue or a required field, and an error
// on a nanosecond column that can't hold it.
func TestStructZeroTime(t *testing.T) {
	type us struct {
		T time.Time `gobi:"T,timestamp(microsecond)"`
	}
	type ns struct {
		T time.Time
	}
	isNull := func(f *Frame) bool {
		c, _ := f.Column("T")
		return c.Column().Data().Chunk(0).IsNull(0)
	}
	zeroInstant := func(f *Frame) bool {
		back, err := ToStructs[us](f)
		return err == nil && back[0].T.Equal(time.Time{})
	}

	def, err := FromStructs([]us{{}})
	if err != nil {
		t.Fatal(err)
	}
	defer def.Release()
	if !isNull(def) {
		t.Error("default: zero time should be null")
	}
	val, err := FromStructs([]us{{}}, StructZeroTimeAsValue())
	if err != nil {
		t.Fatal(err)
	}
	defer val.Release()
	if isNull(val) || !zeroInstant(val) {
		t.Error("StructZeroTimeAsValue: want the zero instant")
	}
	req, err := FromStructs([]us{{}}, StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	defer req.Release()
	if isNull(req) || !zeroInstant(req) {
		t.Error("required: want the zero instant")
	}
	if _, err := FromStructs([]ns{{}}, StructZeroTimeAsValue()); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("zero time into Timestamp[ns]: err = %v, want ErrStructFieldOverflow", err)
	}
}

// TestFromStructs_NanosecondRange — times outside 1677–2262 used to
// overflow UnixNano silently; now they error on ns columns and work on
// coarser ones.
func TestFromStructs_NanosecondRange(t *testing.T) {
	far := time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)
	type ns struct{ T time.Time }
	type nsList struct{ L []time.Time }
	type us struct {
		T time.Time `gobi:"T,timestamp(microsecond)"`
	}
	if _, err := FromStructs([]ns{{far}}); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("2500 into ns: err = %v", err)
	}
	if _, err := FromStructs([]nsList{{[]time.Time{far}}}); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("2500 list element into ns: err = %v", err)
	}
	f, err := FromStructs([]us{{far}})
	if err != nil {
		t.Fatalf("2500 into us: %v", err)
	}
	f.Release()
	edge := time.Date(2262, 4, 11, 0, 0, 0, 0, time.UTC)
	g, err := FromStructs([]ns{{edge}})
	if err != nil {
		t.Fatalf("in-range edge: %v", err)
	}
	g.Release()
}

// TestStructAllocator — FromStructs builds with the given allocator,
// and the Frame's Release returns everything.
func TestStructAllocator(t *testing.T) {
	type row struct {
		A int64
		S string
	}
	checked := memory.NewCheckedAllocator(memory.NewGoAllocator())
	f, err := FromStructs([]row{{1, "x"}, {2, "y"}}, StructAllocator(checked))
	if err != nil {
		t.Fatal(err)
	}
	if checked.CurrentAlloc() == 0 {
		t.Fatal("StructAllocator not used")
	}
	f.Release()
	checked.AssertSize(t, 0)
}

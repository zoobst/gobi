package gobi

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// frameOf builds a Frame from series and drops the caller's reference
// on each, so the Frame owns the only one.
func frameOf(t *testing.T, series ...Series) *Frame {
	t.Helper()
	f, err := NewFrameFromSeries(series...)
	for _, s := range series {
		if s.Column() != nil {
			s.Column().Release()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// nullAndSentFrame: row 0 is all NULL, row 1 sends false / 0 / "" /
// the epoch.
func nullAndSentFrame(t *testing.T) *Frame {
	t.Helper()
	valid := []bool{false, true}
	ts, err := NewTimestampSeriesUnit("seen", []time.Time{{}, time.Unix(0, 0)}, valid, arrow.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	return frameOf(t,
		NewBoolSeries("on_ground", []bool{false, false}, valid),
		NewInt64Series("alt", []int64{0, 0}, valid),
		NewStringSeries("source", []string{"", ""}, valid),
		ts,
	)
}

func TestStructRejectNulls(t *testing.T) {
	f := nullAndSentFrame(t)
	defer f.Release()

	type plain struct {
		OnGround bool      `csv:"on_ground"`
		Alt      int64     `csv:"alt"`
		Source   string    `csv:"source"`
		Seen     time.Time `csv:"seen"`
	}
	// Default: unchanged — the NULL row reads like the sent row.
	rows, err := ToStructs[plain](f, StructRequireColumns())
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	r0, r1 := rows[0], rows[1]
	r0.Seen, r1.Seen = time.Time{}, time.Time{} // the sent row's time is the epoch
	if r0 != r1 {
		t.Fatalf("default: rows differ: %+v", rows)
	}

	// Each plain kind is rejected on its own, naming row and column.
	for col, typ := range map[string]string{"on_ground": "bool", "alt": "int64", "source": "string", "seen": "time.Time"} {
		sub, err := f.SelectCols(col)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ToStructs[plain](sub, StructRejectNulls())
		sub.Release()
		if !errors.Is(err, ErrStructFieldNull) || !strings.Contains(err.Error(), `row 0 "`+col+`"`) ||
			!strings.Contains(err.Error(), ": "+typ+" field can't") {
			t.Errorf("%s: err = %v", col, err)
		}
	}

	// Fields that can hold a null are unaffected.
	type nullable struct {
		OnGround *bool        `csv:"on_ground"`
		Alt      Null[int64]  `csv:"alt"`
		Source   Null[string] `csv:"source"`
		Seen     *time.Time   `csv:"seen"`
	}
	nrows, err := ToStructs[nullable](f, StructRejectNulls(), StructRequireColumns())
	if err != nil {
		t.Fatal(err)
	}
	if nrows[0].OnGround != nil || nrows[0].Alt.Valid || nrows[0].Source.Valid || nrows[0].Seen != nil {
		t.Errorf("null row = %+v", nrows[0])
	}
	if nrows[1].OnGround == nil || *nrows[1].OnGround || !nrows[1].Alt.Valid || !nrows[1].Source.Valid || nrows[1].Seen == nil {
		t.Errorf("sent row = %+v", nrows[1])
	}

	// `optional` keeps zero-fill for that field only.
	type mixed struct {
		Alt    int64  `csv:"alt"`
		Source string `csv:"source,optional"`
	}
	sub, err := f.SelectCols("source")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Release()
	if _, err := ToStructs[mixed](sub, StructRejectNulls()); err != nil {
		t.Errorf("optional field: %v", err)
	}
	if _, err := ToStructs[mixed](f, StructRejectNulls()); !errors.Is(err, ErrStructFieldNull) || !strings.Contains(err.Error(), `"alt"`) {
		t.Errorf("non-optional field: err = %v", err)
	}

	// No nulls at all: no error.
	sent, err := f.Take([]int{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	defer sent.Release()
	if _, err := ToStructs[plain](sent, StructRejectNulls()); err != nil {
		t.Errorf("all valid: %v", err)
	}
}

func TestStructRejectNulls_Lists(t *testing.T) {
	// Row 0: null list; row 1: [1, NULL, 3].
	lb := array.NewListBuilder(memory.DefaultAllocator, arrow.PrimitiveTypes.Int64)
	vb := lb.ValueBuilder().(*array.Int64Builder)
	lb.AppendNull()
	lb.Append(true)
	vb.Append(1)
	vb.AppendNull()
	vb.Append(3)
	f := frameOf(t, SeriesFromArray(arrow.Field{Name: "xs", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true}, lb.NewArray()))
	lb.Release()
	defer f.Release()

	type vals struct {
		Xs []int64 `csv:"xs"`
	}
	if _, err := ToStructs[vals](f, StructRejectNulls()); !errors.Is(err, ErrStructFieldNull) ||
		!strings.Contains(err.Error(), `row 1 "xs": elem 1:`) || !strings.Contains(err.Error(), "use []*int64") {
		t.Errorf("null element: err = %v", err)
	}
	// Default: zero-filled, as before.
	if rows, err := ToStructs[vals](f); err != nil || rows[0].Xs != nil || rows[1].Xs[1] != 0 {
		t.Errorf("default: %+v, %v", rows, err)
	}

	// Pointer elements hold the null; a null list cell is a nil slice.
	type ptrs struct {
		Xs []*int64 `csv:"xs"`
	}
	rows, err := ToStructs[ptrs](f, StructRejectNulls())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Xs != nil || len(rows[1].Xs) != 3 || rows[1].Xs[1] != nil || *rows[1].Xs[2] != 3 {
		t.Errorf("pointer elements: %+v", rows)
	}

	type optional struct {
		Xs []int64 `csv:"xs,optional"`
	}
	if _, err := ToStructs[optional](f, StructRejectNulls()); err != nil {
		t.Errorf("optional list: %v", err)
	}
}

// TestStructRejectNulls_Dictionary — a valid index pointing at a null
// dictionary entry is a null cell (resolved by the cursor's null
// check).
func TestStructRejectNulls_Dictionary(t *testing.T) {
	pool := memory.DefaultAllocator
	db := array.NewStringBuilder(pool)
	db.AppendNull()
	db.Append("adsb")
	dict := db.NewArray()
	db.Release()
	ib := array.NewInt32Builder(pool)
	ib.AppendValues([]int32{1, 0}, nil)
	idx := ib.NewArray()
	ib.Release()
	dt := &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}
	darr := array.NewDictionaryArray(dt, idx, dict)
	idx.Release()
	dict.Release()
	f := frameOf(t, SeriesFromArray(arrow.Field{Name: "source", Type: dt, Nullable: true}, darr))
	defer f.Release()

	type withString struct {
		Source string `csv:"source"`
	}
	if _, err := ToStructs[withString](f, StructRejectNulls()); !errors.Is(err, ErrStructFieldNull) || !strings.Contains(err.Error(), "row 1") {
		t.Errorf("err = %v", err)
	}
}

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

// nullAndSentFrame: row 0 is all NULL, row 1 sends false / 0 / "" /
// the zero instant.
func nullAndSentFrame(t *testing.T) *Frame {
	t.Helper()
	valid := []bool{false, true}
	ts, err := NewTimestampSeriesUnit("seen", []time.Time{{}, time.Unix(0, 0)}, valid, arrow.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFrameFromSeries(
		NewBoolSeries("on_ground", []bool{false, false}, valid),
		NewInt64Series("alt", []int64{0, 0}, valid),
		NewStringSeries("source", []string{"", ""}, valid),
		ts,
	)
	if err != nil {
		t.Fatal(err)
	}
	return f
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
	// Default: unchanged — the NULL row is indistinguishable.
	rows, err := ToStructs[plain](f, StructRequireColumns())
	r0, r1 := rows[0], rows[1]
	r0.Seen, r1.Seen = time.Time{}, time.Time{} // sent row's time is the epoch
	if err != nil || r0 != r1 {
		t.Fatalf("default: %+v, %v", rows, err)
	}

	// Each plain kind is rejected on its own, naming row and column.
	for _, col := range []string{"on_ground", "alt", "source", "seen"} {
		sub, err := f.SelectCols(col)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ToStructs[plain](sub, StructRejectNulls())
		sub.Release()
		if !errors.Is(err, ErrStructFieldNull) || !strings.Contains(err.Error(), `row 0 "`+col+`"`) {
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

func TestStructRejectNulls_SliceAndDictionary(t *testing.T) {
	pool := memory.DefaultAllocator

	// A null list leaves a nil slice, which already means "absent".
	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	lb.AppendNull()
	lb.Append(true)
	lb.ValueBuilder().(*array.Int64Builder).Append(1)
	list := SeriesFromArray(arrow.Field{Name: "xs", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true}, lb.NewArray())
	lb.Release()
	lf, _ := NewFrameFromSeries(list)
	defer lf.Release()
	type withSlice struct {
		Xs []int64 `csv:"xs"`
	}
	if rows, err := ToStructs[withSlice](lf, StructRejectNulls()); err != nil || rows[0].Xs != nil {
		t.Errorf("slice: %+v, %v", rows, err)
	}

	// A valid index pointing at a null dictionary entry is a null too.
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
	df, _ := NewFrameFromSeries(SeriesFromArray(arrow.Field{Name: "source", Type: dt, Nullable: true}, darr))
	defer df.Release()
	type withString struct {
		Source string `csv:"source"`
	}
	if _, err := ToStructs[withString](df, StructRejectNulls()); !errors.Is(err, ErrStructFieldNull) || !strings.Contains(err.Error(), "row 1") {
		t.Errorf("dictionary null entry: err = %v", err)
	}
}

package gobi

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
)

// nullRow carries a Null[T] field per supported value shape.
type nullRow struct {
	ID    int64
	Count Null[int64]
	Small Null[int8]
	Label Null[string]
	Score Null[float64]
	Flag  Null[bool]
	Blob  Null[[]byte]
	At    Null[time.Time] `gobi:"At,timestamp(microsecond)"`
}

// TestNull_RoundTrip — FromStructs writes null exactly when Valid is
// false, a zero V with Valid=true stays a non-null value, and ToStructs
// reads both back unchanged.
func TestNull_RoundTrip(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 8000, time.UTC)
	rows := []nullRow{
		{ID: 1,
			Count: Null[int64]{V: 42, Valid: true}, Small: Null[int8]{V: -7, Valid: true},
			Label: Null[string]{V: "x", Valid: true}, Score: Null[float64]{V: 1.5, Valid: true},
			Flag: Null[bool]{V: true, Valid: true}, Blob: Null[[]byte]{V: []byte{1, 2}, Valid: true},
			At: Null[time.Time]{V: at, Valid: true}},
		{ID: 2}, // every Null field invalid
		{ID: 3, // every field valid at its zero value
			Count: Null[int64]{Valid: true}, Small: Null[int8]{Valid: true},
			Label: Null[string]{Valid: true}, Score: Null[float64]{Valid: true},
			Flag: Null[bool]{Valid: true}, Blob: Null[[]byte]{V: []byte{}, Valid: true},
			At: Null[time.Time]{Valid: true}},
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	if dt := f.Schema().Field(2).Type; dt.ID() != arrow.INT8 {
		t.Errorf("Small column type = %s, want int8", dt)
	}
	for _, name := range []string{"Count", "Small", "Label", "Score", "Flag", "Blob", "At"} {
		s, err := f.Column(name)
		if err != nil {
			t.Fatal(err)
		}
		if !s.field.Nullable {
			t.Errorf("%s: column should be nullable", name)
		}
		chunk := s.Column().Data().Chunks()[0]
		if chunk.IsNull(0) || !chunk.IsNull(1) || chunk.IsNull(2) {
			t.Errorf("%s: nulls = [%v %v %v], want [false true false]",
				name, chunk.IsNull(0), chunk.IsNull(1), chunk.IsNull(2))
		}
	}

	back, err := ToStructs[nullRow](f)
	if err != nil {
		t.Fatal(err)
	}
	r0, r1, r2 := back[0], back[1], back[2]
	if r0.Count != (Null[int64]{V: 42, Valid: true}) || r0.Small != (Null[int8]{V: -7, Valid: true}) ||
		r0.Label != (Null[string]{V: "x", Valid: true}) || r0.Score != (Null[float64]{V: 1.5, Valid: true}) ||
		r0.Flag != (Null[bool]{V: true, Valid: true}) {
		t.Errorf("row 0 scalars: got %+v", r0)
	}
	if !r0.Blob.Valid || string(r0.Blob.V) != "\x01\x02" {
		t.Errorf("row 0 Blob: got %+v", r0.Blob)
	}
	if !r0.At.Valid || !r0.At.V.Equal(at) {
		t.Errorf("row 0 At: got %+v, want %v", r0.At, at)
	}
	if r1.Count.Valid || r1.Small.Valid || r1.Label.Valid || r1.Score.Valid ||
		r1.Flag.Valid || r1.Blob.Valid || r1.At.Valid {
		t.Errorf("row 1: every field should be invalid, got %+v", r1)
	}
	if !r2.Count.Valid || r2.Count.V != 0 || !r2.Label.Valid || r2.Label.V != "" ||
		!r2.Flag.Valid || r2.Flag.V || !r2.Blob.Valid || len(r2.Blob.V) != 0 {
		t.Errorf("row 2: zero values should read back valid, got %+v", r2)
	}
	if !r2.At.Valid || !r2.At.V.IsZero() {
		t.Errorf("row 2 At: want valid zero instant, got %+v", r2.At)
	}
}

// TestNull_ZeroTimeNeedsUnit — a valid zero time.Time is the zero
// instant, which doesn't fit the default Timestamp[ns].
func TestNull_ZeroTimeNeedsUnit(t *testing.T) {
	type row struct{ At Null[time.Time] }
	_, err := FromStructs([]row{{At: Null[time.Time]{Valid: true}}})
	if !errors.Is(err, ErrStructFieldOverflow) {
		t.Fatalf("err = %v, want ErrStructFieldOverflow", err)
	}
	// Invalid stays null with any unit.
	if _, err := FromStructs([]row{{}}); err != nil {
		t.Fatalf("invalid zero time: %v", err)
	}
}

// TestNull_ReadIntoReusedSlice — ToStructsInto resets reused rows, so a
// null cell clears a Valid left over from the previous batch.
func TestNull_ReadIntoReusedSlice(t *testing.T) {
	type row struct{ N Null[int64] }
	valid, err := FromStructs([]row{{N: Null[int64]{V: 9, Valid: true}}})
	if err != nil {
		t.Fatal(err)
	}
	null, err := FromStructs([]row{{}})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := ToStructsInto(valid, []row(nil))
	if err != nil {
		t.Fatal(err)
	}
	dst, err = ToStructsInto(null, dst)
	if err != nil {
		t.Fatal(err)
	}
	if dst[0].N.Valid || dst[0].N.V != 0 {
		t.Errorf("reused row should be reset to invalid zero, got %+v", dst[0].N)
	}
}

// TestNull_RequiredFieldsLeavesNullable — StructRequiredFields makes
// plain fields REQUIRED but keeps Null[T] fields nullable.
func TestNull_RequiredFieldsLeavesNullable(t *testing.T) {
	type row struct {
		A int64
		B Null[int64]
	}
	f, err := FromStructs([]row{{A: 1}}, StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	if f.Schema().Field(0).Nullable {
		t.Error("A should be required")
	}
	if !f.Schema().Field(1).Nullable {
		t.Error("B should stay nullable")
	}
}

// TestNull_UnsupportedShapes — ambiguous Null forms are rejected at
// planning time by both directions.
func TestNull_UnsupportedShapes(t *testing.T) {
	type ptrNull struct{ X *Null[int64] }
	type nullPtr struct{ X Null[*int64] }
	type nullSlice struct{ X Null[[]int64] }
	type requiredNull struct {
		X Null[int64] `gobi:"x,required"`
	}
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrUnsupportedStructField) {
			t.Errorf("%s: err = %v, want ErrUnsupportedStructField", name, err)
		}
	}
	_, err := FromStructs([]ptrNull{{}})
	check("*Null[T] write", err)
	_, err = FromStructs([]nullPtr{{}})
	check("Null[*T] write", err)
	_, err = FromStructs([]nullSlice{{}})
	check("Null[[]T] write", err)
	_, err = FromStructs([]requiredNull{{}})
	check("required Null[T] write", err)

	f, err := FromStructs([]struct{ X int64 }{{X: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ToStructs[ptrNull](f)
	check("*Null[T] read", err)
	_, err = ToStructs[nullPtr](f)
	check("Null[*T] read", err)
}

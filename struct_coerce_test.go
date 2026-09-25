package gobi

import (
	"errors"
	"math"
	"testing"
)

// TestStructCoerceNumbers_Exact — cross-kind conversions succeed when
// the value survives exactly, including list elements.
func TestStructCoerceNumbers_Exact(t *testing.T) {
	type src struct {
		F  float64
		F2 float32
		I  int64
		U  uint32
		L  []float64
	}
	f, err := FromStructs([]src{{F: 3, F2: -2, I: 1 << 50, U: 7, L: []float64{1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	type dst struct {
		F  int32   // float64 3.0 → 3
		F2 int8    // float32 -2 → -2
		I  float64 // 2^50 fits float64's mantissa
		U  int16   // uint32 7 → 7
		L  []int16 // float64 elements → int16
	}
	got, err := ToStructs[dst](f, StructCoerceNumbers())
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	g := got[0]
	if g.F != 3 || g.F2 != -2 || g.I != 1<<50 || g.U != 7 || len(g.L) != 2 || g.L[1] != 2 {
		t.Errorf("got %+v", g)
	}

	// Without the option, cross-kind stays an error.
	if _, err := ToStructs[dst](f); err == nil {
		t.Error("cross-kind without StructCoerceNumbers: want error")
	}
}

// TestStructCoerceNumbers_Rejects — lossy or out-of-range values fail
// with the matching sentinel.
func TestStructCoerceNumbers_Rejects(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
		want error
	}{
		{"non-integral float → int", func() error {
			return coerceInto[struct{ V int64 }](struct{ V float64 }{3.5})
		}, ErrStructFieldInexact},
		{"NaN → int", func() error {
			return coerceInto[struct{ V int64 }](struct{ V float64 }{math.NaN()})
		}, ErrStructFieldInexact},
		{"huge float → int64", func() error {
			return coerceInto[struct{ V int64 }](struct{ V float64 }{1e19})
		}, ErrStructFieldOverflow},
		{"float → int8 overflow", func() error {
			return coerceInto[struct{ V int8 }](struct{ V float64 }{300})
		}, ErrStructFieldOverflow},
		{"negative → uint", func() error {
			return coerceInto[struct{ V uint64 }](struct{ V int64 }{-1})
		}, ErrStructFieldOverflow},
		{"uint64 max → int64", func() error {
			return coerceInto[struct{ V int64 }](struct{ V uint64 }{math.MaxUint64})
		}, ErrStructFieldOverflow},
		{"2^53+1 → float64", func() error {
			return coerceInto[struct{ V float64 }](struct{ V int64 }{1<<53 + 1})
		}, ErrStructFieldInexact},
		{"2^24+1 → float32", func() error {
			return coerceInto[struct{ V float32 }](struct{ V int32 }{1<<24 + 1})
		}, ErrStructFieldInexact},
	}
	for _, c := range cases {
		if err := c.run(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

// coerceInto round-trips one source row into D under
// StructCoerceNumbers and returns the error.
func coerceInto[D any, S any](row S) error {
	f, err := FromStructs([]S{row})
	if err != nil {
		return err
	}
	defer f.Release()
	_, err = ToStructs[D](f, StructCoerceNumbers())
	return err
}

// TestStructRequireColumns — a field with no column fails instead of
// zero-filling; present columns and default behavior are unchanged.
func TestStructRequireColumns(t *testing.T) {
	type src struct{ A int64 }
	f, err := FromStructs([]src{{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	type dst struct {
		A int64
		B string
	}
	if _, err := ToStructs[dst](f, StructRequireColumns()); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("missing column B: err = %v, want ErrColumnNotFound", err)
	}
	got, err := ToStructs[dst](f)
	if err != nil || got[0].A != 1 || got[0].B != "" {
		t.Errorf("default zero-fill: %+v, %v", got, err)
	}
	if _, err := ToStructs[src](f, StructRequireColumns()); err != nil {
		t.Errorf("all columns present: %v", err)
	}
}

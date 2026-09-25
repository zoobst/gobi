package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

func TestExprRound_Modes(t *testing.T) {
	type row struct {
		F  *float64
		F4 float32
	}
	in := []float64{2.5, -2.5, 2.4, -2.6, 0.5, -0.5, 3, math.Inf(1), math.NaN()}
	rows := make([]row, 0, len(in)+1)
	for _, v := range in {
		rows = append(rows, row{F: ptr(v), F4: float32(v)})
	}
	rows = append(rows, row{F: nil, F4: 1.5})
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	want := map[string][]float64{
		"round": {3, -3, 2, -3, 1, -1, 3, math.Inf(1), math.NaN()},
		"floor": {2, -3, 2, -3, 0, -1, 3, math.Inf(1), math.NaN()},
		"ceil":  {3, -2, 3, -2, 1, -0, 3, math.Inf(1), math.NaN()},
		"trunc": {2, -2, 2, -2, 0, -0, 3, math.Inf(1), math.NaN()},
	}
	exprs := map[string]func(Expr) Expr{
		"round": Expr.Round, "floor": Expr.Floor, "ceil": Expr.Ceil, "trunc": Expr.Trunc,
	}
	same := func(a, b float64) bool { return a == b || (math.IsNaN(a) && math.IsNaN(b)) }
	for name, mk := range exprs {
		out, err := f.WithColumnExpr("r", mk(Col("F")))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r, _ := out.Column("r")
		a := r.Column().Data().Chunk(0).(*array.Float64)
		for i, w := range want[name] {
			if !same(a.Value(i), w) {
				t.Errorf("%s(%v) = %v, want %v", name, in[i], a.Value(i), w)
			}
		}
		if !a.IsNull(len(in)) {
			t.Errorf("%s: null input should stay null", name)
		}
		// Float32 keeps its type.
		out4, err := f.WithColumnExpr("r4", mk(Col("F4")))
		if err != nil {
			t.Fatal(err)
		}
		r4, _ := out4.Column("r4")
		if r4.DataType().ID() != arrow.FLOAT32 {
			t.Errorf("%s: Float32 in → %s out", name, r4.DataType())
		}
		if got := r4.Column().Data().Chunk(0).(*array.Float32).Value(0); !same(float64(got), want[name][0]) {
			t.Errorf("%s float32(2.5) = %v", name, got)
		}
	}
}

// TestExprRound_IntoIntField — Round + StructCoerceNumbers lands
// fractional floats in an integer field, rounding half away from zero,
// and still errors on overflow.
func TestExprRound_IntoIntField(t *testing.T) {
	type src struct{ Acc float64 }
	type dst struct{ Acc int32 }
	f, err := FromStructs([]src{{12.5}, {-0.4}, {7}, {3.49}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if _, err := ToStructs[dst](f, StructCoerceNumbers()); !errors.Is(err, ErrStructFieldInexact) {
		t.Fatalf("without Round: err = %v, want ErrStructFieldInexact", err)
	}
	r, err := f.WithColumnExpr("Acc", Col("Acc").Round())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ToStructs[dst](r, StructCoerceNumbers())
	if err != nil {
		t.Fatalf("with Round: %v", err)
	}
	for i, w := range []int32{13, 0, 7, 3} {
		if got[i].Acc != w {
			t.Errorf("row %d = %d, want %d", i, got[i].Acc, w)
		}
	}

	big, err := FromStructs([]src{{3e9}})
	if err != nil {
		t.Fatal(err)
	}
	defer big.Release()
	rb, err := big.WithColumnExpr("Acc", Col("Acc").Round())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ToStructs[dst](rb, StructCoerceNumbers()); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("3e9 into int32: err = %v, want ErrStructFieldOverflow", err)
	}
}

func TestExprRound_TypesAndErrors(t *testing.T) {
	type row struct {
		I int32
		S string
	}
	f, err := FromStructs([]row{{5, "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	out, err := f.WithColumnExpr("r", Col("I").Round())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := out.Column("r")
	if r.DataType().ID() != arrow.INT32 || r.Column().Data().Chunk(0).(*array.Int32).Value(0) != 5 {
		t.Error("integer input should pass through unchanged")
	}
	if _, err := f.WithColumnExpr("r", Col("S").Floor()); !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("string input: err = %v", err)
	}
	if typ, err := Col("I").Ceil().Node().Type(f.Schema()); err != nil || typ.ID() != arrow.INT32 {
		t.Errorf("Type() = %v, %v", typ, err)
	}
	if _, err := Col("S").Trunc().Node().Type(f.Schema()); !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("Type() on string: err = %v", err)
	}
	if s := Col("I").Round().String(); s == "" {
		t.Error("empty String()")
	}
}

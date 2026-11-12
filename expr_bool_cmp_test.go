package gobi

import (
	"slices"
	"testing"
)

// Comparing Boolean values passed the expression type check but failed
// at evaluation with "series is not numeric", so a filter such as
// Col("active").Eq(Lit(true)) could be built but never run.
func TestExpr_BoolComparison(t *testing.T) {
	// active = [true, false, true, false, null]
	// other  = [true, true,  false, false, true]
	f, err := NewFrameFromSeries(
		NewStringSeries("name", []string{"a", "b", "c", "d", "e"}, nil),
		NewBoolSeries("active", []bool{true, false, true, false, false}, []bool{true, true, true, true, false}),
		NewBoolSeries("other", []bool{true, true, false, false, true}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	cases := []struct {
		name string
		expr Expr
		want []string
	}{
		{"col = true", Col("active").Eq(Lit(true)), []string{"a", "c"}},
		{"col = false", Col("active").Eq(Lit(false)), []string{"b", "d"}},
		{"col != true", Col("active").Ne(Lit(true)), []string{"b", "d"}},
		{"true = col", Lit(true).Eq(Col("active")), []string{"a", "c"}},
		{"col = col", Col("active").Eq(Col("other")), []string{"a", "d"}},
		{"col != col", Col("active").Ne(Col("other")), []string{"b", "c"}},
		// false orders before true.
		{"col < col", Col("active").Lt(Col("other")), []string{"b"}},
		{"col <= col", Col("active").Le(Col("other")), []string{"a", "b", "d"}},
		{"col > col", Col("active").Gt(Col("other")), []string{"c"}},
		{"col >= col", Col("active").Ge(Col("other")), []string{"a", "c", "d"}},
	}
	names := func(t *testing.T, out *Frame) []string {
		t.Helper()
		s, err := out.Column("name")
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.Strings()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A null on either side is neither equal nor unequal, so
			// row "e" never matches.
			out, err := f.FilterExpr(c.expr)
			if err != nil {
				t.Fatalf("FilterExpr: %v", err)
			}
			defer out.Release()
			if got := names(t, out); !slices.Equal(got, c.want) {
				t.Errorf("FilterExpr kept %v, want %v", got, c.want)
			}

			lazy, err := f.Lazy().Filter(c.expr).Collect()
			if err != nil {
				t.Fatalf("Lazy: %v", err)
			}
			defer lazy.Release()
			if got := names(t, lazy); !slices.Equal(got, c.want) {
				t.Errorf("Lazy kept %v, want %v", got, c.want)
			}
		})
	}
}

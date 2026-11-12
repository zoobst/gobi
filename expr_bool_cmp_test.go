package gobi

import (
	"slices"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
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

// Boolean (and string) comparisons and the logical ops read every
// chunk of their operands. They used to read the first chunk only, so
// a column in two chunks gave a result as long as its first chunk,
// and a column with no chunks panicked.
func TestExpr_BoolAndStringOps_MultiChunk(t *testing.T) {
	pool := memory.DefaultAllocator
	boolCol := func(name string, chunks ...[]bool) arrow.Column {
		field := arrow.Field{Name: name, Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
		arrs := make([]arrow.Array, len(chunks))
		for i, vals := range chunks {
			b := array.NewBooleanBuilder(pool)
			b.AppendValues(vals, nil)
			arrs[i] = b.NewArray()
			b.Release()
		}
		ch := arrow.NewChunked(field.Type, arrs)
		defer ch.Release()
		for _, a := range arrs {
			a.Release()
		}
		return *arrow.NewColumn(field, ch)
	}
	strCol := func(name string, chunks ...[]string) arrow.Column {
		field := arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true}
		arrs := make([]arrow.Array, len(chunks))
		for i, vals := range chunks {
			b := array.NewStringBuilder(pool)
			b.AppendValues(vals, nil)
			arrs[i] = b.NewArray()
			b.Release()
		}
		ch := arrow.NewChunked(field.Type, arrs)
		defer ch.Release()
		for _, a := range arrs {
			a.Release()
		}
		return *arrow.NewColumn(field, ch)
	}
	frame := func(cols ...arrow.Column) *Frame {
		t.Helper()
		fields := make([]arrow.Field, len(cols))
		for i, c := range cols {
			fields[i] = c.Field()
		}
		f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	bools := func(t *testing.T, s Series) []bool {
		t.Helper()
		v, err := s.Bools()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// flag is split 2 + 3, other 4 + 1, name 1 + 4: no two columns
	// share chunk boundaries.
	f := frame(
		boolCol("flag", []bool{true, false}, []bool{true, true, false}),
		boolCol("other", []bool{true, true, false, true}, []bool{true}),
		strCol("name", []string{"a"}, []string{"b", "a", "c", "a"}),
		strCol("name2", []string{"a", "x", "a"}, []string{"c", "z"}),
	)
	defer f.Release()
	for name, c := range map[string]struct {
		expr Expr
		want []bool
	}{
		"bool = literal":   {Col("flag").Eq(Lit(true)), []bool{true, false, true, true, false}},
		"bool = bool":      {Col("flag").Eq(Col("other")), []bool{true, false, false, true, false}},
		"bool < bool":      {Col("flag").Lt(Col("other")), []bool{false, true, false, false, true}},
		"and":              {Col("flag").And(Col("other")), []bool{true, false, false, true, false}},
		"or":               {Col("flag").Or(Col("other")), []bool{true, true, true, true, true}},
		"not":              {Col("flag").Not(), []bool{false, true, false, false, true}},
		"string = string":  {Col("name").Eq(Col("name2")), []bool{true, false, true, true, false}},
		"string != string": {Col("name").Ne(Col("name2")), []bool{false, true, false, false, true}},
	} {
		out, err := c.expr.node.Eval(f)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := bools(t, out); !slices.Equal(got, c.want) {
			t.Errorf("%s = %v, want %v", name, got, c.want)
		}
	}

	// A column with no chunks at all is an empty column, not a panic.
	empty := frame(boolCol("flag"), boolCol("other"), strCol("name"), strCol("name2"))
	defer empty.Release()
	for name, e := range map[string]Expr{
		"bool = bool": Col("flag").Eq(Col("other")), "not": Col("flag").Not(), "string = string": Col("name").Eq(Col("name2")),
	} {
		out, err := e.node.Eval(empty)
		if err != nil {
			t.Errorf("no chunks, %s: %v", name, err)
			continue
		}
		if out.Len() != 0 {
			t.Errorf("no chunks, %s: len = %d, want 0", name, out.Len())
		}
	}
}

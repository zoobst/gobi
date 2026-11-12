package gobi

import (
	"slices"
	"testing"
	"time"

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

// Every operand pair the type check accepts for a comparison can be
// evaluated, for all six comparisons. String ordering and all Boolean
// comparisons used to pass the type check and then fail at
// evaluation, because each type was routed to its kernel by its own
// special case and some were missing.
func TestExpr_ComparisonsTypeCheckAndEvaluate(t *testing.T) {
	f, err := NewFrameFromSeries(
		NewInt64Series("int", []int64{1, 2, 3}, nil),
		NewFloat64Series("float", []float64{1.5, 2, 2.5}, nil),
		NewStringSeries("str", []string{"b", "a", "b"}, []bool{true, true, false}),
		NewStringSeries("str2", []string{"a", "a", "c"}, nil),
		NewBoolSeries("bool", []bool{true, false, true}, nil),
		NewBoolSeries("bool2", []bool{false, false, true}, nil),
		NewTimestampSeries("ts", []time.Time{time.Unix(1, 0), time.Unix(2, 0), time.Unix(3, 0)}, nil),
		NewTimestampSeries("ts2", []time.Time{time.Unix(2, 0), time.Unix(2, 0), time.Unix(2, 0)}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	ops := map[string]func(a, b Expr) Expr{
		"=": Expr.Eq, "!=": Expr.Ne, "<": Expr.Lt, "<=": Expr.Le, ">": Expr.Gt, ">=": Expr.Ge,
	}
	names := f.ColumnNames()
	for _, a := range names {
		for _, b := range names {
			for opName, op := range ops {
				e := op(Col(a), Col(b))
				_, typeErr := e.node.Type(f.Schema())
				_, evalErr := e.node.Eval(f)
				if (typeErr == nil) != (evalErr == nil) {
					t.Errorf("%s %s %s: type check err = %v, evaluation err = %v", a, opName, b, typeErr, evalErr)
				}
			}
		}
	}

	// String ordering is by byte value; a null on either side is null.
	for name, c := range map[string]struct {
		expr Expr
		want []bool // row 2 is null in "str"
	}{
		"str < str2":   {Col("str").Lt(Col("str2")), []bool{false, false}},
		"str <= str2":  {Col("str").Le(Col("str2")), []bool{false, true}},
		"str > str2":   {Col("str").Gt(Col("str2")), []bool{true, false}},
		"str >= str2":  {Col("str").Ge(Col("str2")), []bool{true, true}},
		`str2 < "b"`:   {Col("str2").Lt(Lit("b")), []bool{true, true, false}},
		`str2 >= "aa"`: {Col("str2").Ge(Lit("aa")), []bool{false, false, true}},
	} {
		out, err := c.expr.node.Eval(f)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got, err := out.Bools()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got[:len(c.want)], c.want) {
			t.Errorf("%s = %v, want %v", name, got, c.want)
		}
		if len(c.want) == 2 && !out.Nulls()[2] {
			t.Errorf("%s: row 2 should be null (null operand)", name)
		}
	}

	// And through a filter, as a query would use it.
	out, err := f.FilterExpr(Col("str2").Lt(Lit("b")))
	if err != nil {
		t.Fatalf("FilterExpr: %v", err)
	}
	defer out.Release()
	if out.NumRows() != 2 {
		t.Errorf("str2 < \"b\" kept %d rows, want 2", out.NumRows())
	}
}

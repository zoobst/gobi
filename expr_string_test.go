package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// stringExprFrame builds a single-String-column Frame from a []any
// where nil entries become nulls. Companion to stringSeries but
// wraps into a Frame so LazyFrame.Filter / WithColumn work.
func stringExprFrame(t *testing.T, name string, vals []any) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewStringBuilder(pool)
	defer b.Release()
	for _, v := range vals {
		if v == nil {
			b.AppendNull()
			continue
		}
		b.Append(v.(string))
	}
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// evalString evaluates a String-returning Expr against f and
// returns the values as []any (nil for null).
func evalString(t *testing.T, f *Frame, e Expr) []any {
	t.Helper()
	s, err := e.Node().Eval(f)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	return stringSeriesValues(t, s)
}

func TestExpr_StrLowerUpper(t *testing.T) {
	f := stringExprFrame(t, "name", []any{"Hello", "WORLD"})
	defer f.Release()
	if v := evalString(t, f, Col("name").StrLower()); v[0] != "hello" || v[1] != "world" {
		t.Errorf("StrLower: got %v", v)
	}
	if v := evalString(t, f, Col("name").StrUpper()); v[0] != "HELLO" || v[1] != "WORLD" {
		t.Errorf("StrUpper: got %v", v)
	}
}

func TestExpr_StrChaining(t *testing.T) {
	// Contains-after-Lower — case-insensitive substring match, one
	// of the classic dataframe idioms.
	f := stringExprFrame(t, "city", []any{"Los Angeles", "San Francisco", "New York"})
	defer f.Release()

	out, err := f.Lazy().Filter(
		Col("city").StrLower().StrContains("angeles"),
	).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	defer out.Release()
	if got := out.NumRows(); got != 1 {
		t.Errorf("row count = %d, want 1 (Los Angeles)", got)
	}
}

func TestExpr_StrRegexMatch_ComposesWithScalar(t *testing.T) {
	// Filter rows where name matches a phone-number-like pattern.
	f := stringExprFrame(t, "note", []any{
		"call 415-555-1234",
		"no phone here",
		"reach me at 212-555-9876",
	})
	defer f.Release()
	out, err := f.Lazy().Filter(
		Col("note").StrRegexMatch(`\d{3}-\d{3}-\d{4}`),
	).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	defer out.Release()
	if got := out.NumRows(); got != 2 {
		t.Errorf("row count = %d, want 2 (two phone-containing rows)", got)
	}
}

func TestExpr_StrLen_ReturnsInt64(t *testing.T) {
	f := stringExprFrame(t, "s", []any{"a", "ab", "abc"})
	defer f.Release()
	dt, err := Col("s").StrLen().Node().Type(f.Schema())
	if err != nil {
		t.Fatal(err)
	}
	if dt.ID() != arrow.INT64 {
		t.Errorf("StrLen type = %s, want Int64", dt)
	}
}

func TestExpr_StrLen_NonStringErrors(t *testing.T) {
	// Build a frame with Int64 column, then run StrLen on it.
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.Append(1)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: false}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	_, err = Col("n").StrLen().Node().Type(f.Schema())
	if err == nil {
		t.Errorf("StrLen on Int64 should error at Type-check time")
	}
	_, err = Col("n").StrLen().Node().Eval(f)
	if err == nil {
		t.Errorf("StrLen on Int64 should error at Eval time")
	}
}

func TestExpr_StrReflectionSurface(t *testing.T) {
	// Table-driven walk over every string op: each must have exactly
	// one child (the inner Expr), produce a non-empty non-sentinel
	// String() rendering, and return a well-formed arrow.DataType
	// from Type() against a String-column schema.
	f := stringExprFrame(t, "s", []any{"hello"})
	defer f.Release()

	cases := []struct {
		name string
		e    Expr
		want arrow.DataType
	}{
		{"StrLower", Col("s").StrLower(), arrow.BinaryTypes.String},
		{"StrUpper", Col("s").StrUpper(), arrow.BinaryTypes.String},
		{"StrTrim", Col("s").StrTrim(), arrow.BinaryTypes.String},
		{"StrTrimLeft", Col("s").StrTrimLeft("x"), arrow.BinaryTypes.String},
		{"StrTrimRight", Col("s").StrTrimRight("x"), arrow.BinaryTypes.String},
		{"StrLen", Col("s").StrLen(), arrow.PrimitiveTypes.Int64},
		{"StrContains", Col("s").StrContains("foo"), arrow.FixedWidthTypes.Boolean},
		{"StrStartsWith", Col("s").StrStartsWith("h"), arrow.FixedWidthTypes.Boolean},
		{"StrEndsWith", Col("s").StrEndsWith("o"), arrow.FixedWidthTypes.Boolean},
		{"StrReplace", Col("s").StrReplace("h", "y"), arrow.BinaryTypes.String},
		{"StrSlice", Col("s").StrSlice(0, 3), arrow.BinaryTypes.String},
		{"StrConcat", Col("s").StrConcat("!"), arrow.BinaryTypes.String},
		{"StrRegexMatch", Col("s").StrRegexMatch(`.*`), arrow.FixedWidthTypes.Boolean},
		{"StrRegexReplace", Col("s").StrRegexReplace(`.*`, "x"), arrow.BinaryTypes.String},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			children := tc.e.Node().Children()
			if len(children) != 1 {
				t.Errorf("Children() len = %d, want 1", len(children))
			}
			s := tc.e.String()
			if s == "" || s == "<nil-expr>" {
				t.Errorf("String() = %q", s)
			}
			dt, err := tc.e.Node().Type(f.Schema())
			if err != nil {
				t.Fatalf("Type: %v", err)
			}
			if dt.ID() != tc.want.ID() {
				t.Errorf("Type() = %s, want %s", dt, tc.want)
			}
		})
	}
}

// TestExpr_StrRegexMatch_BadPatternDeferredToEval — invalid patterns
// don't panic at Expr-build time; they surface at Eval / Type.
// Matches the Lit node's deferred-error style.
func TestExpr_StrRegexMatch_BadPatternDeferredToEval(t *testing.T) {
	f := stringExprFrame(t, "s", []any{"hello"})
	defer f.Release()

	e := Col("s").StrRegexMatch(`[invalid`)
	if e.Node() == nil {
		t.Fatal("StrRegexMatch(bad pattern) returned nil-node Expr")
	}
	// Type-check surfaces the error.
	if _, err := e.Node().Type(f.Schema()); err == nil {
		t.Errorf("Type() on bad-pattern StrRegexMatch should error")
	}
	// Eval surfaces the error.
	if _, err := e.Node().Eval(f); err == nil {
		t.Errorf("Eval on bad-pattern StrRegexMatch should error")
	}
}

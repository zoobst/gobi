package gobi

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// isIn evaluates Col(s.Name()).IsIn(values...) over a one-column frame
// and renders each row as T / F / N(ull).
func isIn(t *testing.T, s Series, values ...any) string {
	t.Helper()
	f, err := NewFrameFromSeries(s)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	e := Col(s.Name()).IsIn(values...)
	if dt, err := e.node.Type(f.Schema()); err != nil || dt.ID() != arrow.BOOL {
		t.Fatalf("Type = %v, %v", dt, err)
	}
	out, err := e.node.Eval(f)
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := out.Bools()
	var b strings.Builder
	for i, null := range out.Nulls() {
		switch {
		case null:
			b.WriteByte('N')
		case vals[i]:
			b.WriteByte('T')
		default:
			b.WriteByte('F')
		}
	}
	return b.String()
}

func TestIsIn_Types(t *testing.T) {
	i8 := func(vals ...int8) Series {
		b := array.NewInt8Builder(memory.DefaultAllocator)
		defer b.Release()
		b.AppendValues(vals, nil)
		return chunkedSeries("x", b.NewArray())
	}
	u64 := func(vals ...uint64) Series { return chunkedSeries("x", uint64Array(vals...)) }
	f32 := func(vals ...float32) Series { return chunkedSeries("x", float32Array(vals...)) }
	date := func(days ...int32) Series {
		b := array.NewDate32Builder(memory.DefaultAllocator)
		defer b.Release()
		for _, d := range days {
			b.Append(arrow.Date32(d))
		}
		return chunkedSeries("x", b.NewArray())
	}
	day2 := time.Date(1970, 1, 3, 0, 0, 0, 0, time.UTC)
	ts, err := NewTimestampSeriesUnit("x", []time.Time{time.Unix(5, 0), time.Unix(6, 0), {}}, []bool{true, true, false}, arrow.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		s      Series
		values []any
		want   string
	}{
		"int64 with null":     {NewInt64Series("x", []int64{1, 2, 3, 0}, []bool{true, true, true, false}), []any{3, int32(1)}, "TFTN"},
		"int64 from slice":    {NewInt64Series("x", []int64{1, 2, 3}, nil), []any{[]int64{2, 3}}, "FTT"},
		"int: 1.5 never, 2.0": {NewInt64Series("x", []int64{1, 2}, nil), []any{1.5, 2.0}, "FT"},
		"int8 out of range":   {i8(44, -1), []any{300, 44 + 256, -1}, "FT"},
		"uint64 max":          {u64(math.MaxUint64, 1), []any{uint64(math.MaxUint64), -1}, "TF"},
		"float32 rounds":      {f32(16777216, 0.1, 0.5), []any{16777217, 0.1}, "TTF"},
		"float64 from ints":   {NewFloat64Series("x", []float64{2, 2.5}, nil), []any{2}, "TF"},
		"string":              {NewStringSeries("x", []string{"CA", "NV", "OR"}, nil), []any{[]string{"OR", "CA"}}, "TFT"},
		"dictionary":          {stringDict(t, "a", "b", "a"), []any{"a"}, "TFT"},
		"bool":                {NewBoolSeries("x", []bool{true, false}, nil), []any{false}, "FT"},
		"zoned timestamp ms":  {ts, []any{time.Unix(6, 0), time.Unix(5, 1)}, "FTN"},
		"multi-chunk tz ts":   {zonedTS("x", []int64{1, 2}, []int64{3}), []any{time.Unix(3, 0).In(time.FixedZone("x", 3600))}, "FFT"},
		"date32 any time":     {date(2, 3, 4), []any{day2.Add(23 * time.Hour), time.Date(1970, 1, 4, 23, 0, 0, 0, fixedLA)}, "TTF"},
		"uint8 via []int":     {chunkedSeries("x", uint8Array(1, 7)), []any{[]int{7, 9}}, "FT"},
		"large_utf8":          {chunkedSeries("x", largeStrings("a", "b")), []any{"b"}, "FT"},
		"empty list":          {NewInt64Series("x", []int64{1, 0}, []bool{true, false}), nil, "FN"},
	} {
		if got := isIn(t, tc.s, tc.values...); got != tc.want {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}
}

// fixedLA is UTC-8 without tzdata: 1970-01-04 23:00 there is
// 1970-01-05 07:00 UTC, but its calendar date is the 4th.
var fixedLA = time.FixedZone("PST", -8*3600)

func uint8Array(vals ...uint8) arrow.Array {
	b := array.NewUint8Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, nil)
	return b.NewArray()
}

func largeStrings(vals ...string) arrow.Array {
	b := array.NewLargeStringBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, nil)
	return b.NewArray()
}

// TestIsIn_ValuesCopied — changing the caller's slice after building
// the expression (variadic or a single typed slice) doesn't change
// what it matches.
func TestIsIn_ValuesCopied(t *testing.T) {
	ids := []int64{1, 2}
	typed := Col("x").IsIn(ids)
	ids[0] = 0
	vals := []any{1, 2}
	e := Col("x").IsIn(vals...)
	vals[0] = struct{}{}
	for _, ex := range []Expr{e, typed} {
		f, _ := NewFrameFromSeries(NewInt64Series("x", []int64{0, 1, 2}, nil))
		out, err := ex.node.Eval(f)
		f.Release()
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := out.Bools(); !slices.Equal(got, []bool{false, true, true}) {
			t.Errorf("%s: got %v", ex, got)
		}
	}
	f, _ := NewFrameFromSeries(NewInt64Series("x", []int64{0, 1, 2}, nil))
	defer f.Release()
	out, err := e.node.Eval(f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := out.Bools(); !slices.Equal(got, []bool{false, true, true}) {
		t.Errorf("got %v", got)
	}
	if out.Name() != "x_is_in" {
		t.Errorf("result name %q, want x_is_in", out.Name())
	}
}

func TestIsIn_Errors(t *testing.T) {
	f, _ := NewFrameFromSeries(NewInt64Series("n", []int64{1}, nil))
	defer f.Release()
	for name, e := range map[string]Expr{
		"string vs int": Col("n").IsIn("1"),
		"nil value":     Col("n").IsIn(1, nil),
		"unsupported":   Col("n").IsIn(struct{}{}),
		"[]byte":        Col("n").IsIn([]byte("AB")),
	} {
		if _, err := e.node.Type(f.Schema()); err == nil {
			t.Errorf("%s: Type: no error", name)
		}
		if _, err := e.node.Eval(f); err == nil {
			t.Errorf("%s: Eval: no error", name)
		}
	}
	if _, err := Col("n").IsIn("1").node.Eval(f); !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("kind mismatch err = %v", err)
	}
}

func TestIsIn_FilterAndLazy(t *testing.T) {
	f, _ := NewFrameFromSeries(
		NewStringSeries("state", []string{"CA", "NV", "OR", "CA"}, nil),
		NewInt64Series("n", []int64{1, 2, 3, 4}, nil),
	)
	defer f.Release()

	eager, err := f.FilterExpr(Col("state").IsIn("CA", "OR"))
	if err != nil {
		t.Fatal(err)
	}
	defer eager.Release()
	lazy, err := f.Lazy().Filter(Col("state").IsIn([]string{"CA", "OR"})).Select(Col("n")).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer lazy.Release()
	for name, fr := range map[string]*Frame{"eager": eager, "lazy": lazy} {
		n, _ := fr.Column("n")
		if v, _ := n.Int64s(); !slices.Equal(v, []int64{1, 3, 4}) {
			t.Errorf("%s: n = %v", name, v)
		}
	}
	if s := Col("state").IsIn("CA", 2).String(); !strings.HasSuffix(s, `.is_in(["CA", 2])`) {
		t.Errorf("String = %s", s)
	}
}

func TestIsIn_CanPossiblyMatch(t *testing.T) {
	s := intStats("id", 10, 20)
	for name, tc := range map[string]struct {
		e    Expr
		want bool
	}{
		"value inside":      {Col("id").IsIn(1, 15), true},
		"all outside":       {Col("id").IsIn(1, 9, 21, 1000), false},
		"edge":              {Col("id").IsIn(20), true},
		"empty":             {Col("id").IsIn(), false},
		"time: unknown":     {Col("id").IsIn(time.Unix(0, 0)), true},
		"other column":      {Col("other").IsIn(1), true},
		"or with a match":   {Col("id").IsIn(5).Or(Col("id").Gt(Lit(int64(19)))), true},
		"and with no match": {Col("id").IsIn(15).And(Col("id").IsIn(5)), false},
	} {
		if got := CanPossiblyMatch(tc.e, s); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		e    Expr
		want bool
	}{
		"many sorted, none inside": {Col("id").IsIn(1, 2, 3, 21, 22, 9, 1e9), false},
		"float inside":             {Col("id").IsIn(14.5), true},
		"NaN vs int stats":         {Col("id").IsIn(math.NaN()), false},
		"string vs int stats":      {Col("id").IsIn("15"), true},
	} {
		if got := CanPossiblyMatch(tc.e, s); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	// float32 stats: compared with the values rounded to float32.
	f32 := &fakeStats{minV: map[string]any{"f": float32(0.1)}, maxV: map[string]any{"f": float32(0.1)}, nulls: map[string]int64{"f": 0}, total: 10}
	if !CanPossiblyMatch(Col("f").IsIn(0.1), f32) || CanPossiblyMatch(Col("f").IsIn(0.2), f32) {
		t.Error("float32 pruning wrong")
	}
	// NaN: min/max ignore NaN rows, so a float group can't be skipped.
	f64 := &fakeStats{minV: map[string]any{"f": 1.0}, maxV: map[string]any{"f": 3.0}, nulls: map[string]int64{"f": 0}, total: 10}
	if !CanPossiblyMatch(Col("f").IsIn(math.NaN(), 5.0), f64) || !CanPossiblyMatch(Col("f").IsIn(math.NaN()), f32) {
		t.Error("NaN value pruned a float row group")
	}
	// Unsigned stats beyond int range.
	u := &fakeStats{minV: map[string]any{"u": uint32(5)}, maxV: map[string]any{"u": uint32(3_000_000_000)}, nulls: map[string]int64{"u": 0}, total: 10}
	if !CanPossiblyMatch(Col("u").IsIn(uint32(5)), u) || !CanPossiblyMatch(Col("u").IsIn(int64(2_999_999_999)), u) {
		t.Error("uint32 pruning wrong")
	}
	bs := &fakeStats{minV: map[string]any{"b": true}, maxV: map[string]any{"b": true}, nulls: map[string]int64{"b": 0}, total: 10}
	if CanPossiblyMatch(Col("b").IsIn(false), bs) || !CanPossiblyMatch(Col("b").IsIn(true), bs) {
		t.Error("bool pruning wrong")
	}
	str := &fakeStats{minV: map[string]any{"s": "b"}, maxV: map[string]any{"s": "d"}, nulls: map[string]int64{"s": 0}, total: 10}
	if CanPossiblyMatch(Col("s").IsIn("a", "e"), str) || !CanPossiblyMatch(Col("s").IsIn("a", "c"), str) {
		t.Error("string pruning wrong")
	}
}

func TestIsIn_SQL(t *testing.T) {
	sql, args, ok := ExprToSQL(Col("state").IsIn("CA", "NV").And(Col("n").Gt(Lit(int64(1)))))
	if !ok || sql != `(("state" IN (?, ?)) AND ("n" > ?))` || !slices.Equal(args, []any{"CA", "NV", int64(1)}) {
		t.Errorf("sql %q args %v ok %v", sql, args, ok)
	}
	// Empty list: false, but null stays null (and so under NOT).
	if sql, _, ok := ExprToSQL(Col("x").IsIn().Not()); !ok || sql != `NOT ((CASE WHEN "x" IS NULL THEN NULL ELSE (1 = 0) END))` {
		t.Errorf("empty: %q %v", sql, ok)
	}
	// Only values a database compares as Eval does are pushed.
	for name, e := range map[string]Expr{
		"nil":           Col("x").IsIn(nil),
		"float":         Col("x").IsIn(0.1),
		"time":          Col("x").IsIn(time.Unix(0, 0)),
		"uint64 hi bit": Col("x").IsIn(uint64(math.MaxUint64)),
	} {
		if _, _, ok := ExprToSQL(e); ok {
			t.Errorf("%s: pushed to SQL", name)
		}
	}
	if _, args, ok := ExprToSQL(Col("x").IsIn(int8(3), uint32(4), true)); !ok || !slices.Equal(args, []any{int64(3), int64(4), true}) {
		t.Errorf("ints/bools: args %v ok %v", args, ok)
	}
}

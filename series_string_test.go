package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// stringSeries builds a single-chunk String Series from a []any
// where nil entries produce null cells and string entries produce
// value cells. Used by every string-op test.
func stringSeries(t *testing.T, name string, vals []any) Series {
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
	field := arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	return Series{name: field.Name, field: field, col: col}
}

// int64SeriesValues extracts an Int64 series as []any (nil for null).
// Companion to stringSeriesValues + boolSeriesValues for the length ops.
func int64SeriesValues(t *testing.T, s Series) []any {
	t.Helper()
	out := make([]any, 0, s.Len())
	for _, chunk := range s.Column().Data().Chunks() {
		a := chunk.(*array.Int64)
		for i := range a.Len() {
			if a.IsNull(i) {
				out = append(out, nil)
				continue
			}
			out = append(out, a.Value(i))
		}
	}
	return out
}

func TestSeries_StrLower(t *testing.T) {
	s := stringSeries(t, "s", []any{"Hello", "WORLD", nil, "MiXeD"})
	got, err := s.StrLower()
	if err != nil {
		t.Fatal(err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"hello", "world", nil, "mixed"}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_StrUpper(t *testing.T) {
	s := stringSeries(t, "s", []any{"hello", nil, "MiXeD"})
	got, err := s.StrUpper()
	if err != nil {
		t.Fatal(err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"HELLO", nil, "MIXED"}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_StrTrim(t *testing.T) {
	s := stringSeries(t, "s", []any{"  hello  ", "\tworld\n", "no trim", nil})
	got, err := s.StrTrim()
	if err != nil {
		t.Fatal(err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"hello", "world", "no trim", nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %q, want %q", i, vals[i], w)
		}
	}
}

func TestSeries_StrTrimLeftRight(t *testing.T) {
	s := stringSeries(t, "s", []any{"xxhelloxx", "yyyworld"})
	left, err := s.StrTrimLeft("xy")
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.StrTrimRight("xy")
	if err != nil {
		t.Fatal(err)
	}
	if v := stringSeriesValues(t, left); v[0] != "helloxx" || v[1] != "world" {
		t.Errorf("TrimLeft: got %v", v)
	}
	if v := stringSeriesValues(t, right); v[0] != "xxhello" || v[1] != "yyyworld" {
		t.Errorf("TrimRight: got %v", v)
	}
}

func TestSeries_StrLen(t *testing.T) {
	// Include a multi-byte UTF-8 string to verify codepoint counting
	// (byte length would give a different answer).
	s := stringSeries(t, "s", []any{"hello", "café", "日本語", nil})
	got, err := s.StrLen()
	if err != nil {
		t.Fatal(err)
	}
	vals := int64SeriesValues(t, got)
	want := []any{int64(5), int64(4), int64(3), nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_StrContains(t *testing.T) {
	s := stringSeries(t, "s", []any{"hello world", "goodbye", nil})
	got, err := s.StrContains("world")
	if err != nil {
		t.Fatal(err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_StrStartsAndEndsWith(t *testing.T) {
	s := stringSeries(t, "s", []any{"hello.txt", "notes.md", "diary.txt"})
	starts, err := s.StrStartsWith("hello")
	if err != nil {
		t.Fatal(err)
	}
	ends, err := s.StrEndsWith(".txt")
	if err != nil {
		t.Fatal(err)
	}
	if v := boolSeriesValues(t, starts); v[0] != true || v[1] != false || v[2] != false {
		t.Errorf("StartsWith: got %v", v)
	}
	if v := boolSeriesValues(t, ends); v[0] != true || v[1] != false || v[2] != true {
		t.Errorf("EndsWith: got %v", v)
	}
}

func TestSeries_StrReplace(t *testing.T) {
	s := stringSeries(t, "s", []any{"foo bar foo", "no match", nil})
	got, err := s.StrReplace("foo", "baz")
	if err != nil {
		t.Fatal(err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"baz bar baz", "no match", nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %q, want %q", i, vals[i], w)
		}
	}
}

func TestSeries_StrSlice(t *testing.T) {
	s := stringSeries(t, "s", []any{"hello", "abcdef", "日本語"})

	// Positive range.
	pos, err := s.StrSlice(1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if v := stringSeriesValues(t, pos); v[0] != "ell" || v[1] != "bcd" || v[2] != "本語" {
		t.Errorf("StrSlice(1, 4): got %v", v)
	}

	// end=0 → "to end" convention.
	toEnd, err := s.StrSlice(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := stringSeriesValues(t, toEnd); v[0] != "llo" || v[1] != "cdef" {
		t.Errorf("StrSlice(2, 0): got %v", v)
	}

	// Negative indices.
	neg, err := s.StrSlice(-3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := stringSeriesValues(t, neg); v[0] != "llo" || v[1] != "def" {
		t.Errorf("StrSlice(-3, 0): got %v", v)
	}

	// Out-of-range clamp.
	over, err := s.StrSlice(100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if v := stringSeriesValues(t, over); v[0] != "" || v[1] != "" {
		t.Errorf("StrSlice(100, 200) clamp: got %v", v)
	}
}

func TestSeries_StrConcat(t *testing.T) {
	s := stringSeries(t, "s", []any{"hello", "world", nil})
	got, err := s.StrConcat("!!!")
	if err != nil {
		t.Fatal(err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"hello!!!", "world!!!", nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %q, want %q", i, vals[i], w)
		}
	}
}

func TestSeries_StrRegexMatch(t *testing.T) {
	s := stringSeries(t, "s", []any{"abc123", "no numbers", "42 is the answer"})
	got, err := s.StrRegexMatch(`\d+`)
	if err != nil {
		t.Fatal(err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, true}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_StrRegexReplace(t *testing.T) {
	s := stringSeries(t, "s", []any{"abc123def", "foo42bar"})
	got, err := s.StrRegexReplace(`\d+`, "X")
	if err != nil {
		t.Fatal(err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"abcXdef", "fooXbar"}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %q, want %q", i, vals[i], w)
		}
	}

	// Capture-group replacement.
	got2, err := s.StrRegexReplace(`([a-z]+)(\d+)`, "$2-$1")
	if err != nil {
		t.Fatal(err)
	}
	vals2 := stringSeriesValues(t, got2)
	if vals2[0] != "123-abcdef" {
		t.Errorf("capture-group replace: got %v", vals2[0])
	}
}

func TestSeries_StrRegexMatch_BadPatternErrors(t *testing.T) {
	s := stringSeries(t, "s", []any{"anything"})
	if _, err := s.StrRegexMatch(`[invalid`); err == nil {
		t.Errorf("expected error for invalid regex pattern")
	}
}

func TestSeries_StrLower_NonStringColumnErrors(t *testing.T) {
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.Append(1)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: false}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	s := Series{name: field.Name, field: field, col: col}

	if _, err := s.StrLower(); err == nil {
		t.Errorf("StrLower on Int64 column should error")
	}
}

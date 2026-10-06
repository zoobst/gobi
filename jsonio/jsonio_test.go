package jsonio_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/jsonio"
)

func read(t *testing.T, in string, opts *jsonio.ReadOptions) *gobi.Frame {
	t.Helper()
	f, err := jsonio.Read(strings.NewReader(in), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Release)
	return f
}

func col(t *testing.T, f *gobi.Frame, name string) gobi.Series {
	t.Helper()
	s, err := f.Column(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRead_TypeInference(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.NewGoAllocator())
	defer mem.AssertSize(t, 0)

	in := `[
	  {"id": 9007199254740993, "score": 1, "ok": true, "name": "a", "mixed": 1, "big": 18446744073709551616, "nest": {"b": [1, 2]}, "none": null},
	  {"id": -5, "score": 2.5, "ok": false, "name": "00123", "mixed": "x", "big": 1, "nest": [ 1 ]},
	  {"score": 1e3, "ok": null, "mixed": true, "big": 2}
	]`
	f, err := jsonio.Read(strings.NewReader(in), &jsonio.ReadOptions{Allocator: mem})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	if !slices.Equal(f.ColumnNames(), []string{"id", "score", "ok", "name", "mixed", "big", "nest", "none"}) {
		t.Fatalf("names %v", f.ColumnNames())
	}
	want := map[string]arrow.Type{
		"id": arrow.INT64, "score": arrow.FLOAT64, "ok": arrow.BOOL, "name": arrow.STRING,
		"mixed": arrow.STRING, "big": arrow.STRING, "nest": arrow.STRING, "none": arrow.STRING,
	}
	for name, id := range want {
		if got := col(t, f, name).DataType().ID(); got != id {
			t.Errorf("%s: type %s, want %s", name, got, id)
		}
	}

	// Above 2^53: exact, not rounded through float64.
	if ids, _ := col(t, f, "id").Int64s(); ids[0] != 9007199254740993 || ids[1] != -5 || !col(t, f, "id").Nulls()[2] {
		t.Errorf("id = %v nulls %v", ids, col(t, f, "id").Nulls())
	}
	if v, _ := col(t, f, "score").Float64s(); !slices.Equal(v, []float64{1, 2.5, 1000}) {
		t.Errorf("score = %v", v)
	}
	if v, _ := col(t, f, "ok").Bools(); !v[0] || v[1] || !col(t, f, "ok").Nulls()[2] {
		t.Errorf("ok = %v", v)
	}
	for name, wantVals := range map[string][]string{
		"name":  {"a", "00123", ""},
		"mixed": {"1", "x", "true"},
		"big":   {"18446744073709551616", "1", "2"},
		"nest":  {`{"b":[1,2]}`, `[1]`, ""},
	} {
		if v, _ := col(t, f, name).Strings(); !slices.Equal(v, wantVals) {
			t.Errorf("%s = %q, want %q", name, v, wantVals)
		}
	}
	if col(t, f, "none").NullCount() != 3 {
		t.Errorf("none: %d nulls, want 3", col(t, f, "none").NullCount())
	}
}

func TestRead_NDJSON(t *testing.T) {
	in := "{\"a\": 1}\n\n{\"b\": \"x\", \"a\": 3}\n{}\n"
	f := read(t, in, nil)
	if !slices.Equal(f.ColumnNames(), []string{"a", "b"}) || f.NumRows() != 3 {
		t.Fatalf("names %v rows %d", f.ColumnNames(), f.NumRows())
	}
	if v, _ := col(t, f, "a").Int64s(); !slices.Equal(v, []int64{1, 3, 0}) || !col(t, f, "a").Nulls()[2] {
		t.Errorf("a = %v nulls %v", v, col(t, f, "a").Nulls())
	}
	if n := col(t, f, "b").Nulls(); !slices.Equal(n, []bool{true, false, true}) {
		t.Errorf("b nulls %v", n)
	}

	// A single object reads as one NDJSON row.
	if f := read(t, `  {"a": 1}`, nil); f.NumRows() != 1 {
		t.Errorf("single object: %d rows", f.NumRows())
	}
}

func TestRead_AllStrings(t *testing.T) {
	f := read(t, `[{"n": 1.50, "i": 12, "b": true, "s": "x", "o": {"k": null}}, {"n": null}]`, &jsonio.ReadOptions{AllStrings: true})
	for name, want := range map[string]string{"n": "1.50", "i": "12", "b": "true", "s": "x", "o": `{"k":null}`} {
		s := col(t, f, name)
		if s.DataType().ID() != arrow.STRING {
			t.Errorf("%s: type %s", name, s.DataType())
		}
		if v, _ := s.Strings(); v[0] != want || !s.Nulls()[1] {
			t.Errorf("%s = %q nulls %v, want %q", name, v, s.Nulls(), want)
		}
	}
}

func TestRead_EmptyAndErrors(t *testing.T) {
	for _, in := range []string{"", "  \n", "[]", " [ ] "} {
		if f := read(t, in, nil); f.NumCols() != 0 || f.NumRows() != 0 {
			t.Errorf("%q: %d cols %d rows", in, f.NumCols(), f.NumRows())
		}
	}

	for in, wantNotObject := range map[string]bool{
		`[{"a": 1}, 2]`:     true,
		"{\"a\": 1}\n[1]\n": true,
		`[{"a": 1}] {}`:     false,
		`[{"a": 1}`:         false,
		`{"a": }`:           false,
		`{"a": 1, "a": 2}`:  false, // duplicate key
		"{\"a\": \"\xff\"}": false, // invalid UTF-8
	} {
		_, err := jsonio.Read(strings.NewReader(in), nil)
		if err == nil || errors.Is(err, jsonio.ErrNotObject) != wantNotObject {
			t.Errorf("%q: err = %v (want ErrNotObject: %v)", in, err, wantNotObject)
		}
	}

	// Forcing a format that doesn't match is an error.
	for _, in := range []string{`{"a": 1}`, ""} {
		if _, err := jsonio.Read(strings.NewReader(in), &jsonio.ReadOptions{Format: jsonio.FormatArray}); err == nil {
			t.Errorf("FormatArray on %q: no error", in)
		}
	}
	if _, err := jsonio.Read(strings.NewReader(`[{"a": 1}]`), &jsonio.ReadOptions{Format: jsonio.FormatNDJSON}); !errors.Is(err, jsonio.ErrNotObject) {
		t.Errorf("FormatNDJSON on array: err = %v", err)
	}
}

func TestReadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rows.ndjson")
	if err := os.WriteFile(path, []byte("{\"id\": 1}\n{\"id\": 2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := jsonio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if v, _ := col(t, f, "id").Int64s(); !slices.Equal(v, []int64{1, 2}) {
		t.Errorf("id = %v", v)
	}
}

// TestRead_FloatColumnKeepsIntegersExact — a fractional value makes a
// numeric column Float64 only if its integers survive the conversion;
// otherwise it falls back to String rather than rounding an ID.
func TestRead_FloatColumnKeepsIntegersExact(t *testing.T) {
	f := read(t, "{\"id\": 9007199254740993}\n{\"id\": 1.5}\n", nil)
	s := col(t, f, "id")
	if v, _ := s.Strings(); s.DataType().ID() != arrow.STRING || !slices.Equal(v, []string{"9007199254740993", "1.5"}) {
		t.Errorf("inexact int: %s %q", s.DataType(), v)
	}

	// 2^53 and 2^60 are exact in float64: Float64 is fine.
	f = read(t, "{\"x\": 9007199254740992}\n{\"x\": 1152921504606846976}\n{\"x\": 0.5}\n", nil)
	s = col(t, f, "x")
	if v, _ := s.Float64s(); s.DataType().ID() != arrow.FLOAT64 || !slices.Equal(v, []float64{1 << 53, 1 << 60, 0.5}) {
		t.Errorf("exact ints: %s %v", s.DataType(), v)
	}

	// Number past float64's range: String, as written.
	f = read(t, "{\"x\": 1e400}\n{\"x\": 2.5}\n", nil)
	if s := col(t, f, "x"); s.DataType().ID() != arrow.STRING {
		t.Errorf("1e400: %s", s.DataType())
	}
}

func TestRead_EscapesAndEmptyObjects(t *testing.T) {
	f := read(t, `[{"ab": "x\tyé", "q\"k": 1}, {"ab": "😀"}]`, nil)
	if !slices.Equal(f.ColumnNames(), []string{"ab", `q"k`}) {
		t.Fatalf("names %q", f.ColumnNames())
	}
	if v, _ := col(t, f, "ab").Strings(); !slices.Equal(v, []string{"x\tyé", "😀"}) {
		t.Errorf("ab = %q", v)
	}

	// Rows with no keys can't show up in a column-less Frame.
	if f := read(t, "{}\n{}\n{}\n", nil); f.NumRows() != 0 || f.NumCols() != 0 {
		t.Errorf("empty objects: %d rows %d cols", f.NumRows(), f.NumCols())
	}
	// ...but count once any key appears.
	if f := read(t, "{}\n{\"a\": 1}\n{}\n", nil); f.NumRows() != 3 {
		t.Errorf("mostly empty: %d rows", f.NumRows())
	}
}

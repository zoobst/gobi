package csvio_test

import (
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
)

// colStrings returns a column's values with null rows as "<null>".
func colStrings(t *testing.T, f *gobi.Frame, name string) []string {
	t.Helper()
	s, err := f.Column(name)
	if err != nil {
		t.Fatal(err)
	}
	if s.DataType().ID() != arrow.STRING {
		t.Fatalf("column %q is %s, want utf8", name, s.DataType())
	}
	vals, err := s.Strings()
	if err != nil {
		t.Fatal(err)
	}
	for i, null := range s.Nulls() {
		if null {
			vals[i] = "<null>"
		}
	}
	return vals
}

func TestReadStrings_NoInference(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.NewGoAllocator())
	defer mem.AssertSize(t, 0)

	in := "id;amount;note\n00123;1e5;\"a;b\nc\"\n# skipped\n;NA;true\n"
	f, err := csvio.ReadStrings(strings.NewReader(in), &csvio.ReadOptions{
		Delimiter: ';', Comment: '#', NullTokens: []string{"NA"}, Allocator: mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if !slices.Equal(f.ColumnNames(), []string{"id", "amount", "note"}) {
		t.Fatalf("names %v", f.ColumnNames())
	}
	for name, want := range map[string][]string{
		"id":     {"00123", "<null>"},
		"amount": {"1e5", "<null>"},
		"note":   {"a;b\nc", "true"},
	} {
		if got := colStrings(t, f, name); !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestReadStrings_HeaderRules(t *testing.T) {
	no := false
	f, err := csvio.ReadStrings(strings.NewReader("1,2\n3,4\n"), &csvio.ReadOptions{HasHeader: &no})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.ColumnNames(), []string{"f0", "f1"}) || !slices.Equal(colStrings(t, f, "f0"), []string{"1", "3"}) {
		t.Errorf("no header: %v %v", f.ColumnNames(), colStrings(t, f, "f0"))
	}
	f.Release()

	// Byte-order mark dropped; a blank header cell is named by index.
	f, err = csvio.ReadStrings(strings.NewReader("\ufeffa,,c\n1,2,3\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.ColumnNames(), []string{"a", "f1", "c"}) {
		t.Errorf("names %q", f.ColumnNames())
	}
	f.Release()

	// Byte-order mark before a quoted header (Excel's CSV UTF-8),
	// through the struct reader too.
	bomQuoted := "\ufeff\"id\",\"n\"\n\"00123\",4\n"
	f, err = csvio.ReadStrings(strings.NewReader(bomQuoted), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.ColumnNames(), []string{"id", "n"}) || !slices.Equal(colStrings(t, f, "id"), []string{"00123"}) {
		t.Errorf("quoted BOM header: %q %q", f.ColumnNames(), colStrings(t, f, "id"))
	}
	f.Release()
	type row struct {
		ID string `csv:"id"`
		N  int64  `csv:"n"`
	}
	tf, err := csvio.Read[row](strings.NewReader(bomQuoted), nil)
	if err != nil {
		t.Fatalf("typed Read, quoted BOM header: %v", err)
	}
	tf.Release()

	// Header only: columns, zero rows.
	f, err = csvio.ReadStrings(strings.NewReader("a,b\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.NumRows() != 0 || !slices.Equal(f.ColumnNames(), []string{"a", "b"}) {
		t.Errorf("header only: %d rows %v", f.NumRows(), f.ColumnNames())
	}
	f.Release()

	for in, want := range map[string]error{
		"":                csvio.ErrHeaderMissing,
		"a,b,a\n1,2,3\n":  csvio.ErrDuplicateHeader,
		"a,b\n1,2\n3\n":   csvio.ErrRowFieldCountMismatch,
		"a,b\n1,2,3\n4\n": csvio.ErrRowFieldCountMismatch,
	} {
		if _, err := csvio.ReadStrings(strings.NewReader(in), nil); !errors.Is(err, want) {
			t.Errorf("%q: err = %v, want %v", in, err, want)
		}
	}
}

func TestReadStringsChunksFunc(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.NewGoAllocator())
	defer mem.AssertSize(t, 0)

	for in, want := range map[string][]int{
		"x\n1\n2\n3\n4\n5\n": {2, 2, 1},
		"x\n1\n2\n3\n4\n":    {2, 2},
		"x\n":                {0},
	} {
		var sizes []int
		var got []string
		err := csvio.ReadStringsChunksFunc(strings.NewReader(in), &csvio.ReadOptions{ChunkRows: 2, Allocator: mem}, func(f *gobi.Frame) error {
			sizes = append(sizes, f.NumRows())
			got = append(got, colStrings(t, f, "x")...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sizes, want) || len(got) != len(strings.Split(in, "\n"))-2 {
			t.Errorf("%q: chunk sizes %v (want %v), values %v", in, sizes, want, got)
		}
	}

	stop := errors.New("stop")
	err := csvio.ReadStringsChunksFunc(strings.NewReader("x\n1\n2\n3\n"), &csvio.ReadOptions{ChunkRows: 1, Allocator: mem}, func(*gobi.Frame) error {
		return stop
	})
	if !errors.Is(err, csvio.ErrChunksAborted) || !errors.Is(err, stop) {
		t.Errorf("abort err = %v", err)
	}
}

func TestReadFileStrings_Gzip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ids.csv.gz")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(out)
	if _, err := zw.Write([]byte("id\n00042\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := csvio.ReadFileStrings(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if got := colStrings(t, f, "id"); !slices.Equal(got, []string{"00042"}) {
		t.Errorf("id = %q", got)
	}

	n := 0
	if err := csvio.ReadFileStringsChunksFunc(path, nil, func(f *gobi.Frame) error {
		n += f.NumRows()
		return nil
	}); err != nil || n != 1 {
		t.Errorf("chunks: %d rows, %v", n, err)
	}
}

package csvio_test

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
	"github.com/zoobst/gobi/geometry"
)

const citiesCSV = `name,population,geometry
New York,8804190,POINT (-74.0060 40.7128)
Los Angeles,3898747,POINT (-118.2437 34.0522)
Chicago,2746388,POINT (-87.6298 41.8781)
`

type city struct {
	Name       string `csv:"name"`
	Population int64  `csv:"population"`
	Geom       string `csv:"geometry" geom:"true"`
}

func TestRead_Cities(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := df.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("shape got (%d, %d) want (3, 3)", rows, cols)
	}
	names := df.ColumnNames()
	if names[0] != "name" || names[1] != "population" || names[2] != "geometry" {
		t.Fatalf("names: %v", names)
	}

	g, err := df.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("expected Point, got %T", g)
	}
	if p.X > -74 || p.X < -74.01 {
		t.Fatalf("X = %v", p.X)
	}
}

func TestReadFile_Cities(t *testing.T) {
	df, err := csvio.ReadFile[city]("../testdata/cities.csv", &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := df.Shape()
	if rows != 5 {
		t.Fatalf("rows = %d, want 5", rows)
	}
}

// makeSyntheticCSV builds a CSV with n rows and 4 columns (name, i, f, note).
func makeSyntheticCSV(n int) string {
	var b strings.Builder
	b.WriteString("name,i,f,note\n")
	for r := range n {
		fmt.Fprintf(&b, "row-%d,%d,%d.5,note-%d\n", r, r*7, r, r%128)
	}
	return b.String()
}

type basicRow struct {
	Name string  `csv:"name"`
	I    int64   `csv:"i"`
	F    float64 `csv:"f"`
	Note string  `csv:"note"`
}

func TestReadChunksFunc_ChunkSize(t *testing.T) {
	// 25k rows with ChunkRows=10k should yield 3 chunks (10k + 10k + 5k).
	src := makeSyntheticCSV(25_000)
	var chunkCount int
	var totalRows int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.ReadOptions{ChunkRows: 10_000},
		func(f *gobi.Frame) error {
			chunkCount++
			totalRows += f.NumRows()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if chunkCount != 3 {
		t.Fatalf("chunk count = %d, want 3", chunkCount)
	}
	if totalRows != 25_000 {
		t.Fatalf("total rows across chunks = %d, want 25_000", totalRows)
	}
}

func TestReadChunksFunc_CallbackErrorAborts(t *testing.T) {
	src := makeSyntheticCSV(10_000)
	sentinel := errors.New("boom")
	var invocations int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.ReadOptions{ChunkRows: 1_000},
		func(f *gobi.Frame) error {
			invocations++
			if invocations == 2 {
				return sentinel
			}
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected error from aborted callback")
	}
	if !errors.Is(err, csvio.ErrChunksAborted) {
		t.Fatalf("want ErrChunksAborted in the chain, got %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("original callback error should be in the chain: %v", err)
	}
	if invocations != 2 {
		t.Fatalf("callback invocations = %d, want 2 (stopped after error)", invocations)
	}
}

func TestReadChunksFunc_DataIntegrityAcrossChunks(t *testing.T) {
	// Confirm each row's values are preserved in-order across chunks.
	const n = 5_000
	src := makeSyntheticCSV(n)
	var rowIdx int64
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.ReadOptions{ChunkRows: 500},
		func(f *gobi.Frame) error {
			iCol, _ := f.Column("i")
			arr := iCol.Column().Data().Chunks()[0].(*array.Int64)
			for i := 0; i < arr.Len(); i++ {
				want := rowIdx * 7
				if arr.Value(i) != want {
					return fmt.Errorf("row %d i = %d, want %d", rowIdx, arr.Value(i), want)
				}
				rowIdx++
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if rowIdx != n {
		t.Fatalf("saw %d rows, want %d", rowIdx, n)
	}
}

func TestReadChunksFunc_RetainAcrossCallback(t *testing.T) {
	// A callback that retains a Frame must be able to read from it after
	// the callback returns.
	src := makeSyntheticCSV(2_000)
	var kept []*gobi.Frame
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.ReadOptions{ChunkRows: 500},
		func(f *gobi.Frame) error {
			f.Retain()
			kept = append(kept, f)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) == 0 {
		t.Fatal("no frames retained")
	}
	// Access the buffers of a retained frame — this should not crash.
	last := kept[len(kept)-1]
	iCol, _ := last.Column("i")
	arr := iCol.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Len() == 0 {
		t.Fatal("retained frame lost its data")
	}
	// Match retains with releases so we don't leak.
	for _, f := range kept {
		f.Release()
	}
}

func TestReadFileChunksFunc_GzipAutoDetect(t *testing.T) {
	// End-to-end: gzipped CSV on disk, streaming callback, geometry column.
	dir := t.TempDir()
	path := filepath.Join(dir, "cities.csv.gz")
	src := `name,population,geometry
New York,8804190,POINT (-74.0060 40.7128)
Los Angeles,3898747,POINT (-118.2437 34.0522)
Chicago,2746388,POINT (-87.6298 41.8781)
`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte(src))
	gw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	var totalRows int
	err := csvio.ReadFileChunksFunc[city](path, &csvio.ReadOptions{CRSHint: 4326},
		func(f *gobi.Frame) error {
			totalRows += f.NumRows()
			// Sanity: geometry column is decoded.
			g, err := f.Geometry("geometry", 0)
			if err != nil {
				return err
			}
			if g == nil {
				return errors.New("geometry column decoded as nil")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if totalRows != 3 {
		t.Fatalf("total rows = %d, want 3", totalRows)
	}
}

func TestReadChunksFunc_EmptyInput(t *testing.T) {
	// Only a header, no data rows: fn should never be called, no error.
	var called int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader("name,i,f,note\n"), nil,
		func(f *gobi.Frame) error {
			called++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("fn called %d times on empty input, want 0", called)
	}
}

// bounded-memory smoke test: process a large synthetic CSV in small chunks
// without accumulating.
func TestReadChunksFunc_BoundedMemoryLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large memory test in short mode")
	}
	src := makeSyntheticCSV(200_000)
	var total int
	err := csvio.ReadChunksFunc[basicRow](strings.NewReader(src),
		&csvio.ReadOptions{ChunkRows: 8_192},
		func(f *gobi.Frame) error {
			total += f.NumRows()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if total != 200_000 {
		t.Fatalf("total rows = %d, want 200_000", total)
	}
}

// discardWriter is a small helper used by nothing here, kept to catch
// import drift if we later expand the streaming tests.
var _ = io.Discard

type event struct {
	Name string    `csv:"name"`
	When time.Time `csv:"when" time:"2006-01-02 15:04:05"`
	Seq  int64     `csv:"seq"`
}

const eventsCSV = `name,when,seq
launch,2026-01-15 09:30:00,1
demo,2026-03-22 14:00:00,2
release,2026-07-04 00:00:00,3
`

func TestRead_TimeColumn_WithExplicitLayout(t *testing.T) {
	df, err := csvio.Read[event](strings.NewReader(eventsCSV), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := df.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("shape got (%d, %d) want (3, 3)", rows, cols)
	}

	when, _ := df.Column("when")
	if !when.IsDateTime() {
		t.Fatal("when column not tagged as datetime")
	}
	got, ok, err := when.TimeAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("row 1 unexpectedly null")
	}
	want := time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("row 1 = %v, want %v", got, want)
	}
}

func TestRead_TimeColumn_DefaultLayoutsFallback(t *testing.T) {
	type simpleEvt struct {
		When time.Time `csv:"when"`
	}
	// Feed an RFC3339 timestamp without an explicit layout — should fall
	// back to DefaultTimeLayouts.
	src := "when\n2026-05-01T12:34:56Z\n"
	df, err := csvio.Read[simpleEvt](strings.NewReader(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	when, _ := df.Column("when")
	got, _, _ := when.TimeAt(0)
	want := time.Date(2026, 5, 1, 12, 34, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("default-layout parse = %v, want %v", got, want)
	}
}

func TestRead_TimeParseFailure(t *testing.T) {
	src := "when\nnot-a-date\n"
	type e struct {
		When time.Time `csv:"when"`
	}
	_, err := csvio.Read[e](strings.NewReader(src), nil)
	if !errors.Is(err, csvio.ErrTimeParse) {
		t.Fatalf("want ErrTimeParse, got %v", err)
	}
}

func TestReadFile_EventsFixture(t *testing.T) {
	df, err := csvio.ReadFile[event]("../testdata/events.csv", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := df.Shape()
	if rows != 5 {
		t.Fatalf("rows = %d, want 5", rows)
	}
	when, _ := df.Column("when")
	// Row 4 (index 4) is "2026-07-20 16:45:00".
	got, _, _ := when.TimeAt(4)
	want := time.Date(2026, 7, 20, 16, 45, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("row 4 = %v, want %v", got, want)
	}
}

type numericRow struct {
	Name string  `csv:"name"`
	N    int64   `csv:"n"`
	F    float64 `csv:"f"`
}

func TestOption_CustomDelimiter_TSV(t *testing.T) {
	src := "name\tn\tf\n" +
		"alpha\t1\t1.5\n" +
		"bravo\t2\t2.5\n"
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{
		Delimiter: '\t',
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := df.Shape(); r != 2 {
		t.Fatalf("rows = %d, want 2", r)
	}
	nCol, _ := df.Column("n")
	nArr := nCol.Column().Data().Chunks()[0].(*array.Int64)
	if nArr.Value(0) != 1 || nArr.Value(1) != 2 {
		t.Fatalf("int64 col: %v %v", nArr.Value(0), nArr.Value(1))
	}
}

func TestOption_CustomDelimiter_Semicolon(t *testing.T) {
	src := "name;n;f\n" +
		"alpha;1;1.5\n"
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{
		Delimiter: ';',
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := df.Shape(); r != 1 {
		t.Fatalf("rows = %d, want 1", r)
	}
}

func TestOption_NullTokens(t *testing.T) {
	src := "name,n,f\n" +
		"alpha,1,1.5\n" +
		"bravo,NA,NULL\n" +
		"charlie,,\n"
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{
		NullTokens: []string{"NA", "NULL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	nCol, _ := df.Column("n")
	nArr := nCol.Column().Data().Chunks()[0].(*array.Int64)
	if nArr.Value(0) != 1 {
		t.Fatalf("row 0 = %v, want 1", nArr.Value(0))
	}
	if !nArr.IsNull(1) {
		t.Fatal("row 1 should be null (NA token)")
	}
	if !nArr.IsNull(2) {
		t.Fatal("row 2 should be null (empty string)")
	}
	fCol, _ := df.Column("f")
	fArr := fCol.Column().Data().Chunks()[0].(*array.Float64)
	if !fArr.IsNull(1) {
		t.Fatal("row 1 float should be null (NULL token)")
	}
}

func TestOption_Comment(t *testing.T) {
	src := "name,n,f\n" +
		"# skipped as a comment\n" +
		"alpha,1,1.5\n" +
		"# another comment\n" +
		"bravo,2,2.5\n"
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{
		Comment: '#',
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := df.Shape(); r != 2 {
		t.Fatalf("rows = %d, want 2 (comments skipped)", r)
	}
}

func TestOption_SkipRows(t *testing.T) {
	// Two junk rows before the header, then a valid header + 2 data rows.
	src := "junk-line-1\n" +
		"junk-line-2\n" +
		"name,n,f\n" +
		"alpha,1,1.5\n" +
		"bravo,2,2.5\n"
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{
		SkipRows: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := df.Shape(); r != 2 {
		t.Fatalf("rows = %d, want 2 (junk skipped)", r)
	}
}

func TestOption_LazyQuotes(t *testing.T) {
	// A cell containing an unescaped double-quote.
	src := `name,n,f
al"pha,1,1.5
`
	// Without LazyQuotes, arrow's csv reader rejects this input.
	if _, err := csvio.Read[numericRow](strings.NewReader(src), nil); err == nil {
		t.Fatal("expected strict-quote error without LazyQuotes")
	}
	// With LazyQuotes, the row parses.
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{
		LazyQuotes: true,
	})
	if err != nil {
		t.Fatalf("LazyQuotes: %v", err)
	}
	if r, _ := df.Shape(); r != 1 {
		t.Fatalf("rows = %d, want 1", r)
	}
}

func TestOption_ChunkRows_SmallBatchProducesSameOutput(t *testing.T) {
	// A tiny chunk size exercises the concatenate-per-column path across
	// many small record batches. Result should be identical to the
	// default batch size.
	src := "name,n,f\n" +
		"a,1,1.5\n" +
		"b,2,2.5\n" +
		"c,3,3.5\n" +
		"d,4,4.5\n" +
		"e,5,5.5\n"
	small, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.ReadOptions{ChunkRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	def, err := csvio.Read[numericRow](strings.NewReader(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := small.NumRows(), def.NumRows(); a != b || a != 5 {
		t.Fatalf("rows small=%d default=%d, want 5 both", a, b)
	}
	// Sanity: values agree row-by-row.
	sN, _ := small.Column("n")
	dN, _ := def.Column("n")
	sArr := sN.Column().Data().Chunks()[0].(*array.Int64)
	dArr := dN.Column().Data().Chunks()[0].(*array.Int64)
	for i := 0; i < 5; i++ {
		if sArr.Value(i) != dArr.Value(i) {
			t.Fatalf("row %d differs: small=%d default=%d", i, sArr.Value(i), dArr.Value(i))
		}
	}
}

// colJSON renders column name of df (one chunk) as JSON.
func colJSON(t *testing.T, df *gobi.Frame, name string) string {
	t.Helper()
	s, err := df.Column(name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Column().Data().Chunks()[0].MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type headerRow struct {
	ID   int64     `csv:"id"`
	Name string    `csv:"name"`
	When time.Time `csv:"when" time:"2006-01-02"`
	Geom string    `csv:"geom" geom:"true"`
}

// TestRead_MatchesHeaderByName — columns are found by header name, in
// any order, with extra columns ignored; the Frame keeps struct order.
func TestRead_MatchesHeaderByName(t *testing.T) {
	src := "\xef\xbb\xbfextra;geom;\"na\nme\";name;when;id\n" +
		"x;POINT (1 2);skip;alpha;2024-01-02;7\n" +
		"y;POINT (3 4);skip;bravo;2024-03-04;9\n"
	// The header cell "na\nme" is quoted across lines; Arrow has to start
	// right after it.
	opts := &csvio.ReadOptions{Delimiter: ';'}
	check := func(what string, df *gobi.Frame) {
		t.Helper()
		if got := strings.Join(df.ColumnNames(), ","); got != "id,name,when,geom" {
			t.Errorf("%s: columns = %s", what, got)
		}
		if got := colJSON(t, df, "id") + colJSON(t, df, "name"); got != `[7,9]["alpha","bravo"]` {
			t.Errorf("%s: id, name = %s", what, got)
		}
		g, err := df.Geometry("geom", 1)
		if err != nil {
			t.Fatal(err)
		}
		if p := g.(geometry.Point); p.X != 3 || p.Y != 4 {
			t.Errorf("%s: geom[1] = %v", what, p)
		}
	}

	df, err := csvio.Read[headerRow](strings.NewReader(src), opts)
	if err != nil {
		t.Fatal(err)
	}
	check("Read", df)
	df.Release()

	err = csvio.ReadChunksFunc[headerRow](strings.NewReader(src), &csvio.ReadOptions{Delimiter: ';', ChunkRows: 1},
		func(f *gobi.Frame) error {
			if got := strings.Join(f.ColumnNames(), ","); got != "id,name,when,geom" {
				t.Errorf("chunk columns = %s", got)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "h.csv")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	df, err = csvio.ScanFile[headerRow](path, opts).Collect()
	if err != nil {
		t.Fatal(err)
	}
	check("ScanFile", df)
	df.Release()
}

func TestRead_HeaderErrors(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      error
		contains  string
	}{
		{"missing", "id,geom,note\n1,POINT (1 2),x\n", csvio.ErrColumnMissing, `"name", "when"`},
		{"duplicate matched", "id,name,when,geom,id\n1,a,2024-01-02,POINT (1 2),2\n", csvio.ErrDuplicateHeader, `"id"`},
		{"empty", "", csvio.ErrHeaderMissing, ""},
	} {
		_, err := csvio.Read[headerRow](strings.NewReader(tc.src), nil)
		if !errors.Is(err, tc.want) || !strings.Contains(fmt.Sprint(err), tc.contains) {
			t.Errorf("%s: err = %v, want %v mentioning %s", tc.name, err, tc.want, tc.contains)
		}
	}
	// A duplicate among the ignored columns is fine.
	df, err := csvio.Read[headerRow](strings.NewReader("x,id,name,when,geom,x\n0,1,a,2024-01-02,POINT (1 2),0\n"), nil)
	if err != nil {
		t.Fatalf("duplicate unmatched column: %v", err)
	}
	df.Release()
}

// TestRead_MatchByPosition — the escape hatch reads columns in struct
// order whatever the header says, and needs one column per field.
func TestRead_MatchByPosition(t *testing.T) {
	src := "ID,Label,Date,Shape\n1,a,2024-01-02,POINT (1 2)\n"
	opts := &csvio.ReadOptions{MatchByPosition: true}
	df, err := csvio.Read[headerRow](strings.NewReader(src), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := colJSON(t, df, "id") + colJSON(t, df, "name"); got != `[1]["a"]` {
		t.Errorf("id, name = %s", got)
	}
	df.Release()
	if _, err := csvio.Read[headerRow](strings.NewReader(src), nil); !errors.Is(err, csvio.ErrColumnMissing) {
		t.Errorf("by name: err = %v, want ErrColumnMissing", err)
	}
	if _, err := csvio.Read[headerRow](strings.NewReader("ID,Label\n1,a\n"), opts); err == nil {
		t.Error("by position with too few columns: no error")
	}
}

// TestRead_HeaderOnlyByName — a header with no rows gives an empty
// Frame with the struct's columns.
func TestRead_HeaderOnlyByName(t *testing.T) {
	df, err := csvio.Read[headerRow](strings.NewReader("geom,when,name,id\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	if rows, cols := df.Shape(); rows != 0 || cols != 4 {
		t.Errorf("shape = (%d, %d), want (0, 4)", rows, cols)
	}
}

// TestOption_UseCRLF — the option is accepted (it used to panic) and
// CRLF input reads the same as LF input.
func TestOption_UseCRLF(t *testing.T) {
	df, err := csvio.Read[headerRow](strings.NewReader("id,name,when,geom\r\n1,a,2024-01-02,POINT (1 2)\r\n"), &csvio.ReadOptions{UseCRLF: true})
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	if got := colJSON(t, df, "id") + colJSON(t, df, "name"); got != `[1]["a"]` {
		t.Errorf("id, name = %s", got)
	}
}

// TestRead_ErrorLineNumbers — parse errors count lines from the start
// of the input, including skipped lines and a header spanning lines,
// whichever reader handled the header.
func TestRead_ErrorLineNumbers(t *testing.T) {
	type row struct {
		ID   int64  `csv:"id"`
		Name string `csv:"name"`
	}
	// Line 1 skipped, header on lines 2-3, rows on 4 and 5 (bad).
	src := "title\n\"x\ny\",id,name\n0,1,a\n0,2,b,extra\n"
	check := func(what string, err error) {
		t.Helper()
		var pe *csv.ParseError
		if !errors.As(err, &pe) || pe.Line != 5 || !errors.Is(err, csvio.ErrRowFieldCountMismatch) {
			t.Errorf("%s: err = %v, want a field-count error on line 5", what, err)
		}
	}
	opts := &csvio.ReadOptions{SkipRows: 1}
	_, err := csvio.Read[row](strings.NewReader(src), opts)
	check("Read", err)
	err = csvio.ReadChunksFunc[row](strings.NewReader(src), opts, func(*gobi.Frame) error { return nil })
	check("ReadChunksFunc", err)
	type byPos struct {
		X    string `csv:"x"`
		ID   int64  `csv:"id"`
		Name string `csv:"name"`
	}
	_, err = csvio.Read[byPos](strings.NewReader(src), &csvio.ReadOptions{SkipRows: 1, MatchByPosition: true})
	check("MatchByPosition", err)
	_, err = csvio.ReadStrings(strings.NewReader(src), opts)
	check("ReadStrings", err)
}

// TestRead_DuplicateFieldNames — two fields naming one column is an
// error when matching by name.
func TestRead_DuplicateFieldNames(t *testing.T) {
	type row struct {
		ID    int64  `csv:"id"`
		IDRaw string `csv:"id"`
	}
	if _, err := csvio.Read[row](strings.NewReader("id\n1\n"), nil); !errors.Is(err, csvio.ErrDuplicateField) {
		t.Errorf("err = %v, want ErrDuplicateField", err)
	}
}

// TestReadChunksFunc_EmptyFile — no input at all is ErrHeaderMissing,
// as in Read, whether columns match by name or by position.
func TestReadChunksFunc_EmptyFile(t *testing.T) {
	for _, opts := range []*csvio.ReadOptions{nil, {MatchByPosition: true}} {
		err := csvio.ReadChunksFunc[basicRow](strings.NewReader(""), opts, func(*gobi.Frame) error { return nil })
		if !errors.Is(err, csvio.ErrHeaderMissing) {
			t.Errorf("opts %+v: err = %v, want ErrHeaderMissing", opts, err)
		}
	}
	// Without a header, empty input is just no rows.
	noHeader := false
	if err := csvio.ReadChunksFunc[basicRow](strings.NewReader(""), &csvio.ReadOptions{HasHeader: &noHeader},
		func(*gobi.Frame) error { return nil }); err != nil {
		t.Errorf("no header: err = %v", err)
	}
}

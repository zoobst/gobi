package csvio_test

import (
	"bytes"
	"compress/gzip"
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
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("original callback error should be wrapped: %v", err)
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

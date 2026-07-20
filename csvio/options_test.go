package csvio_test

import (
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/csvio"
)

type numericRow struct {
	Name string  `csv:"name"`
	N    int64   `csv:"n"`
	F    float64 `csv:"f"`
}

func TestOption_CustomDelimiter_TSV(t *testing.T) {
	src := "name\tn\tf\n" +
		"alpha\t1\t1.5\n" +
		"bravo\t2\t2.5\n"
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{
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
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{
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
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{
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
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{
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
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{
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
	df, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{
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
	small, err := csvio.Read[numericRow](strings.NewReader(src), &csvio.Options{ChunkRows: 1})
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

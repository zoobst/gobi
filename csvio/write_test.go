package csvio_test

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
)

type fmtRow struct {
	S   string           `csv:"s"`
	I   int64            `csv:"i"`
	U   uint8            `csv:"u"`
	F   float64          `csv:"f"`
	F32 float32          `csv:"f32"`
	B   bool             `csv:"b"`
	N   gobi.Null[int64] `csv:"n"`
	Raw []byte           `csv:"raw"`
	T   time.Time        `csv:"t"`
}

// TestWrite_ScalarFormatsAndQuoting pins the exact output for the
// common types and encoding/csv's quoting rules.
func TestWrite_ScalarFormatsAndQuoting(t *testing.T) {
	rows := []fmtRow{
		{S: "plain", I: -3, U: 255, F: 1.5, F32: 0.1, B: true, N: gobi.Null[int64]{V: 7, Valid: true},
			Raw: []byte("hi"), T: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)},
		{S: "a,b \"q\"\nline", F: math.NaN(), F32: float32(math.Inf(1))},
		{S: " lead", I: 1, U: 1, F: math.Inf(-1)},
	}
	var buf bytes.Buffer
	if err := csvio.WriteStructsWriter(rows, &buf, nil); err != nil {
		t.Fatal(err)
	}
	want := "s,i,u,f,f32,b,n,raw,t\n" +
		"plain,-3,255,1.5,0.1,true,7,aGk=,2026-01-02T03:04:05.000006Z\n" +
		"\"a,b \"\"q\"\"\nline\",0,0,NaN,+Inf,false,,,\n" +
		"\" lead\",1,1,-Inf,0,false,,,\n"
	if got := buf.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

// TestWrite_ArrowTypes covers the types FromStructs doesn't produce:
// dates, decimals, durations, zoned timestamps, dictionaries (with a
// null entry), lists and times of day.
func TestWrite_ArrowTypes(t *testing.T) {
	pool := memory.DefaultAllocator
	var arrs []arrow.Array
	add := func(a arrow.Array) { arrs = append(arrs, a) }

	d := array.NewDate32Builder(pool)
	d.AppendValues([]arrow.Date32{19723, 0}, nil)
	add(d.NewArray())
	dec := array.NewDecimal128Builder(pool, &arrow.Decimal128Type{Precision: 10, Scale: 2})
	dec.AppendValues([]decimal128.Num{decimal128.FromI64(1250), decimal128.FromI64(-5)}, nil)
	add(dec.NewArray())
	dur := array.NewDurationBuilder(pool, &arrow.DurationType{Unit: arrow.Second})
	dur.AppendValues([]arrow.Duration{90, 0}, []bool{true, false})
	add(dur.NewArray())
	ts := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Second, TimeZone: "America/New_York"})
	ts.AppendValues([]arrow.Timestamp{0, 0}, []bool{true, false})
	add(ts.NewArray())
	dv := array.NewStringBuilder(pool)
	dv.AppendValues([]string{"x", ""}, []bool{true, false})
	dictVals := dv.NewArray()
	di := array.NewInt32Builder(pool)
	di.AppendValues([]int32{0, 1}, nil) // row 1 → null dictionary entry
	dictIdx := di.NewArray()
	add(array.NewDictionaryArray(&arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}, dictIdx, dictVals))
	dictVals.Release()
	dictIdx.Release()
	lb := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	lb.Append(true)
	lb.ValueBuilder().(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
	lb.AppendNull()
	add(lb.NewArray())
	tod := array.NewTime64Builder(pool, &arrow.Time64Type{Unit: arrow.Microsecond})
	tod.AppendValues([]arrow.Time64{3_723_000_001, 0}, nil)
	add(tod.NewArray())
	for _, b := range []array.Builder{d, dec, dur, ts, dv, di, lb, tod} {
		b.Release()
	}

	names := []string{"d", "dec", "dur", "ts", "dict", "list", "tod"}
	fields := make([]arrow.Field, len(arrs))
	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		fields[i] = arrow.Field{Name: names[i], Type: a.DataType(), Nullable: true}
		ch := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], ch)
		ch.Release()
		a.Release()
	}
	f, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	var buf bytes.Buffer
	if err := csvio.Write(f, &buf, &csvio.WriteOptions{NullValue: "NA"}); err != nil {
		t.Fatal(err)
	}
	want := "d,dec,dur,ts,dict,list,tod\n" +
		"2024-01-01,12.50,90,1969-12-31T19:00:00-05:00,x,\"[1,2]\",01:02:03.000001\n" +
		"1970-01-01,-0.05,NA,NA,NA,NA,00:00:00\n"
	if got := buf.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

type roundTripRow struct {
	ID   int64     `csv:"id"`
	Name string    `csv:"name"`
	Val  float64   `csv:"val"`
	OK   bool      `csv:"ok"`
	At   time.Time `csv:"at"`
	Geom string    `csv:"geometry" geom:"true"`
}

// TestWrite_RoundTripStructs — WriteStructs then ReadStructs gives the
// rows back, including WKT geometry and nanosecond timestamps.
func TestWrite_RoundTripStructs(t *testing.T) {
	rows := []roundTripRow{
		{ID: 1, Name: "a, \"quoted\"\nname", Val: 0.1, OK: true,
			At: time.Date(2026, 7, 1, 12, 0, 0, 123456789, time.UTC), Geom: "POINT (1 2)"},
		{ID: 2, Name: "plain", Val: math.MaxFloat64, At: time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC),
			Geom: "LINESTRING (0 0, 1 1)"},
	}
	path := filepath.Join(t.TempDir(), "rt.csv")
	if err := csvio.WriteStructs(rows, path, nil); err != nil {
		t.Fatal(err)
	}
	back, err := csvio.ReadStructs[roundTripRow](path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(rows) {
		t.Fatalf("rows = %d, want %d", len(back), len(rows))
	}
	for i := range rows {
		want, got := rows[i], back[i]
		if got.ID != want.ID || got.Name != want.Name || got.Val != want.Val || got.OK != want.OK ||
			!got.At.Equal(want.At) || got.Geom != want.Geom {
			t.Errorf("row %d: got %+v, want %+v", i, got, want)
		}
	}
}

// TestWrite_ReadStringsRoundTrip — cells needing quotes read back
// unchanged through ReadStrings, with a non-comma delimiter and CRLF.
func TestWrite_ReadStringsRoundTrip(t *testing.T) {
	cells := []string{"tab\there", "q\"uote", "multi\nline", "cr\r\nlf", " lead", `\.`, "semi;colon", "ünïcode"}
	b := array.NewStringBuilder(memory.DefaultAllocator)
	b.AppendValues(cells, nil)
	f := framesOf(t, "s", b.NewArray())
	b.Release()
	defer f.Release()
	for _, opts := range []*csvio.WriteOptions{nil, {Delimiter: '\t', UseCRLF: true}, {Delimiter: '§'}} {
		var buf bytes.Buffer
		if err := csvio.Write(f, &buf, opts); err != nil {
			t.Fatal(err)
		}
		ro := &csvio.ReadOptions{}
		if opts != nil {
			ro.Delimiter = opts.Delimiter
		}
		back, err := csvio.ReadStrings(&buf, ro)
		if err != nil {
			t.Fatalf("opts %+v: %v", opts, err)
		}
		col, err := back.Column("s")
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := col.AsStrings()
		back.Release()
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range cells {
			if want == "cr\r\nlf" {
				want = "cr\nlf" // the reader turns a quoted CR LF into LF
			}
			if got[i] != want {
				t.Errorf("opts %+v: cell %d = %q, want %q", opts, i, got[i], want)
			}
		}
	}
}

// TestWriter_Streaming — frames written in pieces (one multi-chunk)
// equal one Write of the concatenation; a mismatched schema is
// rejected and sticks; a stream with no rows is header-only.
func TestWriter_Streaming(t *testing.T) {
	part := func(vals ...int64) *gobi.Frame {
		b := array.NewInt64Builder(memory.DefaultAllocator)
		b.AppendValues(vals, nil)
		defer b.Release()
		return framesOf(t, "v", b.NewArray())
	}
	a, b := part(1, 2), part(3)
	cat, err := gobi.Concat(a, b) // two chunks
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := csvio.NewWriter(&buf, a.Schema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*gobi.Frame{a, cat, part()} {
		if err := w.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	if w.NumRows() != 5 {
		t.Errorf("NumRows = %d, want 5", w.NumRows())
	}
	other := func() *gobi.Frame {
		sb := array.NewStringBuilder(memory.DefaultAllocator)
		defer sb.Release()
		sb.Append("x")
		return framesOf(t, "v", sb.NewArray())
	}()
	if err := w.Write(other); err == nil {
		t.Error("schema mismatch: expected error")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "v\n1\n2\n1\n2\n3\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}

	buf.Reset()
	w, err = csvio.NewWriter(&buf, a.Schema(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "v\n" {
		t.Errorf("header-only output %q", buf.String())
	}
	if err := w.Write(a); err == nil {
		t.Error("Write after Close: expected error")
	}
}

// TestWrite_Options — no header, custom time / date layouts, invalid
// delimiters.
func TestWrite_Options(t *testing.T) {
	type row struct {
		At time.Time `csv:"at"`
		N  int64     `csv:"n"`
	}
	rows := []row{{At: time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC), N: 1}}
	noHeader := false
	var buf bytes.Buffer
	err := csvio.WriteStructsWriter(rows, &buf, &csvio.WriteOptions{HasHeader: &noHeader, TimeFormat: "2006/01/02 15h04"})
	if err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "2026/05/06 07h08,1\n" {
		t.Errorf("output %q", got)
	}
	for _, d := range []rune{'"', '\n', '\r', utf8RuneError} {
		err := csvio.WriteStructsWriter(rows, &bytes.Buffer{}, &csvio.WriteOptions{Delimiter: d})
		if !errors.Is(err, csvio.ErrInvalidDelimiter) {
			t.Errorf("delimiter %q: err = %v, want ErrInvalidDelimiter", d, err)
		}
	}
}

const utf8RuneError = '�'

// TestWriteFile_Compression — .gz and .zst names compress and read
// back; a .bz2 name is an error (there's no bzip2 encoder).
func TestWriteFile_Compression(t *testing.T) {
	type row struct {
		ID   int64  `csv:"id"`
		Name string `csv:"name"`
	}
	rows := []row{{1, "a"}, {2, "b"}}
	dir := t.TempDir()
	for _, name := range []string{"x.csv.gz", "x.csv.zst", "x.csv"} {
		path := filepath.Join(dir, name)
		if err := csvio.WriteStructs(rows, path, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if compressed := !strings.HasPrefix(string(raw), "id,name"); compressed != (name != "x.csv") {
			t.Errorf("%s: compressed = %v", name, compressed)
		}
		back, err := csvio.ReadStructs[row](path, nil)
		if err != nil {
			t.Fatalf("%s: read back: %v", name, err)
		}
		if len(back) != 2 || back[1] != rows[1] {
			t.Errorf("%s: read back %+v", name, back)
		}
	}
	err := csvio.WriteStructs(rows, filepath.Join(dir, "x.csv.bz2"), nil)
	if !errors.Is(err, csvio.ErrUnknownCodec) {
		t.Errorf("bz2: err = %v, want ErrUnknownCodec", err)
	}
}

// framesOf wraps one array as a single-column Frame named name.
func framesOf(t *testing.T, name string, a arrow.Array) *gobi.Frame {
	t.Helper()
	defer a.Release()
	fld := arrow.Field{Name: name, Type: a.DataType(), Nullable: true}
	ch := arrow.NewChunked(a.DataType(), []arrow.Array{a})
	defer ch.Release()
	f, err := gobi.NewFrame(arrow.NewSchema([]arrow.Field{fld}, nil), []arrow.Column{*arrow.NewColumn(fld, ch)})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

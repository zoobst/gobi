package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// icebergGolden was produced by iceberg-go v0.6.0's
// BucketTransform{NumBuckets: n}.Transformer(<type>) — the Go reference
// implementation — not computed by hand:
//
//	tr := iceberg.BucketTransform{NumBuckets: n}
//	tr.Transformer(iceberg.PrimitiveTypes.Int64)(int64(v)).Val   // "int64"
//	tr.Transformer(iceberg.PrimitiveTypes.Int32)(int32(v)).Val   // "int32"
//	tr.Transformer(iceberg.PrimitiveTypes.Date)(iceberg.Date(v)) // "date"
//	tr.Transformer(iceberg.PrimitiveTypes.Timestamp)(iceberg.Timestamp(v)) // "timestamp_us"
//	tr.Transformer(iceberg.PrimitiveTypes.String)(s)             // "string"
//	tr.Transformer(iceberg.PrimitiveTypes.Binary)(b)             // "binary"
var icebergGolden = []struct {
	n    int
	kind string
	v    any
	want int32
}{
	{1, "int64", int64(0), 0},
	{1, "int32", int32(0), 0},
	{1, "date", int32(0), 0},
	{1, "timestamp_us", int64(0), 0},
	{1, "int64", int64(1), 0},
	{1, "int32", int32(1), 0},
	{1, "date", int32(1), 0},
	{1, "timestamp_us", int64(1), 0},
	{1, "int64", int64(-1), 0},
	{1, "int32", int32(-1), 0},
	{1, "date", int32(-1), 0},
	{1, "timestamp_us", int64(-1), 0},
	{1, "int64", int64(34), 0},
	{1, "int32", int32(34), 0},
	{1, "date", int32(34), 0},
	{1, "timestamp_us", int64(34), 0},
	{1, "int64", int64(2147483647), 0},
	{1, "int32", int32(2147483647), 0},
	{1, "date", int32(2147483647), 0},
	{1, "timestamp_us", int64(2147483647), 0},
	{1, "int64", int64(-2147483648), 0},
	{1, "int32", int32(-2147483648), 0},
	{1, "date", int32(-2147483648), 0},
	{1, "timestamp_us", int64(-2147483648), 0},
	{1, "int64", int64(1099511627776), 0},
	{1, "timestamp_us", int64(1099511627776), 0},
	{1, "int64", int64(-9223372036854775808), 0},
	{1, "timestamp_us", int64(-9223372036854775808), 0},
	{1, "int64", int64(9223372036854775807), 0},
	{1, "timestamp_us", int64(9223372036854775807), 0},
	{1, "string", "", 0},
	{1, "string", "iceberg", 0},
	{1, "string", "a", 0},
	{1, "string", "user-00042", 0},
	{1, "string", "日本語", 0},
	{1, "string", "\x00\xff", 0},
	{1, "binary", []byte(""), 0},
	{1, "binary", []byte("\x00\x01\x02\x03"), 0},
	{1, "binary", []byte("\xff"), 0},
	{16, "int64", int64(0), 12},
	{16, "int32", int32(0), 12},
	{16, "date", int32(0), 12},
	{16, "timestamp_us", int64(0), 12},
	{16, "int64", int64(1), 4},
	{16, "int32", int32(1), 4},
	{16, "date", int32(1), 4},
	{16, "timestamp_us", int64(1), 4},
	{16, "int64", int64(-1), 8},
	{16, "int32", int32(-1), 8},
	{16, "date", int32(-1), 8},
	{16, "timestamp_us", int64(-1), 8},
	{16, "int64", int64(34), 3},
	{16, "int32", int32(34), 3},
	{16, "date", int32(34), 3},
	{16, "timestamp_us", int64(34), 3},
	{16, "int64", int64(2147483647), 14},
	{16, "int32", int32(2147483647), 14},
	{16, "date", int32(2147483647), 14},
	{16, "timestamp_us", int64(2147483647), 14},
	{16, "int64", int64(-2147483648), 8},
	{16, "int32", int32(-2147483648), 8},
	{16, "date", int32(-2147483648), 8},
	{16, "timestamp_us", int64(-2147483648), 8},
	{16, "int64", int64(1099511627776), 2},
	{16, "timestamp_us", int64(1099511627776), 2},
	{16, "int64", int64(-9223372036854775808), 5},
	{16, "timestamp_us", int64(-9223372036854775808), 5},
	{16, "int64", int64(9223372036854775807), 15},
	{16, "timestamp_us", int64(9223372036854775807), 15},
	{16, "string", "", 0},
	{16, "string", "iceberg", 9},
	{16, "string", "a", 2},
	{16, "string", "user-00042", 14},
	{16, "string", "日本語", 7},
	{16, "string", "\x00\xff", 4},
	{16, "binary", []byte(""), 0},
	{16, "binary", []byte("\x00\x01\x02\x03"), 9},
	{16, "binary", []byte("\xff"), 13},
	{64, "int64", int64(0), 60},
	{64, "int32", int32(0), 60},
	{64, "date", int32(0), 60},
	{64, "timestamp_us", int64(0), 60},
	{64, "int64", int64(1), 4},
	{64, "int32", int32(1), 4},
	{64, "date", int32(1), 4},
	{64, "timestamp_us", int64(1), 4},
	{64, "int64", int64(-1), 40},
	{64, "int32", int32(-1), 40},
	{64, "date", int32(-1), 40},
	{64, "timestamp_us", int64(-1), 40},
	{64, "int64", int64(34), 19},
	{64, "int32", int32(34), 19},
	{64, "date", int32(34), 19},
	{64, "timestamp_us", int64(34), 19},
	{64, "int64", int64(2147483647), 62},
	{64, "int32", int32(2147483647), 62},
	{64, "date", int32(2147483647), 62},
	{64, "timestamp_us", int64(2147483647), 62},
	{64, "int64", int64(-2147483648), 24},
	{64, "int32", int32(-2147483648), 24},
	{64, "date", int32(-2147483648), 24},
	{64, "timestamp_us", int64(-2147483648), 24},
	{64, "int64", int64(1099511627776), 34},
	{64, "timestamp_us", int64(1099511627776), 34},
	{64, "int64", int64(-9223372036854775808), 37},
	{64, "timestamp_us", int64(-9223372036854775808), 37},
	{64, "int64", int64(9223372036854775807), 63},
	{64, "timestamp_us", int64(9223372036854775807), 63},
	{64, "string", "", 0},
	{64, "string", "iceberg", 25},
	{64, "string", "a", 50},
	{64, "string", "user-00042", 46},
	{64, "string", "日本語", 23},
	{64, "string", "\x00\xff", 20},
	{64, "binary", []byte(""), 0},
	{64, "binary", []byte("\x00\x01\x02\x03"), 57},
	{64, "binary", []byte("\xff"), 13},
	{1000, "int64", int64(0), 676},
	{1000, "int32", int32(0), 676},
	{1000, "date", int32(0), 676},
	{1000, "timestamp_us", int64(0), 676},
	{1000, "int64", int64(1), 556},
	{1000, "int32", int32(1), 556},
	{1000, "date", int32(1), 556},
	{1000, "timestamp_us", int64(1), 556},
	{1000, "int64", int64(-1), 712},
	{1000, "int32", int32(-1), 712},
	{1000, "date", int32(-1), 712},
	{1000, "timestamp_us", int64(-1), 712},
	{1000, "int64", int64(34), 379},
	{1000, "int32", int32(34), 379},
	{1000, "date", int32(34), 379},
	{1000, "timestamp_us", int64(34), 379},
	{1000, "int64", int64(2147483647), 606},
	{1000, "int32", int32(2147483647), 606},
	{1000, "date", int32(2147483647), 606},
	{1000, "timestamp_us", int64(2147483647), 606},
	{1000, "int64", int64(-2147483648), 856},
	{1000, "int32", int32(-2147483648), 856},
	{1000, "date", int32(-2147483648), 856},
	{1000, "timestamp_us", int64(-2147483648), 856},
	{1000, "int64", int64(1099511627776), 778},
	{1000, "timestamp_us", int64(1099511627776), 778},
	{1000, "int64", int64(-9223372036854775808), 829},
	{1000, "timestamp_us", int64(-9223372036854775808), 829},
	{1000, "int64", int64(9223372036854775807), 599},
	{1000, "timestamp_us", int64(9223372036854775807), 599},
	{1000, "string", "", 0},
	{1000, "string", "iceberg", 89},
	{1000, "string", "a", 850},
	{1000, "string", "user-00042", 294},
	{1000, "string", "日本語", 231},
	{1000, "string", "\x00\xff", 780},
	{1000, "binary", []byte(""), 0},
	{1000, "binary", []byte("\x00\x01\x02\x03"), 441},
	{1000, "binary", []byte("\xff"), 597},
}

// oneValueSeries builds a single-row Series of the arrow type for kind.
func oneValueSeries(t *testing.T, kind string, v any) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	var arr arrow.Array
	switch kind {
	case "int64":
		b := array.NewInt64Builder(pool)
		b.Append(v.(int64))
		arr = b.NewArray()
		b.Release()
	case "int32":
		b := array.NewInt32Builder(pool)
		b.Append(v.(int32))
		arr = b.NewArray()
		b.Release()
	case "date":
		b := array.NewDate32Builder(pool)
		b.Append(arrow.Date32(v.(int32)))
		arr = b.NewArray()
		b.Release()
	case "timestamp_us":
		b := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"})
		b.Append(arrow.Timestamp(v.(int64)))
		arr = b.NewArray()
		b.Release()
	case "string":
		b := array.NewStringBuilder(pool)
		b.Append(v.(string))
		arr = b.NewArray()
		b.Release()
	case "binary":
		b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
		b.Append(v.([]byte))
		arr = b.NewArray()
		b.Release()
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	s, err := arrayToSeries(pool, "v", arr.DataType(), arr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.col.Release() })
	return s
}

// TestIcebergBucket_MatchesIcebergGo — every golden row, computed by
// iceberg-go, matches gobi's transform.
func TestIcebergBucket_MatchesIcebergGo(t *testing.T) {
	for _, g := range icebergGolden {
		s := oneValueSeries(t, g.kind, g.v)
		out, err := s.IcebergBucket(g.n)
		if err != nil {
			t.Fatalf("%s %v bucket(%d): %v", g.kind, g.v, g.n, err)
		}
		got := out.col.Data().Chunk(0).(*array.Int32).Value(0)
		if got != g.want {
			t.Errorf("%s %#v bucket(%d) = %d, iceberg-go says %d", g.kind, g.v, g.n, got, g.want)
		}
		out.col.Release()
	}
}

// TestMurmur3_SpecVectors — raw hashes from the Iceberg spec's hash
// appendix (int / long 34, "iceberg", 00 01 02 03), plus tail-length
// cases, all cross-checked with github.com/twmb/murmur3.Sum32.
func TestMurmur3_SpecVectors(t *testing.T) {
	le34 := []byte{34, 0, 0, 0, 0, 0, 0, 0}
	for _, c := range []struct {
		in   []byte
		want int32
	}{
		{le34, 2017239379},
		{[]byte("iceberg"), 1210000089},
		{[]byte{0, 1, 2, 3}, -188683207},
		{nil, 0},
		{[]byte("a"), 1009084850},
		{[]byte("ab"), -1681926305},
		{[]byte("abc"), -1277324294},
		{[]byte("hello"), 613153351},
		{[]byte("The quick brown fox jumps over the lazy dog"), 776992547},
	} {
		if got := int32(murmur3Sum32(c.in)); got != c.want {
			t.Errorf("murmur3(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestIcebergBucket_ExprAndEdges — the Expr form, nulls, unit
// conversion, and rejected inputs.
func TestIcebergBucket_ExprAndEdges(t *testing.T) {
	type row struct {
		ID  *string
		Big int64
	}
	f, err := FromStructs([]row{{ptr("iceberg"), 34}, {nil, -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	out, err := f.WithColumnExpr("b", Col("ID").IcebergBucket(16))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := out.Column("b")
	arr := b.Column().Data().Chunk(0).(*array.Int32)
	if b.DataType().ID() != arrow.INT32 || arr.Value(0) != int32((1210000089&math.MaxInt32)%16) || !arr.IsNull(1) {
		t.Errorf("Expr bucket = %v (null row 1: %v)", arr, arr.IsNull(1))
	}
	if typ, err := Col("ID").IcebergBucket(16).Node().Type(f.Schema()); err != nil || typ.ID() != arrow.INT32 {
		t.Errorf("Type() = %v, %v", typ, err)
	}

	// Millisecond timestamps convert exactly to microseconds first.
	pool := memory.DefaultAllocator
	tb := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Millisecond})
	tb.Append(1_700_000_000_000)
	ms, _ := arrayToSeries(pool, "t", tb.Type(), tb.NewArray())
	tb.Release()
	defer ms.col.Release()
	us := oneValueSeries(t, "timestamp_us", int64(1_700_000_000_000_000))
	a, err := ms.IcebergBucket(64)
	if err != nil {
		t.Fatal(err)
	}
	defer a.col.Release()
	c, _ := us.IcebergBucket(64)
	defer c.col.Release()
	if a.col.Data().Chunk(0).(*array.Int32).Value(0) != c.col.Data().Chunk(0).(*array.Int32).Value(0) {
		t.Error("ms timestamp bucket != equivalent µs timestamp bucket")
	}

	// Rejected: bucket count, unsigned, float, ns timestamps.
	type typed struct {
		U uint32
		F float64
	}
	g, err := FromStructs([]typed{{1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release()
	for _, col := range []string{"U", "F"} {
		s, _ := g.Column(col)
		if _, err := s.IcebergBucket(8); !errors.Is(err, ErrExprTypeMismatch) {
			t.Errorf("%s: err = %v, want ErrExprTypeMismatch", col, err)
		}
	}
	idCol, _ := f.Column("ID")
	for _, n := range []int{0, -1} {
		if _, err := idCol.IcebergBucket(n); err == nil {
			t.Errorf("bucket(%d): want error", n)
		}
	}
	nb := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Nanosecond})
	nb.Append(1)
	nsS, _ := arrayToSeries(pool, "ns", nb.Type(), nb.NewArray())
	nb.Release()
	defer nsS.col.Release()
	if _, err := nsS.IcebergBucket(8); !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("ns timestamp: err = %v", err)
	}
}

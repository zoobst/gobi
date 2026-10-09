package gobi

import (
	"math"
	"slices"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/decimal256"
	"github.com/apache/arrow-go/v18/arrow/float16"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// keyFrame builds a frame of a "name" column (a, b, c, d) and one key
// column holding key's values for those rows, in one chunk per element
// of chunks (each a slice of the 4 rows).
func keyFrame(t *testing.T, key arrow.Array, chunks ...[2]int) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nb := array.NewStringBuilder(pool)
	defer nb.Release()
	nb.AppendValues([]string{"a", "b", "c", "d"}, nil)
	names := nb.NewArray()
	defer names.Release()
	if len(chunks) == 0 {
		chunks = [][2]int{{0, 4}}
	}
	var keyChunks []arrow.Array
	for _, c := range chunks {
		keyChunks = append(keyChunks, array.NewSlice(key, int64(c[0]), int64(c[1])))
	}
	defer func() {
		for _, c := range keyChunks {
			c.Release()
		}
	}()
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String},
		{Name: "k", Type: key.DataType(), Nullable: true},
	}
	nc := arrow.NewChunked(names.DataType(), []arrow.Array{names})
	kc := arrow.NewChunked(key.DataType(), keyChunks)
	defer nc.Release()
	defer kc.Release()
	f, err := NewFrame(arrow.NewSchema(fields, nil), []arrow.Column{
		*arrow.NewColumn(fields[0], nc), *arrow.NewColumn(fields[1], kc),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestSortBy_KeyTypes — every key type ranks rows b < a < d with c's
// null last, in both directions.
func TestSortBy_KeyTypes(t *testing.T) {
	pool := memory.DefaultAllocator
	valid := []bool{true, true, false, true}
	build := func(b array.Builder, appendRows func()) arrow.Array {
		defer b.Release()
		appendRows()
		return b.NewArray()
	}
	cases := map[string]func() arrow.Array{
		"large_string": func() arrow.Array {
			b := array.NewLargeStringBuilder(pool)
			return build(b, func() { b.AppendValues([]string{"m", "a", "", "z"}, valid) })
		},
		"string_view": func() arrow.Array {
			b := array.NewStringViewBuilder(pool)
			return build(b, func() { b.AppendValues([]string{"m", "a", "", "z"}, valid) })
		},
		"binary": func() arrow.Array {
			b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
			return build(b, func() { b.AppendValues([][]byte{{5}, {1, 9}, nil, {9}}, valid) })
		},
		"large_binary": func() arrow.Array {
			b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.LargeBinary)
			return build(b, func() { b.AppendValues([][]byte{{5}, {1, 9}, nil, {9}}, valid) })
		},
		"fixed_size_binary": func() arrow.Array {
			b := array.NewFixedSizeBinaryBuilder(pool, &arrow.FixedSizeBinaryType{ByteWidth: 2})
			return build(b, func() { b.AppendValues([][]byte{{5, 0}, {1, 9}, {0, 0}, {9, 0}}, valid) })
		},
		"date32": func() arrow.Array {
			b := array.NewDate32Builder(pool)
			return build(b, func() { b.AppendValues([]arrow.Date32{10, -5, 0, 20}, valid) })
		},
		"date64": func() arrow.Array {
			b := array.NewDate64Builder(pool)
			return build(b, func() { b.AppendValues([]arrow.Date64{10, -5, 0, 20}, valid) })
		},
		"time32": func() arrow.Array {
			b := array.NewTime32Builder(pool, &arrow.Time32Type{Unit: arrow.Millisecond})
			return build(b, func() { b.AppendValues([]arrow.Time32{10, 1, 0, 20}, valid) })
		},
		"time64": func() arrow.Array {
			b := array.NewTime64Builder(pool, &arrow.Time64Type{Unit: arrow.Microsecond})
			return build(b, func() { b.AppendValues([]arrow.Time64{10, 1, 0, 20}, valid) })
		},
		"duration": func() arrow.Array {
			b := array.NewDurationBuilder(pool, &arrow.DurationType{Unit: arrow.Second})
			return build(b, func() { b.AppendValues([]arrow.Duration{10, -1, 0, 20}, valid) })
		},
		"float16": func() arrow.Array {
			b := array.NewFloat16Builder(pool)
			return build(b, func() {
				b.AppendValues([]float16.Num{float16.New(1), float16.New(-2), float16.New(0), float16.New(3)}, valid)
			})
		},
		"decimal128": func() arrow.Array {
			b := array.NewDecimal128Builder(pool, &arrow.Decimal128Type{Precision: 10, Scale: 2})
			return build(b, func() {
				b.AppendValues([]decimal128.Num{decimal128.FromI64(10), decimal128.FromI64(-10), {}, decimal128.FromI64(99)}, valid)
			})
		},
		"decimal256": func() arrow.Array {
			b := array.NewDecimal256Builder(pool, &arrow.Decimal256Type{Precision: 40, Scale: 2})
			return build(b, func() {
				b.AppendValues([]decimal256.Num{decimal256.FromI64(10), decimal256.FromI64(-10), {}, decimal256.FromI64(99)}, valid)
			})
		},
		// Index order (d < b < a) differs from value order (b < a < d):
		// the sort must follow the values.
		"dictionary": func() arrow.Array {
			vb := array.NewStringBuilder(pool)
			dict := build(vb, func() { vb.AppendValues([]string{"zeta", "alpha", "mid"}, nil) })
			defer dict.Release()
			ib := array.NewInt32Builder(pool)
			idx := build(ib, func() { ib.AppendValues([]int32{2, 1, 0, 0}, valid) })
			defer idx.Release()
			dt := &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}
			return array.NewDictionaryArray(dt, idx, dict)
		},
	}
	for name, mk := range cases {
		key := mk()
		f := keyFrame(t, key)
		key.Release()
		for desc, want := range map[bool][]string{false: {"b", "a", "d", "c"}, true: {"d", "a", "b", "c"}} {
			out, err := f.SortBy(SortKey{Column: "k", Descending: desc})
			if err != nil {
				t.Errorf("%s desc=%v: %v", name, desc, err)
				continue
			}
			if got := sortedNames(t, out); !slices.Equal(got, want) {
				t.Errorf("%s desc=%v: got %v, want %v", name, desc, got, want)
			}
		}
	}
}

// TestSortBy_DictionaryNullEntry — a valid index pointing at a null
// dictionary entry sorts like a null.
func TestSortBy_DictionaryNullEntry(t *testing.T) {
	pool := memory.DefaultAllocator
	vb := array.NewStringBuilder(pool)
	defer vb.Release()
	vb.AppendValues([]string{"x", "", "y"}, []bool{true, false, true})
	dict := vb.NewArray()
	defer dict.Release()
	ib := array.NewInt32Builder(pool)
	defer ib.Release()
	ib.AppendValues([]int32{2, 0, 1, 2}, nil) // c → null entry
	idx := ib.NewArray()
	defer idx.Release()
	dt := &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}
	key := array.NewDictionaryArray(dt, idx, dict)
	f := keyFrame(t, key)
	key.Release()
	out, err := f.SortBy(SortKey{Column: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(t, out), []string{"b", "a", "d", "c"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSortBy_MultiChunkKey — a key column split across chunks sorts
// without CompactChunks.
func TestSortBy_MultiChunkKey(t *testing.T) {
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues([]int64{2, 1, 0, 3}, []bool{true, true, false, true})
	key := b.NewArray()
	f := keyFrame(t, key, [2]int{0, 1}, [2]int{1, 3}, [2]int{3, 4})
	key.Release()
	out, err := f.SortBy(SortKey{Column: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(t, out), []string{"b", "a", "d", "c"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSortBy_NaNLastDescending — NaN sorts after every number when
// descending too, ahead of nulls only.
func TestSortBy_NaNLastDescending(t *testing.T) {
	b := array.NewFloat64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues([]float64{1.5, math.NaN(), 0, 0.5}, []bool{true, true, false, true})
	key := b.NewArray()
	f := keyFrame(t, key)
	key.Release()
	for desc, want := range map[bool][]string{false: {"d", "a", "b", "c"}, true: {"a", "d", "b", "c"}} {
		out, err := f.SortBy(SortKey{Column: "k", Descending: desc})
		if err != nil {
			t.Fatal(err)
		}
		if got := sortedNames(t, out); !slices.Equal(got, want) {
			t.Errorf("desc=%v: got %v, want %v", desc, got, want)
		}
	}
}

// TestSortBy_NullKeyColumn — an all-null key keeps input order.
func TestSortBy_NullKeyColumn(t *testing.T) {
	key := array.NewNull(4)
	f := keyFrame(t, key)
	key.Release()
	out, err := f.SortBy(SortKey{Column: "k", Descending: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(t, out), []string{"a", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTake_ViewTypes — StringView / BinaryView columns reorder, single-
// and multi-chunk (compute.Take has no view kernel).
func TestTake_ViewTypes(t *testing.T) {
	pool := memory.DefaultAllocator
	sb := array.NewStringViewBuilder(pool)
	defer sb.Release()
	sb.AppendValues([]string{"a-long-value-past-the-inline-limit", "b", "", "d"}, []bool{true, true, false, true})
	sv := sb.NewArray()
	defer sv.Release()
	bb := array.NewBinaryViewBuilder(pool)
	defer bb.Release()
	bb.AppendValues([][]byte{{1}, {2}, nil, {4}}, []bool{true, true, false, true})
	bv := bb.NewArray()
	defer bv.Release()
	for _, key := range []arrow.Array{sv, bv} {
		for _, chunks := range [][][2]int{{{0, 4}}, {{0, 2}, {2, 4}}} {
			f := keyFrame(t, key, chunks...)
			out, err := f.Take([]int{3, 2, 0})
			if err != nil {
				t.Fatalf("%s %d chunks: %v", key.DataType(), len(chunks), err)
			}
			if got, want := sortedNames(t, out), []string{"d", "c", "a"}; !slices.Equal(got, want) {
				t.Errorf("%s: names %v, want %v", key.DataType(), got, want)
			}
			k, _ := out.Column("k")
			arr := k.Column().Data().Chunks()[0]
			if !arr.IsNull(1) || arr.IsNull(0) || arr.ValueStr(2) != key.ValueStr(0) {
				t.Errorf("%s: taken values wrong: %v", key.DataType(), arr)
			}
		}
	}
}

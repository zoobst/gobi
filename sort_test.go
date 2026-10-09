package gobi

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/decimal256"
	"github.com/apache/arrow-go/v18/arrow/float16"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi/geometry"
)

// sortFrame builds a small frame with mixed key types for SortBy tests:
//
//	name   score (f64)  qty (i64)  active (bool)   region (str)
//	Alpha    3.5           5         true            "US"
//	Bravo    1.0           2         false           "EU"
//	Charlie  3.5           3         true            "US"
//	Delta    2.0           7         false           "EU"
func sortFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"Alpha", "Bravo", "Charlie", "Delta"}, nil)

	scoreB := array.NewFloat64Builder(pool)
	defer scoreB.Release()
	scoreB.AppendValues([]float64{3.5, 1.0, 3.5, 2.0}, nil)

	qtyB := array.NewInt64Builder(pool)
	defer qtyB.Release()
	qtyB.AppendValues([]int64{5, 2, 3, 7}, nil)

	activeB := array.NewBooleanBuilder(pool)
	defer activeB.Release()
	activeB.AppendValues([]bool{true, false, true, false}, nil)

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"US", "EU", "US", "EU"}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "score", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "qty", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "active", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{
		nameB.NewArray(), scoreB.NewArray(), qtyB.NewArray(),
		activeB.NewArray(), regionB.NewArray(),
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// sortedNames reads the "name" column of df and returns its values in
// row order. Used to assert sort orderings without repeating chunk-walk
// boilerplate in every test.
func sortedNames(t *testing.T, df *Frame) []string {
	t.Helper()
	col, err := df.Column("name")
	if err != nil {
		t.Fatal(err)
	}
	arr := col.Column().Data().Chunks()[0].(*array.String)
	out := make([]string, arr.Len())
	for i := range arr.Len() {
		out[i] = arr.Value(i)
	}
	return out
}

func TestSortBy_SingleKeyAsc(t *testing.T) {
	df := sortFrame(t)
	out, err := df.SortBy(SortKey{Column: "qty"})
	if err != nil {
		t.Fatal(err)
	}
	// qty ascending: 2 (Bravo), 3 (Charlie), 5 (Alpha), 7 (Delta)
	got := sortedNames(t, out)
	want := []string{"Bravo", "Charlie", "Alpha", "Delta"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_SingleKeyDesc(t *testing.T) {
	df := sortFrame(t)
	out, err := df.SortBy(SortKey{Column: "qty", Descending: true})
	if err != nil {
		t.Fatal(err)
	}
	// qty descending: 7 (Delta), 5 (Alpha), 3 (Charlie), 2 (Bravo)
	got := sortedNames(t, out)
	want := []string{"Delta", "Alpha", "Charlie", "Bravo"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_MultiKeyTiebreaker(t *testing.T) {
	df := sortFrame(t)
	// score asc, then name asc → 1.0 (Bravo), 2.0 (Delta),
	//                            then 3.5 tie broken by name: Alpha, Charlie
	out, err := df.SortBy(
		SortKey{Column: "score"},
		SortKey{Column: "name"},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	want := []string{"Bravo", "Delta", "Alpha", "Charlie"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_MultiKeyMixedDirection(t *testing.T) {
	df := sortFrame(t)
	// region asc (EU before US), then score desc within region.
	// EU: Bravo (1.0), Delta (2.0) → sorted desc: Delta, Bravo.
	// US: Alpha (3.5), Charlie (3.5) → tie on score desc → stable
	//     retains input order: Alpha, Charlie.
	out, err := df.SortBy(
		SortKey{Column: "region"},
		SortKey{Column: "score", Descending: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	want := []string{"Delta", "Bravo", "Alpha", "Charlie"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_Stable(t *testing.T) {
	// A single key where two rows tie: score=3.5 for Alpha and Charlie.
	// Stable sort preserves their relative input order regardless of
	// direction (their tie doesn't swap).
	df := sortFrame(t)
	for _, desc := range []bool{false, true} {
		out, err := df.SortBy(SortKey{Column: "active", Descending: desc})
		if err != nil {
			t.Fatal(err)
		}
		got := sortedNames(t, out)
		// active=true rows: Alpha, Charlie (input order).
		// active=false rows: Bravo, Delta (input order).
		var trueFirst, falseFirst []string
		if desc {
			trueFirst = []string{"Alpha", "Charlie"}
			falseFirst = []string{"Bravo", "Delta"}
			if got[0] != trueFirst[0] || got[1] != trueFirst[1] ||
				got[2] != falseFirst[0] || got[3] != falseFirst[1] {
				t.Errorf("desc stable: got %v", got)
			}
		} else {
			trueFirst = []string{"Bravo", "Delta"}
			falseFirst = []string{"Alpha", "Charlie"}
			if got[0] != trueFirst[0] || got[1] != trueFirst[1] ||
				got[2] != falseFirst[0] || got[3] != falseFirst[1] {
				t.Errorf("asc stable: got %v", got)
			}
		}
	}
}

func TestSortBy_NullsSortLast(t *testing.T) {
	// Build a frame where qty has a null in the middle. On both
	// ascending and descending sorts, the null must sink to the end.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B", "C", "D"}, nil)
	qtyB := array.NewInt64Builder(pool)
	defer qtyB.Release()
	qtyB.AppendValues([]int64{5, 0, 2, 8}, []bool{true, false, true, true}) // B null

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "qty", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), qtyB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	for _, desc := range []bool{false, true} {
		out, err := df.SortBy(SortKey{Column: "qty", Descending: desc})
		if err != nil {
			t.Fatal(err)
		}
		got := sortedNames(t, out)
		if got[len(got)-1] != "B" {
			t.Errorf("desc=%v: null row should be last, got %v", desc, got)
		}
	}
}

func TestSortBy_NaNSortsLikeNullLast(t *testing.T) {
	// Float64 with a NaN — NaN sorts to the end regardless of direction.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B", "C"}, nil)
	scoreB := array.NewFloat64Builder(pool)
	defer scoreB.Release()
	scoreB.AppendValues([]float64{1.5, math.NaN(), 0.5}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "score", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), scoreB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, _ := NewFrame(schema, cols)

	out, err := df.SortBy(SortKey{Column: "score"})
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	// asc: 0.5 (C), 1.5 (A), NaN (B)
	if got[2] != "B" {
		t.Fatalf("NaN row should be last, got %v", got)
	}
}

func TestSortBy_TimestampKey(t *testing.T) {
	pool := memory.DefaultAllocator
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond}
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B", "C"}, nil)
	tsB := array.NewTimestampBuilder(pool, tsType)
	defer tsB.Release()
	tsB.Append(arrow.Timestamp(3_000_000))
	tsB.Append(arrow.Timestamp(1_000_000))
	tsB.Append(arrow.Timestamp(2_000_000))

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "when", Type: tsType, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), tsB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, _ := NewFrame(schema, cols)

	out, err := df.SortBy(SortKey{Column: "when"})
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	// ascending timestamps: 1M (B), 2M (C), 3M (A)
	want := []string{"B", "C", "A"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_NoKeysErrors(t *testing.T) {
	df := sortFrame(t)
	if _, err := df.SortBy(); err == nil {
		t.Fatal("expected error for zero keys")
	}
}

func TestSortBy_MissingColumnErrors(t *testing.T) {
	df := sortFrame(t)
	_, err := df.SortBy(SortKey{Column: "nope"})
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

// smallIntSortRow carries one key column per small integer width. The
// pointer fields give each column a null in row "c".
type smallIntSortRow struct {
	Name string
	I8   *int8
	I16  *int16
	U8   *uint8
	U16  *uint16
}

// TestSortBy_SmallIntKeys — Int8 / Int16 / Uint8 / Uint16 work as sort
// keys in both directions, with nulls last, through Frame.SortBy and
// LazyFrame.SortBy.
func TestSortBy_SmallIntKeys(t *testing.T) {
	i8 := func(v int8) *int8 { return &v }
	i16 := func(v int16) *int16 { return &v }
	u8 := func(v uint8) *uint8 { return &v }
	u16 := func(v uint16) *uint16 { return &v }
	// Values chosen so each column ranks the rows b < a < d; c is null.
	// Negative Int8/Int16 values check signed comparison.
	rows := []smallIntSortRow{
		{Name: "a", I8: i8(-1), I16: i16(-100), U8: u8(20), U16: u16(2000)},
		{Name: "b", I8: i8(-128), I16: i16(-32768), U8: u8(0), U16: u16(0)},
		{Name: "c"},
		{Name: "d", I8: i8(127), I16: i16(32767), U8: u8(255), U16: u16(65535)},
	}
	df, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	names := func(f *Frame) []string {
		t.Helper()
		got, err := ToStructs[smallIntSortRow](f)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(got))
		for i, r := range got {
			out[i] = r.Name
		}
		return out
	}
	want := map[bool][]string{false: {"b", "a", "d", "c"}, true: {"d", "a", "b", "c"}}
	for _, col := range []string{"I8", "I16", "U8", "U16"} {
		for _, desc := range []bool{false, true} {
			out, err := df.SortBy(SortKey{Column: col, Descending: desc})
			if err != nil {
				t.Fatalf("%s desc=%v: %v", col, desc, err)
			}
			if got := names(out); !slices.Equal(got, want[desc]) {
				t.Errorf("%s desc=%v: got %v, want %v", col, desc, got, want[desc])
			}
		}
		lazy, err := df.Lazy().SortBy(SortKey{Column: col}).Collect()
		if err != nil {
			t.Fatalf("%s lazy: %v", col, err)
		}
		if got := names(lazy); !slices.Equal(got, want[false]) {
			t.Errorf("%s lazy: got %v, want %v", col, got, want[false])
		}
	}
}

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

// TestFrame_SortByHilbert_TightRowGroupBboxes: an unsorted grid of
// polygons ends up with a wide row-group bbox if you naively chunk
// it in insertion order; sorting by Hilbert first collapses each
// row-group bbox to a spatially-tight cluster. This is the whole
// point of spatial sorting, so we verify it directly.
//
// Corpus: 100 polygons on a 10×10 grid at (i, j). Row-group size
// 10. Without sorting, each row-group has bbox spanning many cells
// (because insertion order zig-zags). With Hilbert sort, each
// row-group's bbox stays local.
func TestFrame_SortByHilbert_TightRowGroupBboxes(t *testing.T) {
	pool := memory.DefaultAllocator
	const gridSize = 10
	n := gridSize * gridSize

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	// Insertion order: bottom-to-top by column, then across columns.
	// Deliberately non-spatial so a naive chunking of 10 rows gives
	// wide bboxes.
	for i := range n {
		col := i / gridSize
		row := i % gridSize
		// Offset a bit so no two polygons share coords (breaks ties
		// deterministically for the stable sort).
		x := float64(col*10) + 0.1
		y := float64(row*10) + 0.1
		poly := geometry.SimplePolygon([]geometry.Point{
			{X: x, Y: y},
			{X: x + 1, Y: y},
			{X: x + 1, Y: y + 1},
			{X: x, Y: y + 1},
			{X: x, Y: y},
		}, geometry.PseudoMercator)
		geomB.Append(geometry.WKB(poly))
	}

	field := GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert: %v", err)
	}
	defer sorted.Release()

	// For each chunk of 10 rows in the sorted output, compute the
	// bbox diagonal. Compare to the unsorted diagonal — the sorted
	// version should be substantially smaller.
	unsortedDiag := chunkedDiagonal(t, f, 10)
	sortedDiag := chunkedDiagonal(t, sorted, 10)

	// Not a tight bound — depends on grid layout — but sorted
	// should be at least half the unsorted diagonal.
	if sortedDiag*2 > unsortedDiag {
		t.Errorf("Hilbert sort didn't tighten row-group bboxes enough: sorted=%.2f unsorted=%.2f (expected sorted*2 < unsorted)",
			sortedDiag, unsortedDiag)
	}
}

// chunkedDiagonal returns the average bbox-diagonal length over
// fixed-size row-group chunks. Larger → looser spatial locality.
func chunkedDiagonal(t *testing.T, f *Frame, groupSize int) float64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	n := f.NumRows()
	var total float64
	var groups int
	for start := 0; start < n; start += groupSize {
		end := min(start+groupSize, n)
		b := geometry.EmptyBounds()
		idx := 0
		for _, chunk := range col.Column().Data().Chunks() {
			bin := chunk.(*array.Binary)
			for i := range bin.Len() {
				if idx >= end {
					break
				}
				if idx >= start && !bin.IsNull(i) {
					g, err := geometry.ParseWKB(bin.Value(i))
					if err != nil {
						t.Fatal(err)
					}
					gb := g.Bounds()
					b = b.Union(gb)
				}
				idx++
			}
		}
		if !b.Empty() {
			dx := b.MaxX - b.MinX
			dy := b.MaxY - b.MinY
			total += dx*dx + dy*dy // squared diagonal
			groups++
		}
	}
	if groups == 0 {
		return 0
	}
	return total / float64(groups)
}

// TestFrame_SortByHilbertWith_SharedReferenceFrame: two partitions
// of the same overall dataset, sorted independently with the SAME
// caller-supplied bounds, should produce Hilbert indices that live
// on the same 1D curve. Concretely, if we split a corpus into
// "left half" and "right half" and sort each with the FULL corpus's
// bbox, the two outputs concatenated form a spatially-coherent
// sequence — this is the primitive that multi-file / multi-partition
// pipelines need for cross-file locality.
func TestFrame_SortByHilbertWith_SharedReferenceFrame(t *testing.T) {
	// Build two frames: leftHalf covers x in [0, 500], rightHalf
	// covers x in [500, 1000]. Both share y in [0, 1000].
	sharedBounds := geometry.Bounds{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 1000}
	left := gridFrame(t, 0, 500, 0, 500, 25)
	defer left.Release()
	right := gridFrame(t, 500, 1000, 0, 500, 25)
	defer right.Release()

	// Sort each with the SHARED reference frame.
	leftSorted, err := left.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds})
	if err != nil {
		t.Fatal(err)
	}
	defer leftSorted.Release()
	rightSorted, err := right.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds})
	if err != nil {
		t.Fatal(err)
	}
	defer rightSorted.Release()

	// Each partition's first row's centroid should sit at a smaller
	// Hilbert index (in the shared frame) than its last row's — the
	// sort worked. We check by re-computing HilbertIndex on the
	// centroids of the first and last row of each partition and
	// verifying the ordering holds.
	firstIdx := hilbertOfRow(t, leftSorted, 0, sharedBounds)
	lastIdx := hilbertOfRow(t, leftSorted, leftSorted.NumRows()-1, sharedBounds)
	if firstIdx > lastIdx {
		t.Errorf("left partition not sorted ascending: first=%d last=%d", firstIdx, lastIdx)
	}
	firstIdx = hilbertOfRow(t, rightSorted, 0, sharedBounds)
	lastIdx = hilbertOfRow(t, rightSorted, rightSorted.NumRows()-1, sharedBounds)
	if firstIdx > lastIdx {
		t.Errorf("right partition not sorted ascending: first=%d last=%d", firstIdx, lastIdx)
	}
}

// gridFrame builds a Frame of size×size polygons on an axis-aligned
// grid over [xMin..xMax] × [yMin..yMax]. Rows in insertion order
// (deliberately non-spatial to exercise the sort).
func gridFrame(t *testing.T, xMin, xMax, yMin, yMax float64, size int) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	dx := (xMax - xMin) / float64(size)
	dy := (yMax - yMin) / float64(size)
	for i := range size {
		for j := range size {
			x := xMin + float64(i)*dx
			y := yMin + float64(j)*dy
			poly := geometry.SimplePolygon([]geometry.Point{
				{X: x, Y: y}, {X: x + 1, Y: y}, {X: x + 1, Y: y + 1},
				{X: x, Y: y + 1}, {X: x, Y: y},
			}, geometry.PseudoMercator)
			geomB.Append(geometry.WKB(poly))
		}
	}
	field := GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// hilbertOfRow returns the Hilbert index of the row-i centroid,
// computed in the given reference bounds. Used by the shared-frame
// test to check that a partition sort produces monotonically
// increasing indices when measured against the same frame every
// caller uses.
func hilbertOfRow(t *testing.T, f *Frame, rowIdx int, bounds geometry.Bounds) uint64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	idx := 0
	for _, chunk := range col.Column().Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if idx == rowIdx {
				g, err := geometry.ParseWKB(bin.Value(i))
				if err != nil {
					t.Fatal(err)
				}
				c := g.Centroid()
				return geometry.HilbertIndex(c.X, c.Y, bounds, geometry.DefaultHilbertOrder)
			}
			idx++
		}
	}
	t.Fatalf("row %d out of range", rowIdx)
	return 0
}

// TestFrame_SortByHilbertWith_OrderTakesEffect: a caller-supplied
// non-default Order should produce a sort permutation distinct from
// the default. Doesn't assert WHICH permutation is right (Hilbert
// is a deterministic function of order+bounds), only that changing
// the order changes the output — regression protection for a bug
// where Order silently gets ignored.
func TestFrame_SortByHilbertWith_OrderTakesEffect(t *testing.T) {
	sharedBounds := geometry.Bounds{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 1000}
	f := gridFrame(t, 0, 1000, 0, 1000, 20)
	defer f.Release()

	// Sort with default (order = 16) and with a very coarse order (2:
	// only 4 cells per axis, so lots of ties broken by the stable sort).
	defaultSort, err := f.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds})
	if err != nil {
		t.Fatal(err)
	}
	defer defaultSort.Release()
	coarseSort, err := f.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds, Order: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer coarseSort.Release()

	// Extract per-row centroid X for both — different orderings
	// should produce different X sequences (with a coarse-enough
	// order, ties dominate and produce a visibly different pattern).
	defaultXs := centroidXSequence(t, defaultSort)
	coarseXs := centroidXSequence(t, coarseSort)
	same := true
	for i := range defaultXs {
		if defaultXs[i] != coarseXs[i] {
			same = false
			break
		}
	}
	if same {
		t.Errorf("SortByHilbertWith Order=2 produced identical ordering to default (order=16) — Order parameter is being ignored")
	}
}

// centroidXSequence returns the X-coordinate of each row's centroid,
// in row order. Used by the Order-parameter test to compare two
// permutations without deep-equal on the full row payload.
func centroidXSequence(t *testing.T, f *Frame) []float64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	xs := make([]float64, 0, f.NumRows())
	for _, chunk := range col.Column().Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				xs = append(xs, 0)
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				t.Fatal(err)
			}
			xs = append(xs, g.Centroid().X)
		}
	}
	return xs
}

// TestFrame_SortByHilbert_NullsLast: null-geometry rows should sort
// to the end so downstream chunking can put them in their own
// row-group (or drop them via Head).
func TestFrame_SortByHilbert_NullsLast(t *testing.T) {
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	geomB.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
		{X: 5, Y: 5}, {X: 6, Y: 5}, {X: 6, Y: 6}, {X: 5, Y: 6}, {X: 5, Y: 5},
	}, geometry.PseudoMercator)))
	geomB.AppendNull()
	geomB.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)))

	field := GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert: %v", err)
	}
	defer sorted.Release()

	geomOut, _ := sorted.Column("geometry")
	bin := geomOut.Column().Data().Chunks()[0].(*array.Binary)
	// Last row must be the null.
	if !bin.IsNull(bin.Len() - 1) {
		t.Errorf("expected null row at last position, got non-null")
	}
	// First two rows must be non-null.
	if bin.IsNull(0) || bin.IsNull(1) {
		t.Errorf("non-null rows not at front of sorted output")
	}
}

// TestFrame_SortByHilbert_NonGeometryColumnErrors: sanity check.
func TestFrame_SortByHilbert_NonGeometryColumnErrors(t *testing.T) {
	pool := memory.DefaultAllocator
	strB := array.NewStringBuilder(pool)
	defer strB.Release()
	strB.Append("a")
	arr := strB.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if _, err := f.SortByHilbert("name"); err == nil {
		t.Errorf("SortByHilbert on non-geometry column should error")
	}
}

// TestFrame_SortByHilbert_EmptyFrame: no rows → returns a Frame
// with the same schema and zero rows, no error.
func TestFrame_SortByHilbert_EmptyFrame(t *testing.T) {
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	arr := geomB.NewArray()
	defer arr.Release()
	field := GeometryField("geometry", 3857)
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert on empty: %v", err)
	}
	defer sorted.Release()
	if sorted.NumRows() != 0 {
		t.Errorf("expected 0 rows, got %d", sorted.NumRows())
	}
}

// TestHilbertSortWithCovering_MatchesTwoPassForm proves the fused
// single-pass path is behaviorally equivalent to the two-step
// SortByHilbert → WithBboxCoveringColumns form. Same row order,
// same column set, same bbox values per row.
//
// Regression protection: if the fused path drifts from the
// canonical two-step semantics (e.g. subtle sort tie-breaking
// change, off-by-one on the permutation, wrong null-mask
// alignment), this catches it.
func TestHilbertSortWithCovering_MatchesTwoPassForm(t *testing.T) {
	// 100-polygon shuffled grid — enough rows to exercise the sort,
	// small enough to compare row-by-row.
	f := gridFrame(t, 0, 200, 0, 200, 10)
	defer f.Release()

	// Two-step reference.
	sortedRef, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert: %v", err)
	}
	defer sortedRef.Release()
	augRef, metaRef, err := WithBboxCoveringColumns(sortedRef)
	if err != nil {
		t.Fatalf("WithBboxCoveringColumns: %v", err)
	}
	defer augRef.Release()

	// Fused single-pass.
	augFused, metaFused, err := HilbertSortWithCovering(f, "geometry")
	if err != nil {
		t.Fatalf("HilbertSortWithCovering: %v", err)
	}
	defer augFused.Release()

	// Column sets must match.
	refNames := augRef.ColumnNames()
	fusedNames := augFused.ColumnNames()
	if len(refNames) != len(fusedNames) {
		t.Fatalf("column count: fused=%d ref=%d", len(fusedNames), len(refNames))
	}
	for i, name := range refNames {
		if fusedNames[i] != name {
			t.Errorf("column %d: fused=%q ref=%q", i, fusedNames[i], name)
		}
	}

	// Row count must match.
	refRows, _ := augRef.Shape()
	fusedRows, _ := augFused.Shape()
	if refRows != fusedRows {
		t.Fatalf("row count: fused=%d ref=%d", fusedRows, refRows)
	}

	// Row-by-row bbox check on the covering columns.
	for _, colName := range []string{
		"geometry_bbox_xmin", "geometry_bbox_ymin",
		"geometry_bbox_xmax", "geometry_bbox_ymax",
	} {
		refVals := float64Column(t, augRef, colName)
		fusedVals := float64Column(t, augFused, colName)
		if len(refVals) != len(fusedVals) {
			t.Fatalf("%s length: fused=%d ref=%d", colName, len(fusedVals), len(refVals))
		}
		for i := range refVals {
			// NaN != NaN semantics — treat two NaNs as equal.
			if math.IsNaN(refVals[i]) && math.IsNaN(fusedVals[i]) {
				continue
			}
			if refVals[i] != fusedVals[i] {
				t.Errorf("%s[%d]: fused=%v ref=%v", colName, i, fusedVals[i], refVals[i])
			}
		}
	}

	// Geo metadata must declare the same covering column paths.
	if metaRef.PrimaryColumn != metaFused.PrimaryColumn {
		t.Errorf("primary column: fused=%q ref=%q",
			metaFused.PrimaryColumn, metaRef.PrimaryColumn)
	}
	refCov := metaRef.Columns["geometry"].Covering
	fusedCov := metaFused.Columns["geometry"].Covering
	if refCov == nil || fusedCov == nil {
		t.Fatalf("covering nil: fused=%v ref=%v", fusedCov, refCov)
	}
	if !sliceEqual(refCov.Bbox.Xmin, fusedCov.Bbox.Xmin) ||
		!sliceEqual(refCov.Bbox.Ymin, fusedCov.Bbox.Ymin) ||
		!sliceEqual(refCov.Bbox.Xmax, fusedCov.Bbox.Xmax) ||
		!sliceEqual(refCov.Bbox.Ymax, fusedCov.Bbox.Ymax) {
		t.Errorf("covering paths differ: fused=%+v ref=%+v", fusedCov.Bbox, refCov.Bbox)
	}
}

// float64Column extracts a Float64 column's values across all
// chunks as a flat []float64. Used by the equivalence test for
// row-by-row bbox comparison.
func float64Column(t *testing.T, f *Frame, colName string) []float64 {
	t.Helper()
	col, err := f.Column(colName)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]float64, 0, f.NumRows())
	for _, chunk := range col.Column().Data().Chunks() {
		fa := chunk.(*array.Float64)
		for i := range fa.Len() {
			if fa.IsNull(i) {
				out = append(out, math.NaN())
				continue
			}
			out = append(out, fa.Value(i))
		}
	}
	return out
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHilbertSortWithCovering_EmptyFrame degenerates to the
// standard WithBboxCoveringColumns behavior (no rows, empty
// bboxes column). Regression protection for the early-return
// branch.
func TestHilbertSortWithCovering_EmptyFrame(t *testing.T) {
	f := gridFrame(t, 0, 0, 0, 0, 0) // 0×0 → empty frame
	defer f.Release()
	if f.NumRows() != 0 {
		t.Fatalf("fixture broken: expected 0 rows, got %d", f.NumRows())
	}
	out, meta, err := HilbertSortWithCovering(f, "geometry")
	if err != nil {
		t.Fatalf("HilbertSortWithCovering: %v", err)
	}
	defer out.Release()
	if meta == nil || meta.PrimaryColumn != "geometry" {
		t.Errorf("empty frame should still emit geo metadata; got %+v", meta)
	}
}

// TestFrame_SortBySTR_TightRowGroupBboxes: same invariant as the
// Hilbert-sort tightness test — STR should also collapse row-group
// bboxes on a shuffled grid.
func TestFrame_SortBySTR_TightRowGroupBboxes(t *testing.T) {
	// Grid over [0..2500] × [0..2500]. Insertion order is
	// column-major (spatially incoherent for row-major row-groups).
	f := gridFrame(t, 0, 2500, 0, 2500, 50) // 50*50 = 2500 polygons
	defer f.Release()

	sorted, err := f.SortBySTR("geometry", 50)
	if err != nil {
		t.Fatalf("SortBySTR: %v", err)
	}
	defer sorted.Release()

	unsorted := chunkedDiagonal(t, f, 50)
	sortedD := chunkedDiagonal(t, sorted, 50)
	if sortedD*2 > unsorted {
		t.Errorf("STR sort didn't tighten row-group bboxes enough: sorted=%.2f unsorted=%.2f",
			sortedD, unsorted)
	}
}

// TestFrame_SortBySTR_NullsLast — same contract as Hilbert.
func TestFrame_SortBySTR_NullsLast(t *testing.T) {
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	geomB.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
		{X: 5, Y: 5}, {X: 6, Y: 5}, {X: 6, Y: 6}, {X: 5, Y: 6}, {X: 5, Y: 5},
	}, geometry.PseudoMercator)))
	geomB.AppendNull()
	geomB.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)))

	field := GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	sorted, err := f.SortBySTR("geometry", 10)
	if err != nil {
		t.Fatalf("SortBySTR: %v", err)
	}
	defer sorted.Release()

	geomOut, _ := sorted.Column("geometry")
	bin := geomOut.Column().Data().Chunks()[0].(*array.Binary)
	if !bin.IsNull(bin.Len() - 1) {
		t.Errorf("expected null row at last position, got non-null")
	}
}

// TestFrame_SortBySTR_DefaultLeafSize: leafSize <= 0 falls back
// to STRDefaultLeafSize. With N << default (25 << 5000), the STR
// algorithm degenerates to a single strip → sorted purely by X.
// Assert that ordering to prove the leafSize path did the right
// thing rather than just checking row count.
func TestFrame_SortBySTR_DefaultLeafSize(t *testing.T) {
	f := gridFrame(t, 0, 100, 0, 100, 5) // 5×5 = 25 polygons
	defer f.Release()
	// leafSize=0 → default (5000, bigger than N → single strip).
	sorted, err := f.SortBySTR("geometry", 0)
	if err != nil {
		t.Fatalf("SortBySTR: %v", err)
	}
	defer sorted.Release()
	if sorted.NumRows() != f.NumRows() {
		t.Fatalf("row count changed: sorted=%d original=%d",
			sorted.NumRows(), f.NumRows())
	}
	// Single-strip STR: pass 1 sorts the whole set by X into one
	// strip, then pass 2 sorts within that strip by Y. Net effect:
	// the output is Y-monotone (not X-monotone — the within-strip
	// Y-sort clobbers pass 1's X order). Asserting Y-monotonicity
	// proves pass 2 ran on the whole set as a single strip, which
	// is the leafSize > N branch's whole contract.
	ys := centroidYSequence(t, sorted)
	for i := 1; i < len(ys); i++ {
		if ys[i] < ys[i-1] {
			t.Errorf("single-strip STR not Y-monotone at row %d: %v -> %v",
				i, ys[i-1], ys[i])
			break
		}
	}
}

// centroidYSequence returns the Y-coordinate of each row's centroid,
// in row order. Companion to centroidXSequence in sort_hilbert_test.
func centroidYSequence(t *testing.T, f *Frame) []float64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	ys := make([]float64, 0, f.NumRows())
	for _, chunk := range col.Column().Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				ys = append(ys, 0)
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				t.Fatal(err)
			}
			ys = append(ys, g.Centroid().Y)
		}
	}
	return ys
}

// TestFrame_SortBySTR_MissingColumnErrors: a bogus column name
// surfaces an error rather than panicking. The gridFrame fixture
// carries only a geometry column, so a "not a geometry column"
// case would need a different fixture — covered separately if we
// ever need it.
func TestFrame_SortBySTR_MissingColumnErrors(t *testing.T) {
	f := gridFrame(t, 0, 10, 0, 10, 3)
	defer f.Release()
	if _, err := f.SortBySTR("nonexistent", 10); err == nil {
		t.Errorf("expected error for missing column")
	}
}

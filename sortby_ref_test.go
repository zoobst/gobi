package gobi

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestSortBy_MatchesStableReference checks SortBy against a plain
// stable sort over the same comparators, row for row. SortBy sorts
// (value, row) pairs with an unstable algorithm and relies on a
// row-number tie-break for stability, and sets nulls and NaNs aside
// before sorting, so this pins the two things that could drift: tie
// order (heavy duplicates, both directions) and the null / NaN tail.
func TestSortBy_MatchesStableReference(t *testing.T) {
	const n = 5000
	r := rand.New(rand.NewPCG(7, 11))
	ints := make([]int64, n)
	few := make([]int64, n)
	floats := make([]float64, n)
	strs := make([]string, n)
	bools := make([]bool, n)
	valid := make([]bool, n)
	rows := make([]int64, n)
	for i := range n {
		ints[i] = r.Int64N(1000) - 500
		few[i] = r.Int64N(4)
		floats[i] = float64(r.IntN(200)) / 4
		switch r.IntN(12) {
		case 0:
			floats[i] = math.NaN()
		case 1:
			floats[i] = math.Inf(1 - 2*r.IntN(2))
		case 2:
			floats[i] = math.Copysign(0, -1)
		}
		strs[i] = fmt.Sprintf("s%03d", r.IntN(300))
		bools[i] = r.IntN(2) == 0
		valid[i] = r.IntN(7) != 0
		rows[i] = int64(i)
	}
	f, err := NewFrameFromSeries(
		NewInt64Series("int", ints, nil),
		NewInt64Series("int_nulls", ints, valid),
		NewInt64Series("few", few, nil),
		NewFloat64Series("float", floats, nil),
		NewFloat64Series("float_nulls", floats, valid),
		NewStringSeries("str", strs, nil),
		NewStringSeries("str_nulls", strs, valid),
		NewBoolSeries("bool", bools, valid),
		NewInt64Series("row", rows, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	keySets := [][]SortKey{
		{{Column: "int"}}, {{Column: "int_nulls"}}, {{Column: "few"}},
		{{Column: "float"}}, {{Column: "float_nulls"}},
		{{Column: "str"}}, {{Column: "str_nulls"}},
		{{Column: "bool"}}, // comparator path
		{{Column: "few"}, {Column: "float_nulls"}},
		{{Column: "bool"}, {Column: "str"}, {Column: "int"}},
	}
	for _, base := range keySets {
		// Every combination of directions across the keys.
		for mask := range 1 << len(base) {
			keys := slices.Clone(base)
			name := ""
			for i := range keys {
				keys[i].Descending = mask&(1<<i) != 0
				name += fmt.Sprintf("%s:%v ", keys[i].Column, keys[i].Descending)
			}
			t.Run(name, func(t *testing.T) {
				want := make([]int64, n)
				copy(want, rows)
				cmps := make([]rowComparator, len(keys))
				for i, k := range keys {
					s, err := f.Column(k.Column)
					if err != nil {
						t.Fatal(err)
					}
					c, release, err := newRowComparator(s, k.Descending)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					cmps[i] = c
				}
				sort.SliceStable(want, func(a, b int) bool {
					for _, c := range cmps {
						if v := c(int(want[a]), int(want[b])); v != 0 {
							return v < 0
						}
					}
					return false
				})

				out, err := f.SortBy(keys...)
				if err != nil {
					t.Fatal(err)
				}
				defer out.Release()
				rs, err := out.Column("row")
				if err != nil {
					t.Fatal(err)
				}
				got, err := rs.Int64s()
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, want) {
					for i := range got {
						if got[i] != want[i] {
							t.Fatalf("row order differs from a stable sort at position %d: got row %d, want row %d", i, got[i], want[i])
						}
					}
				}
			})
		}
	}
}

// Every key type the single-key path sorts directly must also have a
// comparator, or it would sort as the only key and be rejected as one
// of several; and the two paths must agree on the order.
func TestSortBy_SingleKeyTypesHaveComparators(t *testing.T) {
	pool := memory.DefaultAllocator
	// Three values with a tie and a null: v[0] > v[1] == v[3], row 2 null.
	build := func(b array.Builder, appendAt func(i int)) arrow.Array {
		defer b.Release()
		for i := range 4 {
			if i == 2 {
				b.AppendNull()
				continue
			}
			appendAt(i)
		}
		return b.NewArray()
	}
	n := []int64{7, 3, 0, 3}
	arrays := map[string]arrow.Array{}
	{
		b := array.NewInt64Builder(pool)
		arrays["int64"] = build(b, func(i int) { b.Append(n[i]) })
	}
	{
		b := array.NewInt32Builder(pool)
		arrays["int32"] = build(b, func(i int) { b.Append(int32(n[i])) })
	}
	{
		b := array.NewInt16Builder(pool)
		arrays["int16"] = build(b, func(i int) { b.Append(int16(n[i])) })
	}
	{
		b := array.NewInt8Builder(pool)
		arrays["int8"] = build(b, func(i int) { b.Append(int8(n[i])) })
	}
	{
		b := array.NewUint64Builder(pool)
		arrays["uint64"] = build(b, func(i int) { b.Append(uint64(n[i])) })
	}
	{
		b := array.NewUint32Builder(pool)
		arrays["uint32"] = build(b, func(i int) { b.Append(uint32(n[i])) })
	}
	{
		b := array.NewUint16Builder(pool)
		arrays["uint16"] = build(b, func(i int) { b.Append(uint16(n[i])) })
	}
	{
		b := array.NewUint8Builder(pool)
		arrays["uint8"] = build(b, func(i int) { b.Append(uint8(n[i])) })
	}
	{
		b := array.NewFloat64Builder(pool)
		arrays["float64"] = build(b, func(i int) { b.Append(float64(n[i])) })
	}
	{
		b := array.NewFloat32Builder(pool)
		arrays["float32"] = build(b, func(i int) { b.Append(float32(n[i])) })
	}
	{
		b := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
		arrays["timestamp"] = build(b, func(i int) { b.Append(arrow.Timestamp(n[i])) })
	}
	{
		b := array.NewDate32Builder(pool)
		arrays["date32"] = build(b, func(i int) { b.Append(arrow.Date32(n[i])) })
	}
	{
		b := array.NewDate64Builder(pool)
		arrays["date64"] = build(b, func(i int) { b.Append(arrow.Date64(n[i])) })
	}
	{
		b := array.NewTime32Builder(pool, &arrow.Time32Type{Unit: arrow.Second})
		arrays["time32"] = build(b, func(i int) { b.Append(arrow.Time32(n[i])) })
	}
	{
		b := array.NewTime64Builder(pool, &arrow.Time64Type{Unit: arrow.Microsecond})
		arrays["time64"] = build(b, func(i int) { b.Append(arrow.Time64(n[i])) })
	}
	{
		b := array.NewDurationBuilder(pool, &arrow.DurationType{Unit: arrow.Second})
		arrays["duration"] = build(b, func(i int) { b.Append(arrow.Duration(n[i])) })
	}
	{
		b := array.NewStringBuilder(pool)
		arrays["string"] = build(b, func(i int) { b.Append(fmt.Sprint(n[i])) })
	}
	{
		b := array.NewLargeStringBuilder(pool)
		arrays["large_string"] = build(b, func(i int) { b.Append(fmt.Sprint(n[i])) })
	}
	{
		b := array.NewStringViewBuilder(pool)
		arrays["string_view"] = build(b, func(i int) { b.Append(fmt.Sprint(n[i])) })
	}
	for name, arr := range arrays {
		defer arr.Release()
		for _, desc := range []bool{false, true} {
			got, ok := sortSingleKey(arr, desc)
			if !ok {
				t.Errorf("%s: not on the single-key path; drop it from this test or add it to sortSingleKey", name)
				continue
			}
			cmp, err := arrayComparator(arr, desc)
			if err != nil {
				t.Errorf("%s: sorts as a single key but has no comparator: %v", name, err)
				continue
			}
			want := []int{0, 1, 2, 3}
			sortRowsBy(want, []rowComparator{cmp})
			if !slices.Equal(got, want) {
				t.Errorf("%s (descending=%v): single-key order %v, comparator order %v", name, desc, got, want)
			}
			// Rows 1 and 3 tie and stay in input order; the null is last.
			if i1, i3 := slices.Index(got, 1), slices.Index(got, 3); i1 > i3 || got[3] != 2 {
				t.Errorf("%s (descending=%v): order %v breaks tie order or null-last", name, desc, got)
			}
		}
	}
}

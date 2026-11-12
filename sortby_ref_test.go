package gobi

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
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

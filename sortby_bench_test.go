package gobi

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// benchSortFrame builds an n-row frame with one column per key shape
// SortBy has a distinct path for: unique int64, low-cardinality int64
// (long runs of ties, which stress stability), float64, int64 with
// nulls, and string.
func benchSortFrame(tb testing.TB, n int) *Frame {
	tb.Helper()
	r := rand.New(rand.NewPCG(1, 2))
	ids := make([]int64, n)
	cats := make([]int64, n)
	vals := make([]float64, n)
	names := make([]string, n)
	valid := make([]bool, n)
	for i := range n {
		ids[i] = r.Int64N(1 << 40)
		cats[i] = r.Int64N(16)
		vals[i] = r.Float64() * 1000
		names[i] = fmt.Sprintf("name-%07d", r.IntN(n))
		valid[i] = r.IntN(10) != 0
	}
	f, err := NewFrameFromSeries(
		NewInt64Series("id", ids, nil),
		NewInt64Series("cat", cats, nil),
		NewFloat64Series("val", vals, nil),
		NewInt64Series("sparse", ids, valid),
		NewStringSeries("name", names, nil),
	)
	if err != nil {
		tb.Fatal(err)
	}
	return f
}

// BenchmarkSortBy sorts 1M rows by each key shape. The whole frame is
// reordered, so the numbers include take as well as the sort itself.
func BenchmarkSortBy(b *testing.B) {
	const n = 1_000_000
	f := benchSortFrame(b, n)
	defer f.Release()
	cases := []struct {
		name string
		keys []SortKey
	}{
		{"int64", []SortKey{{Column: "id"}}},
		{"int64_desc", []SortKey{{Column: "id", Descending: true}}},
		{"int64_ties", []SortKey{{Column: "cat"}}},
		{"float64", []SortKey{{Column: "val"}}},
		{"int64_nulls", []SortKey{{Column: "sparse"}}},
		{"string", []SortKey{{Column: "name"}}},
		{"two_keys", []SortKey{{Column: "cat"}, {Column: "val", Descending: true}}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				out, err := f.SortBy(c.keys...)
				if err != nil {
					b.Fatal(err)
				}
				out.Release()
			}
		})
	}
}

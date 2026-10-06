package compute

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestAndChainF64_BranchlessMatchesShortCircuit — the branchless bodies
// give exactly the `&&` definition's results, including the edge cases
// a rewrite could get wrong: NaN (always out), ±Inf, values exactly on
// a bound (inclusive), -0 vs +0, and an empty range (lo > hi).
func TestAndChainF64_BranchlessMatchesShortCircuit(t *testing.T) {
	special := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, math.Copysign(0, -1), 1, 2, 3, -1}
	r := rand.New(rand.NewPCG(5, 6))
	a := append([]float64{}, special...)
	b := append([]float64{}, special...)
	for range 5000 {
		a = append(a, r.Float64()*6-2)
		b = append(b, r.Float64()*6-2)
	}
	b = b[:len(a)]
	ranges := [][2]float64{{1, 2}, {0, 0}, {2, 1}, {math.Inf(-1), math.Inf(1)}, {-1, 3}, {math.NaN(), 1}}
	out := make([]bool, len(a))
	for _, rg := range ranges {
		lo, hi := rg[0], rg[1]
		AndChainF64Range(a, lo, hi, out)
		for i, v := range a {
			if want := lo <= v && v <= hi; out[i] != want {
				t.Fatalf("Range[%v,%v](%v) = %v, want %v", lo, hi, v, out[i], want)
			}
		}
		for _, rg2 := range ranges {
			bLo, bHi := rg2[0], rg2[1]
			AndChainF64BBox(a, lo, hi, b, bLo, bHi, out)
			for i := range a {
				want := lo <= a[i] && a[i] <= hi && bLo <= b[i] && b[i] <= bHi
				if out[i] != want {
					t.Fatalf("BBox x[%v,%v] y[%v,%v] at (%v,%v) = %v, want %v",
						lo, hi, bLo, bHi, a[i], b[i], out[i], want)
				}
			}
		}
	}
}

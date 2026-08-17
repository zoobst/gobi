package compute

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestReduceParity exercises every reduction kernel against a
// scalar oracle on random data. Same test runs under both the
// SIMD and scalar builds (via `go test` / `GOEXPERIMENT=simd
// go1.27rc1 test`), so a divergence between paths shows up in CI
// or local sweep.
func TestReduceParity(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	// Include sizes that hit both the vectorized body and the
	// non-lane-aligned tail (arm64 NEON has Float64s laneCount=2,
	// Int64s laneCount=2), plus edge cases: empty, 1-elem,
	// exactly-one-lane-group.
	sizes := []int{0, 1, 2, 3, 4, 7, 8, 100, 1024, 4097}
	for _, n := range sizes {
		a := make([]float64, n)
		ai := make([]int64, n)
		for i := range a {
			// Range mostly in [-500, 500] but with a few outliers
			// so Min/Max find real extremes near the ends of the
			// array (not just at index 0).
			a[i] = rng.Float64()*1000 - 500
			ai[i] = rng.Int64N(1_000_000) - 500_000
		}

		// Oracle
		var wantSum float64
		var wantSumI int64
		wantMin, wantMax := math.Inf(1), math.Inf(-1)
		wantMinI, wantMaxI := int64(math.MaxInt64), int64(math.MinInt64)
		for i, v := range a {
			wantSum += v
			if v < wantMin {
				wantMin = v
			}
			if v > wantMax {
				wantMax = v
			}
			iv := ai[i]
			wantSumI += iv
			if iv < wantMinI {
				wantMinI = iv
			}
			if iv > wantMaxI {
				wantMaxI = iv
			}
		}

		gotSum := SumF64(a)
		gotSumI := SumI64(ai)
		gotMin, minOK := MinF64(a)
		gotMax, maxOK := MaxF64(a)
		gotMinI, minIOK := MinI64(ai)
		gotMaxI, maxIOK := MaxI64(ai)

		// Float sum: tolerate lane-order-dependent rounding by
		// comparing at ~1e-9 relative tolerance. Sum of 100+
		// float64 values in [-500, 500] can differ in the last
		// mantissa bit between scalar-in-order and SIMD-parallel-
		// lane reductions; that's not a correctness issue.
		if diff := gotSum - wantSum; math.Abs(diff) > 1e-9*math.Abs(wantSum)+1e-9 {
			t.Errorf("n=%d SumF64: got %v, want %v (diff=%v)", n, gotSum, wantSum, diff)
		}
		if gotSumI != wantSumI {
			t.Errorf("n=%d SumI64: got %d, want %d", n, gotSumI, wantSumI)
		}

		// Min/Max: exact match required — bit-identical, no reduction-order sensitivity.
		if n == 0 {
			if minOK || maxOK || minIOK || maxIOK {
				t.Errorf("n=0: expected all reduces to report !ok, got Min=%v,Max=%v,MinI=%v,MaxI=%v",
					minOK, maxOK, minIOK, maxIOK)
			}
		} else {
			if !minOK || gotMin != wantMin {
				t.Errorf("n=%d MinF64: got (%v, %v), want (%v, true)", n, gotMin, minOK, wantMin)
			}
			if !maxOK || gotMax != wantMax {
				t.Errorf("n=%d MaxF64: got (%v, %v), want (%v, true)", n, gotMax, maxOK, wantMax)
			}
			if !minIOK || gotMinI != wantMinI {
				t.Errorf("n=%d MinI64: got (%d, %v), want (%d, true)", n, gotMinI, minIOK, wantMinI)
			}
			if !maxIOK || gotMaxI != wantMaxI {
				t.Errorf("n=%d MaxI64: got (%d, %v), want (%d, true)", n, gotMaxI, maxIOK, wantMaxI)
			}
		}
	}
}

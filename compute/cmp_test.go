package compute

import (
	"math/rand/v2"
	"testing"
)

// TestCmpParity exercises every compare kernel against a scalar
// oracle on random data. Both the scalar and SIMD builds ship
// through this — same test file, same signatures, same expected
// outputs.
func TestCmpParity(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	// Include lengths that hit both the vectorized body AND the
	// non-lane-aligned tail (arm64 NEON has laneCount=2, so odd
	// lengths force the tail path; use several to be safe).
	sizes := []int{0, 1, 2, 3, 7, 8, 15, 16, 100, 4097}
	for _, n := range sizes {
		a := make([]float64, n)
		for i := range a {
			a[i] = rng.Float64()*100 - 50
		}
		b := 0.0

		wantGe := make([]bool, n)
		wantLe := make([]bool, n)
		wantGt := make([]bool, n)
		wantLt := make([]bool, n)
		wantRange := make([]bool, n)
		for i, v := range a {
			wantGe[i] = v >= b
			wantLe[i] = v <= b
			wantGt[i] = v > b
			wantLt[i] = v < b
			wantRange[i] = -10 <= v && v <= 10
		}

		gotGe := make([]bool, n)
		gotLe := make([]bool, n)
		gotGt := make([]bool, n)
		gotLt := make([]bool, n)
		gotRange := make([]bool, n)
		gotDist := make([]bool, n)
		CmpF64Ge(a, b, gotGe)
		CmpF64Le(a, b, gotLe)
		CmpF64Gt(a, b, gotGt)
		CmpF64Lt(a, b, gotLt)
		AndChainF64Range(a, -10, 10, gotRange)

		// Distance kernel: reuse `a` for lats, generate a matching
		// lons slice from the RNG so both are non-trivial. refLat=0,
		// refLon=0, cosRefLat=1 (equator, no scaling), threshold
		// = 30² (rough — half the value range).
		lons := make([]float64, n)
		wantDist := make([]bool, n)
		for i := range n {
			lons[i] = rng.Float64()*100 - 50
			dLat := a[i]
			dLon := lons[i]
			wantDist[i] = dLat*dLat+dLon*dLon <= 900
		}
		WithinSqDistF64(a, lons, 0, 0, 1.0, 900, gotDist)

		for i := range n {
			if gotGe[i] != wantGe[i] {
				t.Fatalf("n=%d CmpF64Ge[%d]: got %v, want %v (a=%v)", n, i, gotGe[i], wantGe[i], a[i])
			}
			if gotLe[i] != wantLe[i] {
				t.Fatalf("n=%d CmpF64Le[%d]: got %v, want %v", n, i, gotLe[i], wantLe[i])
			}
			if gotGt[i] != wantGt[i] {
				t.Fatalf("n=%d CmpF64Gt[%d]: got %v, want %v", n, i, gotGt[i], wantGt[i])
			}
			if gotLt[i] != wantLt[i] {
				t.Fatalf("n=%d CmpF64Lt[%d]: got %v, want %v", n, i, gotLt[i], wantLt[i])
			}
			if gotRange[i] != wantRange[i] {
				t.Fatalf("n=%d AndChainF64Range[%d]: got %v, want %v", n, i, gotRange[i], wantRange[i])
			}
			if gotDist[i] != wantDist[i] {
				t.Fatalf("n=%d WithinSqDistF64[%d]: got %v, want %v (lat=%v lon=%v)",
					n, i, gotDist[i], wantDist[i], a[i], lons[i])
			}
		}
	}
}

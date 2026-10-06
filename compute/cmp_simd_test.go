//go:build goexperiment.simd && (arm64 || amd64)

// SIMD-body parity tests. The public cmp entry points skip the SIMD
// path on 2-lane NEON (see cmpKernelSIMDEligible in cmp_simd.go), so
// on Apple hardware the parity tests in cmp_test.go only exercise the
// scalar fallback declared inside the SIMD build. This file calls the
// unexported *SIMDBody functions directly so vector-kernel regressions
// surface in CI regardless of the runtime lane count. Mirrors the
// pipCrossingCountSIMDBody test in geom_simd_test.go.

package compute

import (
	"math/rand/v2"
	"testing"
)

func TestFusedSIMDBody_MatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(55, 66))
	sizes := []int{0, 1, 2, 3, 7, 8, 15, 16, 100, 4097}
	for _, n := range sizes {
		a := make([]float64, n)
		b := make([]float64, n)
		for i := range a {
			a[i] = rng.Float64()*100 - 50
			b[i] = rng.Float64()*100 - 50
		}
		wantBBox := make([]bool, n)
		for i := range n {
			wantBBox[i] = -20 <= a[i] && a[i] <= 20 && -30 <= b[i] && b[i] <= 30
		}
		gotBBox := make([]bool, n)
		andChainF64BBoxSIMDBody(a, -20, 20, b, -30, 30, gotBBox)
		for i := range n {
			if gotBBox[i] != wantBBox[i] {
				t.Fatalf("n=%d andChainF64BBoxSIMDBody[%d]: got %v, want %v", n, i, gotBBox[i], wantBBox[i])
			}
		}
	}
}

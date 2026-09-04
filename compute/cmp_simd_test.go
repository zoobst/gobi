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

func TestCmpF64SIMDBody_MatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 22))
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
		for i, v := range a {
			wantGe[i] = v >= b
			wantLe[i] = v <= b
			wantGt[i] = v > b
			wantLt[i] = v < b
		}
		gotGe := make([]bool, n)
		gotLe := make([]bool, n)
		gotGt := make([]bool, n)
		gotLt := make([]bool, n)
		cmpF64GeSIMDBody(a, b, gotGe)
		cmpF64LeSIMDBody(a, b, gotLe)
		cmpF64GtSIMDBody(a, b, gotGt)
		cmpF64LtSIMDBody(a, b, gotLt)
		for i := range n {
			if gotGe[i] != wantGe[i] {
				t.Fatalf("n=%d cmpF64GeSIMDBody[%d]: got %v, want %v", n, i, gotGe[i], wantGe[i])
			}
			if gotLe[i] != wantLe[i] {
				t.Fatalf("n=%d cmpF64LeSIMDBody[%d]: got %v, want %v", n, i, gotLe[i], wantLe[i])
			}
			if gotGt[i] != wantGt[i] {
				t.Fatalf("n=%d cmpF64GtSIMDBody[%d]: got %v, want %v", n, i, gotGt[i], wantGt[i])
			}
			if gotLt[i] != wantLt[i] {
				t.Fatalf("n=%d cmpF64LtSIMDBody[%d]: got %v, want %v", n, i, gotLt[i], wantLt[i])
			}
		}
	}
}

func TestCmpI64SIMDBody_MatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(33, 44))
	sizes := []int{0, 1, 2, 3, 7, 8, 15, 16, 100, 4097}
	for _, n := range sizes {
		a := make([]int64, n)
		for i := range a {
			a[i] = int64(rng.Uint64()) % 200
		}
		var b int64 = 42
		wantGe := make([]bool, n)
		wantLe := make([]bool, n)
		wantGt := make([]bool, n)
		wantLt := make([]bool, n)
		for i, v := range a {
			wantGe[i] = v >= b
			wantLe[i] = v <= b
			wantGt[i] = v > b
			wantLt[i] = v < b
		}
		gotGe := make([]bool, n)
		gotLe := make([]bool, n)
		gotGt := make([]bool, n)
		gotLt := make([]bool, n)
		cmpI64GeSIMDBody(a, b, gotGe)
		cmpI64LeSIMDBody(a, b, gotLe)
		cmpI64GtSIMDBody(a, b, gotGt)
		cmpI64LtSIMDBody(a, b, gotLt)
		for i := range n {
			if gotGe[i] != wantGe[i] {
				t.Fatalf("n=%d cmpI64GeSIMDBody[%d]: got %v, want %v", n, i, gotGe[i], wantGe[i])
			}
			if gotLe[i] != wantLe[i] {
				t.Fatalf("n=%d cmpI64LeSIMDBody[%d]: got %v, want %v", n, i, gotLe[i], wantLe[i])
			}
			if gotGt[i] != wantGt[i] {
				t.Fatalf("n=%d cmpI64GtSIMDBody[%d]: got %v, want %v", n, i, gotGt[i], wantGt[i])
			}
			if gotLt[i] != wantLt[i] {
				t.Fatalf("n=%d cmpI64LtSIMDBody[%d]: got %v, want %v", n, i, gotLt[i], wantLt[i])
			}
		}
	}
}

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
		wantRange := make([]bool, n)
		wantBBox := make([]bool, n)
		wantDist := make([]bool, n)
		for i := range n {
			wantRange[i] = -10 <= a[i] && a[i] <= 10
			wantBBox[i] = -20 <= a[i] && a[i] <= 20 && -30 <= b[i] && b[i] <= 30
			dLat := a[i]
			dLon := b[i]
			wantDist[i] = dLat*dLat+dLon*dLon <= 900
		}
		gotRange := make([]bool, n)
		gotBBox := make([]bool, n)
		gotDist := make([]bool, n)
		andChainF64RangeSIMDBody(a, -10, 10, gotRange)
		andChainF64BBoxSIMDBody(a, -20, 20, b, -30, 30, gotBBox)
		withinSqDistF64SIMDBody(a, b, 0, 0, 1.0, 900, gotDist)
		for i := range n {
			if gotRange[i] != wantRange[i] {
				t.Fatalf("n=%d andChainF64RangeSIMDBody[%d]: got %v, want %v", n, i, gotRange[i], wantRange[i])
			}
			if gotBBox[i] != wantBBox[i] {
				t.Fatalf("n=%d andChainF64BBoxSIMDBody[%d]: got %v, want %v", n, i, gotBBox[i], wantBBox[i])
			}
			if gotDist[i] != wantDist[i] {
				t.Fatalf("n=%d withinSqDistF64SIMDBody[%d]: got %v, want %v", n, i, gotDist[i], wantDist[i])
			}
		}
	}
}

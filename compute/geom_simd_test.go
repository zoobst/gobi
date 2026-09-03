//go:build goexperiment.simd && (arm64 || amd64)

package compute

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestPIPCrossingCountSIMDBody_MatchesScalar — the public
// PIPCrossingCount skips the SIMD body on 2-lane NEON (see the
// `lane < 4` gate in geom_simd.go), so the parity test in
// geom_test.go never touches it on Apple. This variant calls the
// SIMD body directly with an explicit lane count so regressions
// in the vector kernel surface even without amd64 hardware.
//
// Tests the kernel at every lane count the compile target might
// see at runtime (2 = NEON, 4 = AVX2, 8 = AVX-512). The kernel
// is agnostic to the caller's `lane` argument as long as it
// matches what `simd.BroadcastFloat64s(0).Len()` returns at
// runtime — but for correctness testing we can walk the lane
// counts explicitly.
func TestPIPCrossingCountSIMDBody_MatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(123, 456))
	// Query the runtime lane count once. On this hardware only
	// lane==actual will produce a meaningful bench, but the
	// correctness test uses whatever the runtime reports.
	lane := runtimeLane()
	// Sizes past simdMinSize=64 so the body actually walks the
	// vector loop, with non-lane-aligned tails on every arch.
	sizes := []int{64, 65, 100, 127, 128, 129, 256, 1024, 4097}
	for _, n := range sizes {
		if n < lane+1 {
			continue
		}
		xs := make([]float64, n+1)
		ys := make([]float64, n+1)
		for i := range n {
			theta := 2.0 * math.Pi * float64(i) / float64(n)
			xs[i] = 500 + 400*math.Cos(theta)
			ys[i] = 500 + 400*math.Sin(theta)
		}
		xs[n] = xs[0]
		ys[n] = ys[0]

		for range 50 {
			tx := 100 + rng.Float64()*800
			ty := 100 + rng.Float64()*800
			got := pipCrossingCountSIMDBody(xs, ys, tx, ty, len(xs), lane)
			want := oracleToggle(xs, ys, tx, ty)
			if got != want {
				t.Errorf("n=%d @ (%v,%v): SIMD-body got %v, want %v",
					n, tx, ty, got, want)
			}
		}
	}
}

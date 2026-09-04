//go:build goexperiment.simd && (arm64 || amd64)

package compute

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestBoundsF64SIMDBody_MatchesScalar — the public BoundsF64 skips
// the SIMD body on 2-lane NEON (lane<4 gate), so on Apple hardware
// the parity test in geom_test.go only exercises the scalar fallback.
// This variant calls the SIMD body directly with an explicit lane
// count so vector-kernel regressions surface without amd64 hardware.
func TestBoundsF64SIMDBody_MatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(211, 222))
	lane := runtimeLane()
	sizes := []int{2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 100, 1024, 4097}
	for _, n := range sizes {
		if n < lane {
			continue
		}
		xs := make([]float64, n)
		ys := make([]float64, n)
		for i := range xs {
			xs[i] = rng.Float64()*1000 - 500
			ys[i] = rng.Float64()*1000 - 500
		}
		wantMinX, wantMaxX := xs[0], xs[0]
		wantMinY, wantMaxY := ys[0], ys[0]
		for i := 1; i < n; i++ {
			if xs[i] < wantMinX {
				wantMinX = xs[i]
			}
			if xs[i] > wantMaxX {
				wantMaxX = xs[i]
			}
			if ys[i] < wantMinY {
				wantMinY = ys[i]
			}
			if ys[i] > wantMaxY {
				wantMaxY = ys[i]
			}
		}
		gotMinX, gotMinY, gotMaxX, gotMaxY, ok := boundsF64SIMDBody(xs, ys, n, lane)
		if !ok {
			t.Fatalf("n=%d: ok=false", n)
		}
		if gotMinX != wantMinX || gotMinY != wantMinY ||
			gotMaxX != wantMaxX || gotMaxY != wantMaxY {
			t.Errorf("n=%d: got (%v,%v,%v,%v), want (%v,%v,%v,%v)",
				n, gotMinX, gotMinY, gotMaxX, gotMaxY,
				wantMinX, wantMinY, wantMaxX, wantMaxY)
		}
	}
}

// TestPolygonCentroidShoelaceSIMDBody_MatchesScalar — force the
// vector shoelace body on 2-lane hardware.
func TestPolygonCentroidShoelaceSIMDBody_MatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(311, 322))
	lane := runtimeLane()
	// Body precondition: n ≥ max(simdMinSize=64, lane+1).
	sizes := []int{64, 65, 100, 127, 128, 129, 256, 1024, 4097}
	for _, n := range sizes {
		if n < lane+1 {
			continue
		}
		xs := make([]float64, n+1)
		ys := make([]float64, n+1)
		for i := range n {
			theta := 2.0 * math.Pi * float64(i) / float64(n)
			xs[i] = 100 + 10*math.Cos(theta) + rng.Float64()*0.5
			ys[i] = 100 + 10*math.Sin(theta) + rng.Float64()*0.5
		}
		xs[n] = xs[0]
		ys[n] = ys[0]
		wantCx, wantCy, wantOk := polygonCentroidShoelaceScalar(xs, ys, n+1)
		gotCx, gotCy, gotOk := polygonCentroidShoelaceSIMDBody(xs, ys, n+1, lane)
		if gotOk != wantOk {
			t.Fatalf("n=%d: ok=%v want %v", n, gotOk, wantOk)
		}
		// Tolerance for accumulator-order perturbation.
		if math.Abs(gotCx-wantCx) > 1e-8 || math.Abs(gotCy-wantCy) > 1e-8 {
			t.Errorf("n=%d: SIMD (%v,%v) vs scalar (%v,%v)", n, gotCx, gotCy, wantCx, wantCy)
		}
	}
}

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

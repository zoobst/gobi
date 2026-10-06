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
	// Body precondition: n ≥ lane+1 (the public wrapper's size
	// threshold doesn't apply when the body is called directly).
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

// TestBoundsF64SIMDBody_NaNAnywhere — a NaN anywhere on an axis makes
// that axis's min and max NaN, wherever it falls: the first vector,
// the vectorized middle (any lane), or the scalar tail. Hardware
// min/max disagree on NaN (arm64 FMAX/FMIN propagate it; x86
// MAXPD/MINPD return the other operand), so the body must not rely
// on either. Calls the body directly so this runs at the runtime
// lane width on every arch, including 2-lane SSE / NEON.
func TestBoundsF64SIMDBody_NaNAnywhere(t *testing.T) {
	lane := runtimeLane()
	n := 4*lane + lane/2 + 1 // several vectors plus a scalar tail
	for _, pos := range []int{0, 1, lane, 2*lane + lane - 1, n - 1} {
		xs := make([]float64, n)
		ys := make([]float64, n)
		for i := range n {
			xs[i], ys[i] = float64(i), float64(-i)
		}
		xs[pos] = math.NaN()
		mnX, mnY, mxX, mxY, ok := boundsF64SIMDBody(xs, ys, n, lane)
		if !ok {
			t.Fatalf("pos %d: ok=false", pos)
		}
		if !math.IsNaN(mnX) || !math.IsNaN(mxX) {
			t.Errorf("lane %d, NaN at %d: x bounds = [%v, %v], want NaN", lane, pos, mnX, mxX)
		}
		if mnY != float64(-(n-1)) || mxY != 0 {
			t.Errorf("lane %d, NaN at %d: y bounds = [%v, %v], want [%d, 0] (NaN-free axis)", lane, pos, mnY, mxY, -(n - 1))
		}
		// The public entry point (SIMD or scalar path) agrees.
		pmnX, _, pmxX, _, _ := BoundsF64(xs, ys)
		if !math.IsNaN(pmnX) || !math.IsNaN(pmxX) {
			t.Errorf("BoundsF64, NaN at %d: x bounds = [%v, %v], want NaN", pos, pmnX, pmxX)
		}
	}
}

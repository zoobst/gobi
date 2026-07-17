//go:build !(goexperiment.simd && amd64)

// TODO(1.27-simd): Revisit when Go 1.27 ships. Once the amd64 SIMD path is
// rewritten against the portable `simd` package, the build-tag conditions
// here will need to change — likely to `//go:build !goexperiment.simd`,
// since the portable simd path will cover arm64/amd64/wasm in a single
// file. If Go 1.27 mainlines simd (drops the GOEXPERIMENT gate), this
// entire file becomes dead code. See series_ops_simd_amd64.go for the
// full migration plan.

package gobi

// simdEnabled indicates whether the AVX SIMD path is compiled into this
// binary. This file provides the scalar fallback used everywhere except
// amd64 with GOEXPERIMENT=simd; the compiler will inline these tight loops.
const simdEnabled = false

func addF64Kernel(out, a, b []float64) {
	for i := 0; i < len(out); i++ {
		out[i] = a[i] + b[i]
	}
}

func subF64Kernel(out, a, b []float64) {
	for i := 0; i < len(out); i++ {
		out[i] = a[i] - b[i]
	}
}

func mulF64Kernel(out, a, b []float64) {
	for i := 0; i < len(out); i++ {
		out[i] = a[i] * b[i]
	}
}

func divF64Kernel(out, a, b []float64) {
	for i := 0; i < len(out); i++ {
		out[i] = a[i] / b[i]
	}
}

func addScalarF64Kernel(out, a []float64, v float64) {
	for i := 0; i < len(out); i++ {
		out[i] = a[i] + v
	}
}

func mulScalarF64Kernel(out, a []float64, v float64) {
	for i := 0; i < len(out); i++ {
		out[i] = a[i] * v
	}
}

func sumF64Kernel(a []float64) float64 {
	var total float64
	for _, v := range a {
		total += v
	}
	return total
}

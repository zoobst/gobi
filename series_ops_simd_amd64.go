//go:build goexperiment.simd && amd64

// TODO(1.27-simd): When Go 1.27 (Aug 2026) ships, replace this file with a
// portable implementation using the new stdlib `simd` package
// (Float64s + Add/Sub/Mul/Div/Sum). One file covers arm64 NEON, amd64
// AVX2/AVX-512, and WASM SIMD instead of the current amd64-only 256-bit
// Float64x4 kernel. The `simd/archsimd` amd64 API is being revised in
// 1.27, so this file will likely need updates just to compile — a full
// rewrite against the portable package is the cleaner move.
//   - Update build tag: //go:build goexperiment.simd  (drop amd64)
//   - Import "simd" instead of "simd/archsimd"
//   - Delete series_ops_simd_fallback.go if the portable simd path
//     compiles on all supported architectures; otherwise trim its scope
//     to just non-simd builds.
//   - Add a CI job with GOEXPERIMENT=simd so both paths are exercised.
// See: https://go.dev/doc/go1.27 and the Go 1.27 release notes for the
// simd/archsimd revised amd64 API.

package gobi

import "simd/archsimd"

// simdEnabled indicates whether the AVX SIMD path is compiled into this
// binary. It is set to true only for GOEXPERIMENT=simd on amd64; every other
// build uses the scalar fallback in series_ops_simd_fallback.go.
const simdEnabled = true

// The kernels below assume len(a) == len(b) == len(out) and expect the
// caller to have already excluded the "any nulls" case (nulls fall back to
// the per-element path). They process 4 float64s per iteration via
// Float64x4 (256-bit AVX2) and then finish any remainder with scalar code.

func addF64Kernel(out, a, b []float64) {
	n := len(out)
	i := 0
	for ; i+4 <= n; i += 4 {
		va := archsimd.LoadFloat64x4Slice(a[i:])
		vb := archsimd.LoadFloat64x4Slice(b[i:])
		va.Add(vb).StoreSlice(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] + b[i]
	}
}

func subF64Kernel(out, a, b []float64) {
	n := len(out)
	i := 0
	for ; i+4 <= n; i += 4 {
		va := archsimd.LoadFloat64x4Slice(a[i:])
		vb := archsimd.LoadFloat64x4Slice(b[i:])
		va.Sub(vb).StoreSlice(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] - b[i]
	}
}

func mulF64Kernel(out, a, b []float64) {
	n := len(out)
	i := 0
	for ; i+4 <= n; i += 4 {
		va := archsimd.LoadFloat64x4Slice(a[i:])
		vb := archsimd.LoadFloat64x4Slice(b[i:])
		va.Mul(vb).StoreSlice(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] * b[i]
	}
}

func divF64Kernel(out, a, b []float64) {
	n := len(out)
	i := 0
	for ; i+4 <= n; i += 4 {
		va := archsimd.LoadFloat64x4Slice(a[i:])
		vb := archsimd.LoadFloat64x4Slice(b[i:])
		va.Div(vb).StoreSlice(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] / b[i]
	}
}

func addScalarF64Kernel(out, a []float64, v float64) {
	n := len(out)
	vv := archsimd.BroadcastFloat64x4(v)
	i := 0
	for ; i+4 <= n; i += 4 {
		va := archsimd.LoadFloat64x4Slice(a[i:])
		va.Add(vv).StoreSlice(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] + v
	}
}

func mulScalarF64Kernel(out, a []float64, v float64) {
	n := len(out)
	vv := archsimd.BroadcastFloat64x4(v)
	i := 0
	for ; i+4 <= n; i += 4 {
		va := archsimd.LoadFloat64x4Slice(a[i:])
		va.Mul(vv).StoreSlice(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] * v
	}
}

// sumF64Kernel returns the sum of a using four parallel accumulators. Order
// of reduction differs from a strict left-to-right scalar sum, so results
// may differ by a few ULPs from the scalar path for very ill-conditioned
// inputs; standard practice in vectorized reductions.
func sumF64Kernel(a []float64) float64 {
	n := len(a)
	if n == 0 {
		return 0
	}
	i := 0
	acc := archsimd.BroadcastFloat64x4(0)
	for ; i+4 <= n; i += 4 {
		acc = acc.Add(archsimd.LoadFloat64x4Slice(a[i:]))
	}
	// Horizontal sum of the 4-lane accumulator.
	var buf [4]float64
	acc.Store(&buf)
	total := buf[0] + buf[1] + buf[2] + buf[3]
	for ; i < n; i++ {
		total += a[i]
	}
	return total
}

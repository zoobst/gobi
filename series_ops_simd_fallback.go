//go:build !goexperiment.simd || (!arm64 && !amd64)

// Scalar fallbacks — active when SIMD isn't built in (default Go
// build, or on unsupported architectures). Signatures MUST match
// series_ops_simd.go exactly so callers get identical behavior —
// only throughput changes.

package gobi

import "github.com/zoobst/gobi/compute"

// simdEnabled indicates whether the SIMD path is compiled in.
// False on this fallback file; the arm64/amd64 counterpart in
// series_ops_simd.go sets it to true.
const simdEnabled = false

func addF64Kernel(out, a, b []float64) {
	for i := range out {
		out[i] = a[i] + b[i]
	}
}

func subF64Kernel(out, a, b []float64) {
	for i := range out {
		out[i] = a[i] - b[i]
	}
}

func mulF64Kernel(out, a, b []float64) {
	for i := range out {
		out[i] = a[i] * b[i]
	}
}

func divF64Kernel(out, a, b []float64) {
	for i := range out {
		out[i] = a[i] / b[i]
	}
}

func addScalarF64Kernel(out, a []float64, v float64) {
	for i := range out {
		out[i] = a[i] + v
	}
}

func mulScalarF64Kernel(out, a []float64, v float64) {
	for i := range out {
		out[i] = a[i] * v
	}
}

func sumF64Kernel(a []float64) float64 {
	// Delegate to the compute package's SumF64. On the default
	// build compute is scalar (identical inner loop to what this
	// used to be). On `GOEXPERIMENT=simd` (arm64 NEON, amd64
	// AVX2/AVX-512) it uses lane-parallel SIMD reduction with
	// horizontal reduce at the tail — measured ~2× on arm64 for
	// n≥1M. Non-simd builds see no regression.
	return compute.SumF64(a)
}

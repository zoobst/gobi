//go:build goexperiment.simd && (arm64 || amd64)

// SIMD-vectorized elementwise Float64 kernels. Active only when
// built with `GOEXPERIMENT=simd` on arm64 (NEON) or amd64
// (AVX2/AVX-512), using the stdlib `simd` package (still an
// experiment in Go 1.27). The scalar fallback for other builds
// lives in series_ops_simd_fallback.go.
//
// Lane count is queried at runtime (2 on NEON, 4 on AVX2, 8 on
// AVX-512) rather than hard-coded. Kernels process laneCount
// float64s per iteration via LoadFloat64s + arithmetic + Store,
// then finish any remainder with scalar code.

package gobi

import (
	"simd"

	"github.com/zoobst/gobi/compute"
)

// simdEnabled indicates whether the SIMD path is compiled into
// this binary. True for arm64 and amd64 on Go 1.27+; the fallback
// file sets it to false everywhere else.
const simdEnabled = true

func addF64Kernel(out, a, b []float64) {
	lane := simd.BroadcastFloat64s(0).Len()
	n := len(out)
	i := 0
	for ; i+lane <= n; i += lane {
		va := simd.LoadFloat64s(a[i:])
		vb := simd.LoadFloat64s(b[i:])
		va.Add(vb).Store(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] + b[i]
	}
}

func subF64Kernel(out, a, b []float64) {
	lane := simd.BroadcastFloat64s(0).Len()
	n := len(out)
	i := 0
	for ; i+lane <= n; i += lane {
		va := simd.LoadFloat64s(a[i:])
		vb := simd.LoadFloat64s(b[i:])
		va.Sub(vb).Store(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] - b[i]
	}
}

func mulF64Kernel(out, a, b []float64) {
	lane := simd.BroadcastFloat64s(0).Len()
	n := len(out)
	i := 0
	for ; i+lane <= n; i += lane {
		va := simd.LoadFloat64s(a[i:])
		vb := simd.LoadFloat64s(b[i:])
		va.Mul(vb).Store(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] * b[i]
	}
}

func divF64Kernel(out, a, b []float64) {
	lane := simd.BroadcastFloat64s(0).Len()
	n := len(out)
	i := 0
	for ; i+lane <= n; i += lane {
		va := simd.LoadFloat64s(a[i:])
		vb := simd.LoadFloat64s(b[i:])
		va.Div(vb).Store(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] / b[i]
	}
}

func addScalarF64Kernel(out, a []float64, v float64) {
	vv := simd.BroadcastFloat64s(v)
	lane := vv.Len()
	n := len(out)
	i := 0
	for ; i+lane <= n; i += lane {
		va := simd.LoadFloat64s(a[i:])
		va.Add(vv).Store(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] + v
	}
}

func mulScalarF64Kernel(out, a []float64, v float64) {
	vv := simd.BroadcastFloat64s(v)
	lane := vv.Len()
	n := len(out)
	i := 0
	for ; i+lane <= n; i += lane {
		va := simd.LoadFloat64s(a[i:])
		va.Mul(vv).Store(out[i:])
	}
	for ; i < n; i++ {
		out[i] = a[i] * v
	}
}

// sumF64Kernel delegates to compute.SumF64, which carries a
// dedicated SIMD reduction with lane-parallel accumulators and a
// horizontal reduce at the tail. Kept as a thin wrapper here to
// preserve the local symbol for callers in series_ops.go.
func sumF64Kernel(a []float64) float64 {
	return compute.SumF64(a)
}

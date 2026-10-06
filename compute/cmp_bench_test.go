package compute

import (
	"math/rand/v2"
	"testing"
)

// BenchmarkCmpI64Ge_1M — Int64 compare on 1M rows. Scalar in both
// builds since the SIMD body was dropped (see cmp_basic.go); kept as
// the bandwidth reference for the fused kernels below.
func BenchmarkCmpI64Ge_1M(b *testing.B) {
	const n = 1_000_000
	a := make([]int64, n)
	rng := rand.New(rand.NewPCG(11, 22))
	for i := range a {
		a[i] = int64(rng.Uint64()) % 1000
	}
	out := make([]bool, n)
	b.ReportAllocs()
	for b.Loop() {
		CmpI64Ge(a, 500, out)
	}
}

// BenchmarkCmpF64Ge_1M — reference F64 for delta comparison.
func BenchmarkCmpF64Ge_1M(b *testing.B) {
	const n = 1_000_000
	a := make([]float64, n)
	rng := rand.New(rand.NewPCG(11, 22))
	for i := range a {
		a[i] = rng.Float64() * 1000
	}
	out := make([]bool, n)
	b.ReportAllocs()
	for b.Loop() {
		CmpF64Ge(a, 500, out)
	}
}

// BenchmarkCountTrue_1M — Slice 23c bool-reduce on 1M rows.
// Baseline for compiler auto-vectorization of the byte-sum
// loop; a hand-tuned SIMD popcount would land here if
// measurement justifies it.
func BenchmarkCountTrue_1M(b *testing.B) {
	const n = 1_000_000
	a := make([]bool, n)
	rng := rand.New(rand.NewPCG(33, 44))
	for i := range a {
		a[i] = rng.Float64() < 0.3
	}
	b.ReportAllocs()
	var sink int
	for b.Loop() {
		sink += CountTrue(a)
	}
	_ = sink
}

// fusedBenchInput: 1M uniform values in [0, 1000).
func fusedBenchInput(seed uint64) []float64 {
	a := make([]float64, 1_000_000)
	rng := rand.New(rand.NewPCG(seed, seed+1))
	for i := range a {
		a[i] = rng.Float64() * 1000
	}
	return a
}

// BenchmarkAndChainF64Range_1M — fused two-sided range compare.
// Scalar in every build (its SIMD body lost 29% on AVX2 against the
// branchless loop); kept as a reference for AndChainF64BBox.
func BenchmarkAndChainF64Range_1M(b *testing.B) {
	a := fusedBenchInput(55)
	out := make([]bool, len(a))
	b.ReportAllocs()
	for b.Loop() {
		AndChainF64Range(a, 250, 750, out)
	}
}

// BenchmarkAndChainF64BBox_1M — fused four-compare bbox filter. The
// one fused kernel with a SIMD body; compare the scalar and
// GOEXPERIMENT=simd builds to keep checking it still wins.
func BenchmarkAndChainF64BBox_1M(b *testing.B) {
	xs, ys := fusedBenchInput(66), fusedBenchInput(77)
	out := make([]bool, len(xs))
	b.ReportAllocs()
	for b.Loop() {
		AndChainF64BBox(xs, 250, 750, ys, 100, 900, out)
	}
}

// BenchmarkWithinSqDistF64_1M — fused equirectangular radius test.
// Scalar in every build (its SIMD body lost 14% on AVX2).
func BenchmarkWithinSqDistF64_1M(b *testing.B) {
	lats, lons := fusedBenchInput(88), fusedBenchInput(99)
	for i := range lats {
		lats[i] = lats[i]/1000*180 - 90
		lons[i] = lons[i]/1000*360 - 180
	}
	out := make([]bool, len(lats))
	b.ReportAllocs()
	for b.Loop() {
		WithinSqDistF64(lats, lons, 40, -75, 0.766, 25, out)
	}
}

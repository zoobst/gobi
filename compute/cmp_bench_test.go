package compute

import (
	"math/rand/v2"
	"testing"
)

// BenchmarkCmpI64Ge_1M — Slice 23a Int64 SIMD compare on 1M rows.
// Delta between scalar and SIMD builds is the vectorization win.
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

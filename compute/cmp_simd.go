//go:build goexperiment.simd && (arm64 || amd64)

// SIMD-vectorized fused bbox comparison: AndChainF64BBox. Active
// only when built with `GOEXPERIMENT=simd` on arm64 or amd64; the
// signature must match the scalar fallback in cmp_scalar.go. Every
// other compare kernel is scalar in all builds (cmp_basic.go has the
// measurements).
//
// Per-lane store into out[] goes through a per-lane int64 scratch
// buffer + a downconvert loop — the portable `simd` package has no
// direct mask→[]bool store. On amd64 that round trip is a
// store-to-load forwarding stall per lane group. With four compares
// per store the bbox kernel still beat the branchless scalar loop
// (Ryzen 7 5800X, AVX2, 1M rows: 921 vs 1,057 µs); the one- and
// two-compare kernels didn't, and were routed to scalar.
//
// # Testability
//
// Each public entry point is thin: eligibility gate + dispatch to a
// scalar fallback or a SIMD-body function. The SIMD bodies are
// unexported but callable from _test.go, so parity tests can force
// the vector kernel on 2-lane NEON where the eligibility gate would
// otherwise reroute to scalar.

package compute

import "simd"

const simdEnabled = true

// cmpKernelSIMDEligible reports whether the per-lane scratch-store
// + bool-tail-loop overhead of the SIMD compare kernels is worth
// paying at the current lane width. Measured on Apple M3 (arm64
// 2-lane NEON): the SIMD kernels are ~3× SLOWER than the
// compiler-auto-vectorized scalar range loop because the
// Masked→Store→per-lane-bool conversion at each lane group is
// bigger than a tight `for i, v := range a { out[i] = v OP b }`
// loop that the Go compiler vectorizes cleanly on M3. Gate the
// SIMD dispatch to lane ≥ 4 (amd64 AVX2 / AVX-512), same shape
// as the Slice-8 PIP SIMD gate.
//
// Under this gate, the Slice 22a / 23b wire-ins in gobi/series_ops.go
// stay correct: on 2-lane hardware they get the scalar loop. On
// 4/8-lane hardware the fused kernels take the SIMD body. "Lane ≥ 4
// wins" turned out false for the single compares on AVX2 (Ryzen 7
// 5800X: 1.77× slower), so treat it as unproven for these too until
// the fused benchmarks are measured there.
func cmpKernelSIMDEligible() bool {
	return simd.BroadcastFloat64s(0).Len() >= 4
}

// AndChainF64BBox writes
//
//	out[i] = (aLo <= a[i] <= aHi) && (bLo <= b[i] <= bHi)
//
// in a single pass. Four SIMD compares + three SIMD ANDs + one
// store per lane group. Preserves the "no intermediate boolean
// buffers" property the callers on the fused-filter path rely on
// — critical for staying memory-bandwidth-efficient.
func AndChainF64BBox(a []float64, aLo, aHi float64, b []float64, bLo, bHi float64, out []bool) {
	if len(a) != len(b) {
		panic("compute: AndChainF64BBox: a and b length mismatch")
	}
	if !cmpKernelSIMDEligible() {
		andChainF64BBoxScalar(a, aLo, aHi, b, bLo, bHi, out)
		return
	}
	andChainF64BBoxSIMDBody(a, aLo, aHi, b, bLo, bHi, out)
}

func andChainF64BBoxSIMDBody(a []float64, aLo, aHi float64, b []float64, bLo, bHi float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vaLo := simd.BroadcastFloat64s(aLo)
	vaHi := simd.BroadcastFloat64s(aHi)
	vbLo := simd.BroadcastFloat64s(bLo)
	vbHi := simd.BroadcastFloat64s(bHi)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vaLo.Len()
	// Stack-allocated scratch, sized to the max supported lane
	// count (8, AVX-512 Int64s). See cmpF64GeSIMDBody comment.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadFloat64s(a[i:])
		vb := simd.LoadFloat64s(b[i:])
		var mask simd.Mask64s
		mask = va.GreaterEqual(vaLo)
		mask = mask.And(va.LessEqual(vaHi))
		mask = mask.And(vb.GreaterEqual(vbLo))
		mask = mask.And(vb.LessEqual(vbHi))
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = aLo <= a[i] && a[i] <= aHi && bLo <= b[i] && b[i] <= bHi
	}
}

// CmpI64Ge / Le / Gt / Lt — SIMD variants of the Int64 scalar-vs-
// column comparisons in cmp_scalar.go. Same lane-parallel compare
// + mask-store shape as the Float64 variants; simd.Int64s ships
// all four order comparisons natively.
//
// Reference benchmark shape (arm64 NEON 2-lane): compare kernel
// throughput sits between the F64 versions and the reduce ops —
// integer compare is cheaper per op than float compare (no
// denormals / NaN handling) but the mask-to-bool tail loop is
// the same, so the observed win vs scalar is comparable
// (~3-4× on 100k+ rows).

// CountTrue shares the scalar-body implementation with the
// !simd build. Go's compiler auto-vectorizes the tight
// `if v { n++ }` loop cleanly on both arm64 and amd64; an
// explicit simd.Uint8s.Sum path adds header + tail complexity
// without a measurable improvement on realistic mask sizes.
// Kept in cmp_simd.go so both builds compile — if a benchmark
// ever justifies it, this is where a hand-written SIMD popcount
// would land.
func CountTrue(a []bool) int {
	var n int
	for _, v := range a {
		if v {
			n++
		}
	}
	return n
}

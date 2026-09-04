//go:build goexperiment.simd && (arm64 || amd64)

// SIMD-vectorized comparison kernels. Active only when built with
// `GOEXPERIMENT=simd` on arm64 or amd64. Signatures must match the
// scalar fallbacks in cmp_scalar.go exactly.
//
// Per-lane store into out[] goes through a per-lane int64 scratch
// buffer + a downconvert loop — the stdlib `simd` package does not
// yet expose a direct mask→[]bool store. That trade costs a small
// tail loop per SIMD lane group but keeps the compare + AND phases
// on the vectorized fast path, which is where the real win lives.
//
// # Testability
//
// Each public entry point (CmpF64Ge, AndChainF64BBox, WithinSqDistF64,
// …) is thin: eligibility gate + dispatch to a scalar fallback or a
// SIMD-body function. The SIMD bodies are unexported but callable
// from _test.go — so parity tests can force the vector kernel on
// 2-lane NEON where the eligibility gate would otherwise reroute to
// scalar. Matches the pipCrossingCountSIMDBody pattern in geom_simd.go.
//
// Benchmark reference (arm64, NEON, 2M rows):
//
//	scalar CmpF64Ge:              ~9 ms
//	SIMD   CmpF64Ge:              ~2 ms   (~4.5×)
//	scalar AndChainF64Range:      ~12 ms
//	SIMD   AndChainF64Range:      ~2 ms   (~5.6×)

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
// stay correct: on 2-lane hardware they get the scalar loop
// (fast); on 4/8-lane hardware they get the SIMD kernel (also
// expected fast, though unmeasured on this Apple M3 dev machine).
func cmpKernelSIMDEligible() bool {
	return simd.BroadcastFloat64s(0).Len() >= 4
}

// CmpF64Ge writes out[i] = a[i] >= b.
func CmpF64Ge(a []float64, b float64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v >= b
		}
		return
	}
	cmpF64GeSIMDBody(a, b, out)
}

// cmpF64GeSIMDBody is the ungated SIMD kernel. Callable from tests so
// the vector body is exercised on 2-lane hardware where CmpF64Ge would
// otherwise take the scalar path.
func cmpF64GeSIMDBody(a []float64, b float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	// Query lane count from a broadcast, not from the input slice —
	// LoadFloat64s panics if len(a) < laneCount (very-short-input
	// case). Broadcast has no such requirement.
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadFloat64s(a[i:])
		mask := va.GreaterEqual(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	// Tail
	for ; i < len(a); i++ {
		out[i] = a[i] >= b
	}
}

// CmpF64Le writes out[i] = a[i] <= b.
func CmpF64Le(a []float64, b float64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v <= b
		}
		return
	}
	cmpF64LeSIMDBody(a, b, out)
}

func cmpF64LeSIMDBody(a []float64, b float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadFloat64s(a[i:])
		mask := va.LessEqual(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] <= b
	}
}

// CmpF64Gt writes out[i] = a[i] > b.
func CmpF64Gt(a []float64, b float64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v > b
		}
		return
	}
	cmpF64GtSIMDBody(a, b, out)
}

func cmpF64GtSIMDBody(a []float64, b float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadFloat64s(a[i:])
		mask := va.Greater(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] > b
	}
}

// CmpF64Lt writes out[i] = a[i] < b.
func CmpF64Lt(a []float64, b float64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v < b
		}
		return
	}
	cmpF64LtSIMDBody(a, b, out)
}

func cmpF64LtSIMDBody(a []float64, b float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadFloat64s(a[i:])
		mask := va.Less(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] < b
	}
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
		for i, v := range a {
			out[i] = aLo <= v && v <= aHi && bLo <= b[i] && b[i] <= bHi
		}
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

// WithinSqDistF64 writes
//
//	out[i] = ((lats[i]-refLat)² + ((lons[i]-refLon)·cosRefLat)²) <= sqThreshold
//
// Single-pass SIMD kernel: two subtractions, two multiplies (for
// squarings), one multiply (cosRefLat scaling of the longitude
// delta), one add, one compare. All Float64s ops execute at the
// full lane width — this is the workload shape where SIMD's
// ceiling is the highest for gobi's use cases.
func WithinSqDistF64(lats, lons []float64, refLat, refLon, cosRefLat, sqThreshold float64, out []bool) {
	if len(lats) != len(lons) {
		panic("compute: WithinSqDistF64: lats and lons length mismatch")
	}
	if !cmpKernelSIMDEligible() {
		for i := range lats {
			dLat := lats[i] - refLat
			dLon := (lons[i] - refLon) * cosRefLat
			out[i] = dLat*dLat+dLon*dLon <= sqThreshold
		}
		return
	}
	withinSqDistF64SIMDBody(lats, lons, refLat, refLon, cosRefLat, sqThreshold, out)
}

func withinSqDistF64SIMDBody(lats, lons []float64, refLat, refLon, cosRefLat, sqThreshold float64, out []bool) {
	if len(lats) == 0 {
		return
	}
	vRefLat := simd.BroadcastFloat64s(refLat)
	vRefLon := simd.BroadcastFloat64s(refLon)
	vCosRef := simd.BroadcastFloat64s(cosRefLat)
	vSqThr := simd.BroadcastFloat64s(sqThreshold)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vRefLat.Len()
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(lats); i += laneCount {
		vlat := simd.LoadFloat64s(lats[i:])
		vlon := simd.LoadFloat64s(lons[i:])
		dLat := vlat.Sub(vRefLat)
		dLon := vlon.Sub(vRefLon).Mul(vCosRef)
		sq := dLat.Mul(dLat).Add(dLon.Mul(dLon))
		mask := sq.LessEqual(vSqThr)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	// Tail
	for ; i < len(lats); i++ {
		dLat := lats[i] - refLat
		dLon := (lons[i] - refLon) * cosRefLat
		out[i] = dLat*dLat+dLon*dLon <= sqThreshold
	}
}

// AndChainF64Range writes out[i] = (lo <= a[i]) && (a[i] <= hi).
// Fused two-sided range check — a single vector load per lane
// group is compared against BOTH bounds, and the two masks are
// ANDed in SIMD before the store. Two compares + one AND +
// one store per lane group.
func AndChainF64Range(a []float64, lo, hi float64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = lo <= v && v <= hi
		}
		return
	}
	andChainF64RangeSIMDBody(a, lo, hi, out)
}

func andChainF64RangeSIMDBody(a []float64, lo, hi float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vLo := simd.BroadcastFloat64s(lo)
	vHi := simd.BroadcastFloat64s(hi)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vLo.Len()
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]

	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadFloat64s(a[i:])
		var mask simd.Mask64s
		mask = va.GreaterEqual(vLo)
		mask = mask.And(va.LessEqual(vHi))
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = lo <= a[i] && a[i] <= hi
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

func CmpI64Ge(a []int64, b int64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v >= b
		}
		return
	}
	cmpI64GeSIMDBody(a, b, out)
}

func cmpI64GeSIMDBody(a []int64, b int64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastInt64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]
	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadInt64s(a[i:])
		mask := va.GreaterEqual(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] >= b
	}
}

func CmpI64Le(a []int64, b int64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v <= b
		}
		return
	}
	cmpI64LeSIMDBody(a, b, out)
}

func cmpI64LeSIMDBody(a []int64, b int64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastInt64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]
	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadInt64s(a[i:])
		mask := va.LessEqual(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] <= b
	}
}

func CmpI64Gt(a []int64, b int64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v > b
		}
		return
	}
	cmpI64GtSIMDBody(a, b, out)
}

func cmpI64GtSIMDBody(a []int64, b int64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastInt64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]
	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadInt64s(a[i:])
		mask := va.Greater(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] > b
	}
}

func CmpI64Lt(a []int64, b int64, out []bool) {
	if !cmpKernelSIMDEligible() {
		for i, v := range a {
			out[i] = v < b
		}
		return
	}
	cmpI64LtSIMDBody(a, b, out)
}

func cmpI64LtSIMDBody(a []int64, b int64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastInt64s(b)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vb.Len()
	// Stack-allocated scratch — max SIMD lane count on any
	// current target is 8 (AVX-512 Int64s), so a [8]int64
	// bounds the maximum. Slicing to laneCount is safe because
	// Store only writes the first laneCount entries.
	var scratchArr [8]int64
	scratch := scratchArr[:laneCount]
	i := 0
	for ; i+laneCount <= len(a); i += laneCount {
		va := simd.LoadInt64s(a[i:])
		mask := va.Less(vb)
		vOnes.Masked(mask).Store(scratch)
		for j := range laneCount {
			out[i+j] = scratch[j] != 0
		}
	}
	for ; i < len(a); i++ {
		out[i] = a[i] < b
	}
}

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

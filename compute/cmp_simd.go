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
// Benchmark reference (arm64, NEON, 2M rows):
//
//	scalar CmpF64Ge:              ~9 ms
//	SIMD   CmpF64Ge:              ~2 ms   (~4.5×)
//	scalar AndChainF64Range:      ~12 ms
//	SIMD   AndChainF64Range:      ~2 ms   (~5.6×)

package compute

import "simd"

const simdEnabled = true

// CmpF64Ge writes out[i] = a[i] >= b.
func CmpF64Ge(a []float64, b float64, out []bool) {
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	// Query lane count from a broadcast, not from the input slice —
	// LoadFloat64s panics if len(a) < laneCount (very-short-input
	// case). Broadcast has no such requirement.
	laneCount := vb.Len()
	scratch := make([]int64, laneCount)

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
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	// Query lane count from a broadcast, not from the input slice —
	// LoadFloat64s panics if len(a) < laneCount (very-short-input
	// case). Broadcast has no such requirement.
	laneCount := vb.Len()
	scratch := make([]int64, laneCount)

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
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	// Query lane count from a broadcast, not from the input slice —
	// LoadFloat64s panics if len(a) < laneCount (very-short-input
	// case). Broadcast has no such requirement.
	laneCount := vb.Len()
	scratch := make([]int64, laneCount)

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
	if len(a) == 0 {
		return
	}
	vb := simd.BroadcastFloat64s(b)
	vOnes := simd.BroadcastInt64s(1)
	// Query lane count from a broadcast, not from the input slice —
	// LoadFloat64s panics if len(a) < laneCount (very-short-input
	// case). Broadcast has no such requirement.
	laneCount := vb.Len()
	scratch := make([]int64, laneCount)

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
	if len(a) == 0 {
		return
	}
	if len(a) != len(b) {
		panic("compute: AndChainF64BBox: a and b length mismatch")
	}
	vaLo := simd.BroadcastFloat64s(aLo)
	vaHi := simd.BroadcastFloat64s(aHi)
	vbLo := simd.BroadcastFloat64s(bLo)
	vbHi := simd.BroadcastFloat64s(bHi)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vaLo.Len()
	scratch := make([]int64, laneCount)

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
	if len(lats) == 0 {
		return
	}
	if len(lats) != len(lons) {
		panic("compute: WithinSqDistF64: lats and lons length mismatch")
	}
	vRefLat := simd.BroadcastFloat64s(refLat)
	vRefLon := simd.BroadcastFloat64s(refLon)
	vCosRef := simd.BroadcastFloat64s(cosRefLat)
	vSqThr := simd.BroadcastFloat64s(sqThreshold)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vRefLat.Len()
	scratch := make([]int64, laneCount)

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
	if len(a) == 0 {
		return
	}
	vLo := simd.BroadcastFloat64s(lo)
	vHi := simd.BroadcastFloat64s(hi)
	vOnes := simd.BroadcastInt64s(1)
	laneCount := vLo.Len()
	scratch := make([]int64, laneCount)

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

// Single-compare kernels, shared by the scalar and SIMD builds.
//
// These were vectorized in the SIMD build until amd64 measurement
// (Ryzen 7 5800X, AVX2, 4 lanes) showed the vector bodies 1.77×
// SLOWER than this loop at 1M rows: storing each 4-lane mask to a
// scratch buffer and reading it back as bools hits a store-to-load
// forwarding stall on every iteration. The plain loop already runs
// near memory bandwidth (~21 GB/s there), so a vector body has
// little to gain even without the stall. On 2-lane NEON the vector
// bodies were already gated off for the same reason.

package compute

// CmpF64Ge writes out[i] = a[i] >= b for i in [0, len(a)). out
// must have len >= len(a); extra tail is left untouched. Callers
// are expected to pre-allocate out sized to len(a).
func CmpF64Ge(a []float64, b float64, out []bool) {
	for i, v := range a {
		out[i] = v >= b
	}
}

// CmpF64Le writes out[i] = a[i] <= b.
func CmpF64Le(a []float64, b float64, out []bool) {
	for i, v := range a {
		out[i] = v <= b
	}
}

// CmpF64Gt writes out[i] = a[i] > b.
func CmpF64Gt(a []float64, b float64, out []bool) {
	for i, v := range a {
		out[i] = v > b
	}
}

// CmpF64Lt writes out[i] = a[i] < b.
func CmpF64Lt(a []float64, b float64, out []bool) {
	for i, v := range a {
		out[i] = v < b
	}
}

// CmpI64Ge writes out[i] = a[i] >= b. Same shape as CmpF64Ge but
// on int64 columns. Signatures match cmp_simd.go — SIMD build
// vectorizes the compare via simd.Int64s.GreaterEqual + mask store.
func CmpI64Ge(a []int64, b int64, out []bool) {
	for i, v := range a {
		out[i] = v >= b
	}
}

// CmpI64Le writes out[i] = a[i] <= b.
func CmpI64Le(a []int64, b int64, out []bool) {
	for i, v := range a {
		out[i] = v <= b
	}
}

// CmpI64Gt writes out[i] = a[i] > b.
func CmpI64Gt(a []int64, b int64, out []bool) {
	for i, v := range a {
		out[i] = v > b
	}
}

// CmpI64Lt writes out[i] = a[i] < b.
func CmpI64Lt(a []int64, b int64, out []bool) {
	for i, v := range a {
		out[i] = v < b
	}
}

// b2u converts a comparison result to 0 / 1. The compiler emits a
// flag-to-register instruction (SETcc on amd64, CSET on arm64), not a
// branch, which is what keeps the fused range kernels below branchless.
func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// andChainF64RangeScalar is the scalar body of AndChainF64Range for
// both builds. The comparisons are combined with a bitwise AND of
// 0 / 1 bytes rather than `&&`: Go's `&&` short-circuits, which
// compiled to a branch per element that random data mispredicts about
// half the time. Measured at 1M random rows: 2.38 → 0.49 ms on arm64,
// ~3× on amd64; on sorted (predictable) input it is still no slower.
// NaN compares false either way, so NaN rows stay out.
func andChainF64RangeScalar(a []float64, lo, hi float64, out []bool) {
	out = out[:len(a)]
	for i, v := range a {
		out[i] = b2u(lo <= v)&b2u(v <= hi) != 0
	}
}

// andChainF64BBoxScalar is the scalar body of AndChainF64BBox for
// both builds; branchless for the same reason as
// andChainF64RangeScalar (4.29 → 0.77 ms at 1M rows on arm64).
// Caller guarantees len(b) == len(a).
func andChainF64BBoxScalar(a []float64, aLo, aHi float64, b []float64, bLo, bHi float64, out []bool) {
	b = b[:len(a)]
	out = out[:len(a)]
	for i, x := range a {
		y := b[i]
		out[i] = b2u(aLo <= x)&b2u(x <= aHi)&b2u(bLo <= y)&b2u(y <= bHi) != 0
	}
}

// AndChainF64Range and WithinSqDistF64 are scalar in every build too.
// Against the branchless scalar loops their SIMD bodies lost on AVX2
// (Ryzen 7 5800X, 1M rows): 29% and 14% slower — one or two compares
// per mask store doesn't recover the store-and-read-back cost.
// AndChainF64BBox (four compares per store) kept its SIMD body.

// AndChainF64Range writes out[i] = (lo <= a[i]) && (a[i] <= hi).
// Fused two-sided range check — the shape most bbox filters
// produce. Branchless; see andChainF64RangeScalar.
func AndChainF64Range(a []float64, lo, hi float64, out []bool) {
	andChainF64RangeScalar(a, lo, hi, out)
}

// WithinSqDistF64 writes
//
//	out[i] = ((lats[i]-refLat)² + ((lons[i]-refLon)·cosRefLat)²) <= sqThreshold
//
// — the equirectangular-approximation "point within radius r of
// (refLat, refLon)" filter. Compute-heavy shape: 2 subtractions
// + 2 squarings + 1 multiply-by-scaling + 1 add + 1 compare per
// row, all fully vectorizable. Callers precompute cosRefLat once
// (avoids per-row trig) and pass the squared threshold to save
// a sqrt in the inner loop.
//
// Accuracy: equirect approximation is fine for distances small
// relative to Earth's radius (< a few hundred km). For global
// distances use a proper haversine impl (not SIMD-friendly on the
// current simd.Float64s surface — no atan2/trig).
//
// lats and lons must have equal length; out must have len >= len(lats).
func WithinSqDistF64(lats, lons []float64, refLat, refLon, cosRefLat, sqThreshold float64, out []bool) {
	if len(lats) != len(lons) {
		panic("compute: WithinSqDistF64: lats and lons length mismatch")
	}
	for i := range lats {
		dLat := lats[i] - refLat
		dLon := (lons[i] - refLon) * cosRefLat
		out[i] = dLat*dLat+dLon*dLon <= sqThreshold
	}
}

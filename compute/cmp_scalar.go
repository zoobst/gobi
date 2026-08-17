//go:build !goexperiment.simd || (!arm64 && !amd64)

// Scalar fallbacks — active when SIMD isn't built in (default Go
// build, or on unsupported architectures). Signatures MUST match
// cmp_simd.go exactly so callers get the same surface regardless
// of build.

package compute

const simdEnabled = false

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

// AndChainF64Range writes out[i] = (lo <= a[i]) && (a[i] <= hi).
// Fused two-sided range check — the shape most bbox filters
// produce. Short-circuits per element on the low side to skip
// the high compare on out-of-range rows.
func AndChainF64Range(a []float64, lo, hi float64, out []bool) {
	for i, v := range a {
		out[i] = lo <= v && v <= hi
	}
}

// AndChainF64BBox writes
//
//	out[i] = (aLo <= a[i] <= aHi) && (bLo <= b[i] <= bHi)
//
// in a single pass — the canonical 2D bbox filter shape. Fusing
// both columns' comparisons into one kernel avoids the
// intermediate []bool that a "per-column primitive + scalar AND"
// composition would allocate. Callers that just want a 1D range
// should use AndChainF64Range instead.
//
// a and b must have equal length; out must have len >= len(a).
// Panics on length mismatch (invariant guaranteed by callers
// operating on same-frame columns).
func AndChainF64BBox(a []float64, aLo, aHi float64, b []float64, bLo, bHi float64, out []bool) {
	if len(a) != len(b) {
		panic("compute: AndChainF64BBox: a and b length mismatch")
	}
	for i := range a {
		out[i] = aLo <= a[i] && a[i] <= aHi && bLo <= b[i] && b[i] <= bHi
	}
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

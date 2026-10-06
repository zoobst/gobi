//go:build !goexperiment.simd || (!arm64 && !amd64)

// Scalar fallbacks for the kernels that still have a SIMD variant
// (AndChainF64BBox, CountTrue) — active when SIMD isn't built in
// (default Go build, or on unsupported architectures). Signatures
// MUST match cmp_simd.go exactly. Every other compare kernel lives in
// cmp_basic.go, shared by both builds.

package compute

const simdEnabled = false

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
	andChainF64BBoxScalar(a, aLo, aHi, b, bLo, bHi, out)
}

// CountTrue returns the number of true entries in a. Foundational
// bool-reduce kernel used by filter mask sizing (`Frame.Filter`
// can allocate the keep-index slice at exact selectivity instead
// of over-allocating to full mask length) and any bool-column
// count/sum reduction.
//
// Scalar loop over `bool` values — Go's `bool` is a distinct type,
// not a byte to be truth-tested. The SIMD file (cmp_simd.go) shares
// this implementation today; if a benchmark ever justifies it, a
// hand-written popcount would go there.
func CountTrue(a []bool) int {
	var n int
	for _, v := range a {
		if v {
			n++
		}
	}
	return n
}

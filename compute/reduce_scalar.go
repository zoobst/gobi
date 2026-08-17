//go:build !goexperiment.simd || (!arm64 && !amd64)

// Scalar reduction kernels. Active on the default Go build and on
// architectures where the SIMD experiment isn't wired up. Same
// signatures as reduce_simd.go — callers get identical behavior,
// only the throughput changes.
//
// All reductions are null-oblivious: they operate on the concrete
// []T slice they're given. Callers pre-filter nulls (either by
// extracting a compact non-null slice OR by looping externally and
// only calling the kernel with a null-free view). This keeps the
// inner loop free of validity-bitmap branching — the shape the
// SIMD path benefits most from.

package compute

// SumF64 returns the sum of every element in a. Empty input
// returns 0.
func SumF64(a []float64) float64 {
	var s float64
	for _, v := range a {
		s += v
	}
	return s
}

// SumI64 returns the sum of every element in a as int64. Overflow
// wraps (matches Go's built-in `+` semantics). Empty input returns 0.
func SumI64(a []int64) int64 {
	var s int64
	for _, v := range a {
		s += v
	}
	return s
}

// MinF64 returns (min, true) when a is non-empty, else (0, false).
// NaN semantics match Go's built-in `<`: NaN comparisons return
// false, so a first-seen NaN sticks unless a later non-NaN value
// is strictly less. Empty groups return (0, false).
func MinF64(a []float64) (float64, bool) {
	if len(a) == 0 {
		return 0, false
	}
	m := a[0]
	for _, v := range a[1:] {
		if v < m {
			m = v
		}
	}
	return m, true
}

// MaxF64 returns (max, true) when a is non-empty, else (0, false).
func MaxF64(a []float64) (float64, bool) {
	if len(a) == 0 {
		return 0, false
	}
	m := a[0]
	for _, v := range a[1:] {
		if v > m {
			m = v
		}
	}
	return m, true
}

// MinI64 returns (min, true) when a is non-empty, else (0, false).
func MinI64(a []int64) (int64, bool) {
	if len(a) == 0 {
		return 0, false
	}
	m := a[0]
	for _, v := range a[1:] {
		if v < m {
			m = v
		}
	}
	return m, true
}

// MaxI64 returns (max, true) when a is non-empty, else (0, false).
func MaxI64(a []int64) (int64, bool) {
	if len(a) == 0 {
		return 0, false
	}
	m := a[0]
	for _, v := range a[1:] {
		if v > m {
			m = v
		}
	}
	return m, true
}

//go:build goexperiment.simd && (arm64 || amd64)

// SIMD-vectorized reduction kernels. Every reduce is a lane-parallel
// accumulator loop (Add across current + running vector, or
// Min/Max) followed by a horizontal reduce at the tail. Matches
// what Polars' arrow2 reductions do internally on their equivalent
// SIMD path.
//
// Only Float64 has direct SIMD Min/Max in the stdlib `simd`
// package as of Go 1.27. Int64 has Add (for Sum) but no direct
// Min/Max — those fall to scalar in this build too.
//
// Benchmark reference (arm64, NEON, 10M rows, Apple M3 Pro):
//
//	scalar SumF64:    ~6.3 ms   ( 1.58 Grows/s)
//	SIMD   SumF64:    ~1.6 ms   ( 6.31 Grows/s)  ~4× win
//	scalar MinF64:    ~7.0 ms   ( 1.42 Grows/s)
//	SIMD   MinF64:    ~1.7 ms   ( 5.90 Grows/s)  ~4× win
//
// See `benchmarks/simd/reduce/main.go` for the full harness.

package compute

import "simd"

// SumF64 returns the sum of every element in a. Empty input
// returns 0. SIMD-vectorized: lane-parallel add into a running
// accumulator vector, horizontal reduce at the tail.
func SumF64(a []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	// Query lane count from a broadcast — Load on a shorter-than-
	// lane slice would panic, and we want SumF64([]float64{1,2,3})
	// to work on 2-lane NEON.
	laneCount := simd.BroadcastFloat64s(0).Len()
	scratch := make([]float64, laneCount)

	if len(a) < laneCount {
		var s float64
		for _, v := range a {
			s += v
		}
		return s
	}

	acc := simd.LoadFloat64s(a)
	i := laneCount
	for ; i+laneCount <= len(a); i += laneCount {
		acc = acc.Add(simd.LoadFloat64s(a[i:]))
	}
	acc.Store(scratch)
	var s float64
	for _, x := range scratch {
		s += x
	}
	// Tail
	for ; i < len(a); i++ {
		s += a[i]
	}
	return s
}

// SumI64 returns the sum of every element in a as int64. Overflow
// wraps. Empty input returns 0. Same lane-parallel Add + horizontal
// reduce pattern as SumF64.
func SumI64(a []int64) int64 {
	if len(a) == 0 {
		return 0
	}
	laneCount := simd.BroadcastInt64s(0).Len()
	scratch := make([]int64, laneCount)

	if len(a) < laneCount {
		var s int64
		for _, v := range a {
			s += v
		}
		return s
	}

	acc := simd.LoadInt64s(a)
	i := laneCount
	for ; i+laneCount <= len(a); i += laneCount {
		acc = acc.Add(simd.LoadInt64s(a[i:]))
	}
	acc.Store(scratch)
	var s int64
	for _, x := range scratch {
		s += x
	}
	for ; i < len(a); i++ {
		s += a[i]
	}
	return s
}

// MinF64 returns (min, true) when a is non-empty, else (0, false).
// SIMD-vectorized via Float64s.Min. NaN handling matches the
// scalar path: NaN comparisons return false so a first-seen NaN
// sticks unless a later non-NaN value is strictly less.
func MinF64(a []float64) (float64, bool) {
	if len(a) == 0 {
		return 0, false
	}
	laneCount := simd.BroadcastFloat64s(0).Len()
	if len(a) < laneCount {
		m := a[0]
		for _, v := range a[1:] {
			if v < m {
				m = v
			}
		}
		return m, true
	}

	scratch := make([]float64, laneCount)
	acc := simd.LoadFloat64s(a)
	i := laneCount
	for ; i+laneCount <= len(a); i += laneCount {
		acc = acc.Min(simd.LoadFloat64s(a[i:]))
	}
	acc.Store(scratch)
	m := scratch[0]
	for _, x := range scratch[1:] {
		if x < m {
			m = x
		}
	}
	for ; i < len(a); i++ {
		if a[i] < m {
			m = a[i]
		}
	}
	return m, true
}

// MaxF64 returns (max, true) when a is non-empty, else (0, false).
func MaxF64(a []float64) (float64, bool) {
	if len(a) == 0 {
		return 0, false
	}
	laneCount := simd.BroadcastFloat64s(0).Len()
	if len(a) < laneCount {
		m := a[0]
		for _, v := range a[1:] {
			if v > m {
				m = v
			}
		}
		return m, true
	}

	scratch := make([]float64, laneCount)
	acc := simd.LoadFloat64s(a)
	i := laneCount
	for ; i+laneCount <= len(a); i += laneCount {
		acc = acc.Max(simd.LoadFloat64s(a[i:]))
	}
	acc.Store(scratch)
	m := scratch[0]
	for _, x := range scratch[1:] {
		if x > m {
			m = x
		}
	}
	for ; i < len(a); i++ {
		if a[i] > m {
			m = a[i]
		}
	}
	return m, true
}

// MinI64 returns (min, true) when a is non-empty, else (0, false).
// Scalar even in the SIMD build — Int64s doesn't expose Min/Max
// directly in Go 1.27's simd package. Could be written via IfElse
// + Less-mask but the branch-free win is smaller than for Float64
// and the code is harder to read; keep scalar until measured
// otherwise.
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
// Scalar for the same reason as MinI64.
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

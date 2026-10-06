//go:build goexperiment.simd && (arm64 || amd64)

// SIMD-vectorized geometry kernels. Active only when built with
// `GOEXPERIMENT=simd` on arm64 or amd64. Signatures + semantics
// must match the scalar fallbacks in geom_scalar.go exactly —
// callers (geometry.BoundsFromXY, polygonRingCentroid, PIPRingFromXY)
// don't branch on build tags.
//
// # Kernels
//
// - BoundsF64: lane-parallel min/max reduce on (Xs, Ys) — recycles
//   the MinF64/MaxF64 shape shipped in reduce_simd.go.
// - PolygonCentroidShoelace: lane-parallel `cross = x0*y1 - x1*y0`
//   + per-lane accumulator vectors for areaTwo, cx, cy. Loads two
//   staggered coordinate windows per iter (xs[i:] and xs[i+1:]).
// - PIPCrossingCount: reformulated crossing-count kernel — breaks
//   the scalar `inside = !inside` dependency by tracking a
//   running lane-parallel count, then horizontal-reduces to
//   parity at the tail.
//
// # Design notes
//
// The lane count is queried at runtime (2 on arm64 NEON, 4 on
// amd64 AVX2, 8 on AVX-512). Kernels fall back to scalar when
// n < lane_count to avoid the SIMD setup + horizontal reduce
// overhead dominating on tiny inputs — the same shape the
// reduction kernels use.
//
// Tail handling: any coordinates past the last aligned lane group
// run through a compact scalar tail loop. On typical geometry
// shapes (5-vertex bboxes to 64K-vertex coastlines) the tail is
// at most (lane_count - 1) segments, negligible next to the
// vectorized body.

package compute

import (
	"math"
	"simd"
)

// BoundsF64 — lane-parallel min/max reduce on parallel Xs/Ys.
// Matches the scalar signature; scalar tail handles the last
// (n mod lane_count) coordinates.
//
// # Arch gate
//
// Slice-6a reverted the BoundsFromXY wire-in after Ampere / M3
// regressions traced to the SIMD setup + horizontal-reduce
// overhead dominating on 2-lane NEON. Gated to lane ≥ 4
// (amd64 AVX2 / AVX-512) to match the PIP kernel's Slice-8 gate.
// 2-lane callers get the compiler-auto-vectorized scalar reduce,
// which measures at parity or faster on M3.
func BoundsF64(xs, ys []float64) (minX, minY, maxX, maxY float64, ok bool) {
	if len(xs) == 0 || len(ys) == 0 {
		return 0, 0, 0, 0, false
	}
	n := min(len(ys), len(xs))
	if n < boundsSIMDMinSize {
		// Checked before the lane query so small inputs pay nothing
		// for the SIMD build.
		minX, minY, maxX, maxY = boundsF64Scalar(xs, ys, n)
		return minX, minY, maxX, maxY, true
	}
	lane := simd.BroadcastFloat64s(0).Len()
	if lane < 4 || lane > maxSIMDLanes {
		minX, minY, maxX, maxY = boundsF64Scalar(xs, ys, n)
		return minX, minY, maxX, maxY, true
	}
	return boundsF64SIMDBody(xs, ys, n, lane)
}

// centroidSIMDMinSize is the ring size below which
// PolygonCentroidShoelace stays scalar (AVX2 crossover is between 1K
// and 64K points; see the note in PolygonCentroidShoelace).
const centroidSIMDMinSize = 8192

// boundsSIMDMinSize is the input size below which BoundsF64 stays
// scalar. The vector body has a fixed cost (spilling and reducing six
// accumulators); on AVX2 (Ryzen 7 5800X) the scalar loop won outright
// at 64 points and SIMD won 2.8× at 1K and 4.6× from 64K up.
const boundsSIMDMinSize = 256

// maxSIMDLanes bounds the stack scratch used by the horizontal
// reduces: 8 float64 lanes is the widest current target (AVX-512).
const maxSIMDLanes = 8

// boundsF64Scalar is the 2-lane fallback: scalar min/max reduce
// over parallel xs/ys. Preserves the original one-else-if shape
// (which the Go compiler auto-vectorizes cleanly on 2-lane NEON,
// matching or beating explicit SIMD).
// Caller must ensure n > 0.
func boundsF64Scalar(xs, ys []float64, n int) (minX, minY, maxX, maxY float64) {
	return boundsF64ScalarNaN(xs, ys, n)
}

// boundsF64SIMDBody is the lane-parallel min/max reduce. Callable
// from tests so the vector kernel is exercised on 2-lane hardware
// where BoundsF64 would otherwise take the scalar path.
// Caller must ensure n ≥ lane.
func boundsF64SIMDBody(xs, ys []float64, n, lane int) (minX, minY, maxX, maxY float64, ok bool) {
	// Load first lane group and use it to initialize all four
	// accumulators (min-of-xs, min-of-ys, max-of-xs, max-of-ys).
	xAcc := simd.LoadFloat64s(xs)
	yAcc := simd.LoadFloat64s(ys)
	minXV, maxXV := xAcc, xAcc
	minYV, maxYV := yAcc, yAcc
	// NaN tracking: a lane is NaN iff v != v. Hardware min/max can't
	// be trusted with NaN — arm64 FMAX/FMIN propagate it, x86
	// MAXPD/MINPD return the other operand — so record it separately
	// and apply the "any NaN → NaN" rule at the end.
	nanXV := xAcc.NotEqual(xAcc)
	nanYV := yAcc.NotEqual(yAcc)
	i := lane
	for ; i+lane <= n; i += lane {
		xv := simd.LoadFloat64s(xs[i:])
		yv := simd.LoadFloat64s(ys[i:])
		minXV = minXV.Min(xv)
		maxXV = maxXV.Max(xv)
		minYV = minYV.Min(yv)
		maxYV = maxYV.Max(yv)
		nanXV = nanXV.Or(xv.NotEqual(xv))
		nanYV = nanYV.Or(yv.NotEqual(yv))
	}
	// Spill all six accumulators before reading any lane back. A
	// scalar read right after the vector store that wrote it can't be
	// forwarded from the store buffer (wide store, narrower load at a
	// different offset), so it stalls until the store commits — on
	// AVX2 that cost ~300 ns per call when each reduce did its own
	// store-then-read. Issuing every store first overlaps them, so
	// only the first read waits. Stack arrays sized for the widest
	// target avoid a heap scratch slice.
	var sMinX, sMaxX, sMinY, sMaxY [maxSIMDLanes]float64
	var mNanX, mNanY [maxSIMDLanes]int64
	minXV.Store(sMinX[:lane])
	maxXV.Store(sMaxX[:lane])
	minYV.Store(sMinY[:lane])
	maxYV.Store(sMaxY[:lane])
	nanXV.ToInt64s().Store(mNanX[:lane])
	nanYV.ToInt64s().Store(mNanY[:lane])
	minX, maxX, minY, maxY = sMinX[0], sMaxX[0], sMinY[0], sMaxY[0]
	nanX, nanY := mNanX[0] != 0, mNanY[0] != 0
	for j := 1; j < lane; j++ {
		if sMinX[j] < minX {
			minX = sMinX[j]
		}
		if sMaxX[j] > maxX {
			maxX = sMaxX[j]
		}
		if sMinY[j] < minY {
			minY = sMinY[j]
		}
		if sMaxY[j] > maxY {
			maxY = sMaxY[j]
		}
		nanX = nanX || mNanX[j] != 0
		nanY = nanY || mNanY[j] != 0
	}
	// Scalar tail for the leftover (n mod lane) coordinates.
	for ; i < n; i++ {
		x := xs[i]
		if x < minX {
			minX = x
		} else if x > maxX {
			maxX = x
		} else if x != x {
			nanX = true
		}
		y := ys[i]
		if y < minY {
			minY = y
		} else if y > maxY {
			maxY = y
		} else if y != y {
			nanY = true
		}
	}
	if nanX {
		minX, maxX = math.NaN(), math.NaN()
	}
	if nanY {
		minY, maxY = math.NaN(), math.NaN()
	}
	return minX, minY, maxX, maxY, true
}

// PolygonCentroidShoelace — lane-parallel shoelace kernel.
// Loads two staggered coordinate windows (xs[j:] and xs[j+1:])
// per iteration so each lane processes one full segment
// independently, breaking the scalar loop's per-segment
// dependency chain. Per-lane accumulators for areaTwo, cx, cy,
// sx, sy; horizontal reduce at the tail.
//
// # Measured arch behavior
//
// arm64 NEON (2-lane Float64): the compiler's auto-vectorized
// scalar path is already competitive because the M-series
// out-of-order engine ILP-saturates the 5-accumulator loop.
// Explicit SIMD is roughly flat at large sizes and pays setup
// overhead at small sizes; gated off by the lane < 4 check.
//
// amd64 AVX2 (4-lane, Ryzen 7 5800X): ~5.6% faster than scalar from
// 64K points up, slower below ~1K, so the body only engages from
// centroidSIMDMinSize points. AVX-512 (8-lane) is unmeasured.
//
// Semantics + numeric behavior match the scalar path.
// Accumulator ordering differs (lane-parallel reduce vs.
// sequential) which can perturb the last few ULPs on very
// ill-conditioned inputs; tolerance-based tests catch any real
// divergence.
func PolygonCentroidShoelace(xs, ys []float64) (cx, cy float64, ok bool) {
	n := min(len(ys), len(xs))
	if n < 3 {
		return 0, 0, false
	}
	// Size gate first, so small rings never pay for the lane query.
	// The vector body has a fixed cost (spilling and reducing five
	// accumulators). On AVX2 (Ryzen 7 5800X) it lost 5.6× at 64
	// points and 27% at 1K, and won ~5.6% from 64K up; the crossover
	// is between 1K and 64K.
	if n < centroidSIMDMinSize {
		return polygonCentroidShoelaceScalar(xs, ys, n)
	}
	// Arch gate (lane < 4): 2-lane NEON regresses per-segment work vs
	// setup even at large n; the scalar path is competitive there.
	lane := simd.BroadcastFloat64s(0).Len()
	if lane < 4 || lane > maxSIMDLanes || n < lane+1 {
		return polygonCentroidShoelaceScalar(xs, ys, n)
	}
	return polygonCentroidShoelaceSIMDBody(xs, ys, n, lane)
}

// polygonCentroidShoelaceSIMDBody is the ungated shoelace kernel.
// Callable from tests so the vector body is exercised on 2-lane
// hardware where PolygonCentroidShoelace would otherwise take the
// scalar path. Caller must ensure n ≥ lane+1 and lane ≤ maxSIMDLanes.
func polygonCentroidShoelaceSIMDBody(xs, ys []float64, n, lane int) (cx, cy float64, ok bool) {
	zero := simd.BroadcastFloat64s(0)
	areaAcc, cxAcc, cyAcc, sxAcc, syAcc := zero, zero, zero, zero, zero

	// Vectorized body: iterate segments in groups of `lane`.
	// Segment index j starts at 0; each iter processes segments
	// j..j+lane-1 (endpoints at indices j..j+lane).
	var j int
	for j = 0; j+lane+1 <= n; j += lane {
		xLo := simd.LoadFloat64s(xs[j:])
		yLo := simd.LoadFloat64s(ys[j:])
		xHi := simd.LoadFloat64s(xs[j+1:])
		yHi := simd.LoadFloat64s(ys[j+1:])
		// cross = xLo*yHi - xHi*yLo per lane
		cross := xLo.Mul(yHi).Sub(xHi.Mul(yLo))
		areaAcc = areaAcc.Add(cross)
		// cx += (xLo + xHi) * cross ; cy += (yLo + yHi) * cross
		cxAcc = cxAcc.Add(xLo.Add(xHi).Mul(cross))
		cyAcc = cyAcc.Add(yLo.Add(yHi).Mul(cross))
		// sx += xLo (segment-start accumulator, used only on the
		// zero-area fallback path — matches the scalar shape).
		sxAcc = sxAcc.Add(xLo)
		syAcc = syAcc.Add(yLo)
	}

	// Horizontal reduce. Spill all five accumulators before reading
	// any lane back, for the same store-forwarding reason as
	// boundsF64SIMDBody; each accumulator is still summed in lane
	// order, so results are bit-identical to reducing one at a time.
	var sArea, sCx, sCy, sSx, sSy [maxSIMDLanes]float64
	areaAcc.Store(sArea[:lane])
	cxAcc.Store(sCx[:lane])
	cyAcc.Store(sCy[:lane])
	sxAcc.Store(sSx[:lane])
	syAcc.Store(sSy[:lane])
	var areaTwo, cxSum, cySum, sxSum, sySum float64
	for k := range lane {
		areaTwo += sArea[k]
	}
	for k := range lane {
		cxSum += sCx[k]
	}
	for k := range lane {
		cySum += sCy[k]
	}
	for k := range lane {
		sxSum += sSx[k]
	}
	for k := range lane {
		sySum += sSy[k]
	}

	// Scalar tail: any segments the SIMD body didn't fit.
	for ; j < n-1; j++ {
		px, py := xs[j], ys[j]
		x, y := xs[j+1], ys[j+1]
		cross := px*y - x*py
		areaTwo += cross
		cxSum += (px + x) * cross
		cySum += (py + y) * cross
		sxSum += px
		sySum += py
	}

	// Closing edge: (xs[n-1], ys[n-1]) → (xs[0], ys[0]). Add only
	// when the ring wasn't already closed. Matches the scalar
	// polygonCentroidShoelaceScalar semantics exactly.
	fx, fy := xs[0], ys[0]
	lx, ly := xs[n-1], ys[n-1]
	var segCount int
	if lx == fx && ly == fy {
		segCount = n - 1
	} else {
		cross := lx*fy - fx*ly
		areaTwo += cross
		cxSum += (lx + fx) * cross
		cySum += (ly + fy) * cross
		sxSum += lx
		sySum += ly
		segCount = n
	}
	if areaTwo == 0 {
		return sxSum / float64(segCount), sySum / float64(segCount), true
	}
	return cxSum / (3 * areaTwo), cySum / (3 * areaTwo), true
}

// runtimeLane returns the Float64s SIMD lane count on the current
// build target. Exposed so tests can query it without importing
// the `simd` package themselves — importing `simd` from a
// `_test.go` file triggers a Go 1.27 compiler ICE.
//
// TODO: file an upstream issue at https://github.com/golang/go/issues
// once we have a reduced repro; drop this indirection once the
// stdlib SIMD experiment stabilizes and the ICE is fixed. Until
// then, this wrapper is the workaround: production code imports
// `simd` freely, tests go through runtimeLane and the exported
// SIMDBody helpers.
func runtimeLane() int { return simd.BroadcastFloat64s(0).Len() }

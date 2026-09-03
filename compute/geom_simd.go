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

import "simd"

// BoundsF64 — lane-parallel min/max reduce on parallel Xs/Ys.
// Matches the scalar signature; scalar tail handles the last
// (n mod lane_count) coordinates.
func BoundsF64(xs, ys []float64) (minX, minY, maxX, maxY float64, ok bool) {
	if len(xs) == 0 || len(ys) == 0 {
		return 0, 0, 0, 0, false
	}
	n := min(len(ys), len(xs))
	lane := simd.BroadcastFloat64s(0).Len()
	if n < lane {
		// Scalar path for tiny input — SIMD setup + horizontal
		// reduce overhead would dominate.
		minX, maxX = xs[0], xs[0]
		minY, maxY = ys[0], ys[0]
		for i := 1; i < n; i++ {
			x := xs[i]
			if x < minX {
				minX = x
			} else if x > maxX {
				maxX = x
			}
			y := ys[i]
			if y < minY {
				minY = y
			} else if y > maxY {
				maxY = y
			}
		}
		return minX, minY, maxX, maxY, true
	}
	// Load first lane group and use it to initialize all four
	// accumulators (min-of-xs, min-of-ys, max-of-xs, max-of-ys).
	xAcc := simd.LoadFloat64s(xs)
	yAcc := simd.LoadFloat64s(ys)
	minXV, maxXV := xAcc, xAcc
	minYV, maxYV := yAcc, yAcc
	i := lane
	for ; i+lane <= n; i += lane {
		xv := simd.LoadFloat64s(xs[i:])
		yv := simd.LoadFloat64s(ys[i:])
		minXV = minXV.Min(xv)
		maxXV = maxXV.Max(xv)
		minYV = minYV.Min(yv)
		maxYV = maxYV.Max(yv)
	}
	// Horizontal-reduce each accumulator vector.
	scratch := make([]float64, lane)
	minXV.Store(scratch)
	minX = scratch[0]
	for _, v := range scratch[1:] {
		if v < minX {
			minX = v
		}
	}
	maxXV.Store(scratch)
	maxX = scratch[0]
	for _, v := range scratch[1:] {
		if v > maxX {
			maxX = v
		}
	}
	minYV.Store(scratch)
	minY = scratch[0]
	for _, v := range scratch[1:] {
		if v < minY {
			minY = v
		}
	}
	maxYV.Store(scratch)
	maxY = scratch[0]
	for _, v := range scratch[1:] {
		if v > maxY {
			maxY = v
		}
	}
	// Scalar tail for the leftover (n mod lane) coordinates.
	for ; i < n; i++ {
		x := xs[i]
		if x < minX {
			minX = x
		} else if x > maxX {
			maxX = x
		}
		y := ys[i]
		if y < minY {
			minY = y
		} else if y > maxY {
			maxY = y
		}
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
// overhead at small sizes. `simdMinSize` (below) gates the SIMD
// body at n≥64 so small-polygon workloads don't regress.
//
// amd64 AVX2 (4-lane) / AVX-512 (8-lane): higher lane count
// gives explicit SIMD the lane budget to beat the compiler's
// auto-vectorization. Not measurable locally on the Apple M3
// dev machine, but the kernel is written to scale.
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
	lane := simd.BroadcastFloat64s(0).Len()
	// Small-size gate: SIMD setup + horizontal reduce dominate
	// below simdMinSize on 2-lane NEON. Threshold empirically
	// picked from the geom_bench_test.go measurements — n<64
	// consistently regresses on M3, n≥64 is a wash or a small
	// win. On amd64's wider lanes the crossover is lower;
	// keeping a single threshold trades some potential amd64
	// benefit for portable predictability.
	const simdMinSize = 64
	if n < simdMinSize || n < lane+1 {
		return polygonCentroidShoelaceScalar(xs, ys, n)
	}

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

	// Horizontal reduce each accumulator to scalar.
	scratch := make([]float64, lane)
	var areaTwo, cxSum, cySum, sxSum, sySum float64
	areaAcc.Store(scratch)
	for _, v := range scratch {
		areaTwo += v
	}
	cxAcc.Store(scratch)
	for _, v := range scratch {
		cxSum += v
	}
	cyAcc.Store(scratch)
	for _, v := range scratch {
		cySum += v
	}
	sxAcc.Store(scratch)
	for _, v := range scratch {
		sxSum += v
	}
	syAcc.Store(scratch)
	for _, v := range scratch {
		sySum += v
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

// PIPCrossingCount — lane-parallel even-odd crossing kernel.
// The reformulated running-count form (Slice 6c) lets each lane
// track its own segment's contribution independently; a
// horizontal reduce at the tail sums the per-lane counts and
// parity-checks the total.
//
// # Vector body shape
//
// For each lane group of `lane` interior segments, load staggered
// coordinate windows (xs[j:] as segment starts j, xs[j+1:] as
// ends i). The crossing test per lane uses a **divless
// reformulation** — the scalar path evaluates
// `tx < (xj-xi)*(ty-yi)/(yj-yi) + xi` which pays a per-lane
// float64 divide even when the straddle mask is false. Vectorized
// as-is this loses badly to the scalar branch (Apple M3: 2.4×
// slower; the scalar body only computes xInter on the ~2 segments
// that actually cross, while the SIMD body computes it on all n).
//
// Divless equivalent: let dy = yj-yi, then multiply both sides by
// `dy`. The inequality direction depends on sign(dy):
//
//	tx < xj*t + xi*(1-t)   where t = (ty-yi)/(yj-yi)
//	⇔ (tx-xi) < (xj-xi)*(ty-yi)/(yj-yi)
//	⇔ pred := (tx-xi)*(yj-yi) - (xj-xi)*(ty-yi) < 0 when dy > 0
//	⇔ pred > 0 when dy < 0
//	⇔ sign(pred) != sign(dy)
//	⇔ (pred<0) XOR (dy<0)
//
// Straddle is guaranteed to imply `dy != 0` (if yj == yi then the
// two endpoints are on the same side of ty and straddle is false),
// so the sign test is well-defined in every counted lane.
//
// Neither Mask64s XOR nor Not is in the current simd surface
// (only And/Or), so both XORs are expressed via `Int64s.Xor` after
// promoting each mask to 1/0 via `vOnes.Masked(m)`.
//
// The closing segment (n-1, 0) and any leftover tail past the
// last aligned lane group are handled by a compact scalar loop.
// Semantics match the scalar path exactly (accumulator ordering
// is commutative — crossings are unordered additions).
//
// # Arch expectations
//
// arm64 NEON (2-lane, Apple M3): **regresses ~2.4×** across every
// bench size. Root cause: on typical convex ring + interior-point
// queries the straddle rate is O(1/n) — the scalar branch skips
// almost every segment's compute, while the SIMD body pays full
// mul-sub-mul-sub-mask work in every lane. Even the divless form
// (this kernel) doesn't recover the branch-elimination advantage
// on 2-lane hardware. Ampere / Graviton Neoverse-N-class cores
// have similar 2-lane NEON and are expected to behave the same;
// no measurement yet.
//
// amd64 AVX2 (4-lane) / AVX-512 (8-lane): larger lane budget
// tips the balance — 4-8× parallel work per iter outweighs the
// wasted per-lane compute. Not measurable on the M3 dev machine;
// gated behind a `lane >= 4` check so 2-lane hardware falls back
// to scalar until Ampere / Graviton measurement lands.
//
// simdMinSize gates the SIMD body at n≥64 so per-refine-call
// small polygons (5-vertex bboxes) don't pay the setup +
// horizontal reduce overhead.
func PIPCrossingCount(xs, ys []float64, tx, ty float64) bool {
	n := min(len(ys), len(xs))
	if n < 3 {
		return false
	}
	lane := simd.BroadcastFloat64s(0).Len()
	const simdMinSize = 64
	// 2-lane NEON regresses on measured Apple hardware — see the
	// Slice 8 comment block above. Fall back to scalar until a
	// server-class ARM64 measurement (Ampere / Graviton) shows a
	// win or a divless form that recovers Apple parity lands.
	if lane < 4 || n < simdMinSize || n < lane+1 {
		return pipCrossingCountScalar(xs, ys, tx, ty, n)
	}
	return pipCrossingCountSIMDBody(xs, ys, tx, ty, n, lane)
}

// runtimeLane returns the Float64s SIMD lane count on the current
// build target. Exposed so tests can query it without importing
// the `simd` package themselves (which triggers a Go 1.27
// compiler ICE when imported directly from a `_test.go` file).
func runtimeLane() int { return simd.BroadcastFloat64s(0).Len() }

// pipCrossingCountSIMDBody is the divless SIMD kernel body. Kept
// as a separate function so parity tests can exercise it on 2-lane
// hardware where the public PIPCrossingCount would otherwise take
// the scalar path (see the `lane < 4` gate above). Caller must
// ensure n ≥ max(simdMinSize, lane+1); no re-check inside.
func pipCrossingCountSIMDBody(xs, ys []float64, tx, ty float64, n, lane int) bool {
	vTy := simd.BroadcastFloat64s(ty)
	vTx := simd.BroadcastFloat64s(tx)
	vZero := simd.BroadcastFloat64s(0)
	vOnes := simd.BroadcastInt64s(1)
	crossingsAcc := simd.BroadcastInt64s(0)

	// Vector body: interior segments (j, j+1) for j in [0, n-1-lane].
	// The closing segment (n-1, 0) is handled after the tail.
	var j int
	for j = 0; j+lane+1 <= n; j += lane {
		xj := simd.LoadFloat64s(xs[j:])
		yj := simd.LoadFloat64s(ys[j:])
		xi := simd.LoadFloat64s(xs[j+1:])
		yi := simd.LoadFloat64s(ys[j+1:])

		aboveJ := yj.Greater(vTy)
		aboveI := yi.Greater(vTy)
		aboveJI := vOnes.Masked(aboveJ)
		aboveII := vOnes.Masked(aboveI)
		straddle := aboveJI.Xor(aboveII).ToMask()

		// pred = (tx-xi)*(yj-yi) - (xj-xi)*(ty-yi)
		// crossing = straddle AND ((pred<0) XOR (dy<0))
		dtxi := vTx.Sub(xi)
		dtyi := vTy.Sub(yi)
		dx := xj.Sub(xi)
		dy := yj.Sub(yi)
		pred := dtxi.Mul(dy).Sub(dx.Mul(dtyi))

		predNeg := pred.Less(vZero)
		dyNeg := dy.Less(vZero)
		predNegI := vOnes.Masked(predNeg)
		dyNegI := vOnes.Masked(dyNeg)
		crossHere := predNegI.Xor(dyNegI).ToMask()

		crossAdd := straddle.And(crossHere)
		crossingsAcc = crossingsAcc.Add(vOnes.Masked(crossAdd))
	}

	// Horizontal reduce the per-lane crossing counts.
	scratch := make([]int64, lane)
	crossingsAcc.Store(scratch)
	var crossings int64
	for _, v := range scratch {
		crossings += v
	}

	// Scalar tail: any interior segments the SIMD body didn't fit.
	for ; j < n-1; j++ {
		yStart := ys[j]
		yEnd := ys[j+1]
		if (yStart > ty) != (yEnd > ty) {
			xStart := xs[j]
			xEnd := xs[j+1]
			xInter := (xStart-xEnd)*(ty-yEnd)/(yStart-yEnd) + xEnd
			if tx < xInter {
				crossings++
			}
		}
	}

	// Closing segment (n-1, 0). Matches the scalar j=n-1, i=0 case.
	yStart := ys[n-1]
	yEnd := ys[0]
	if (yStart > ty) != (yEnd > ty) {
		xStart := xs[n-1]
		xEnd := xs[0]
		xInter := (xStart-xEnd)*(ty-yEnd)/(yStart-yEnd) + xEnd
		if tx < xInter {
			crossings++
		}
	}
	return crossings&1 == 1
}

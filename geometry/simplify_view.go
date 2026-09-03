package geometry

// SimplifyDPFromXY runs iterative Douglas-Peucker on parallel
// Xs / Ys slabs, returning a fresh pair of slabs containing the
// retained coordinates. Endpoints are always preserved.
// tolerance ≤ 0 or n < 3 returns a copy of the input coordinates.
//
// # Design vs. the AoS douglasPeucker
//
// The AoS `douglasPeucker([]Point, float64)` in simplify.go is
// recursive; at every split it slices the []Point twice and
// stitches the two halves with a fresh append allocation.
// Frequent splits on real-world polylines (coastlines, admin
// boundaries) mean O(log n) heap allocations per polyline
// alongside the O(n) `[]Point` walks the recursion drives.
//
// This SoA rewrite is iterative on an explicit (lo, hi) stack
// plus a keep-bitmap:
//
//   - One []bool allocation of length n.
//   - One stack allocation (typically log₂(n) frames — 20-ish
//     for a coastline-scale ring).
//   - Two final output []float64 allocations sized to the exact
//     retained count.
//
// Total: 3-4 allocations regardless of split count, vs the AoS
// recursion's O(log n) per-split appends.
//
// The perpendicular-distance kernel avoids the sqrt+div on
// non-splitting segments: instead of computing
// `d = |cross| / segLen` per point and comparing against
// `tolerance`, it tracks `argmax(cross²)` across the sub-array
// and compares once against `tolerance² * segLen²`. Saves one
// sqrt per split when no interior point exceeds tolerance
// (the common case near the leaves of the DP tree).
//
// # Determinism vs. AoS
//
// Split order matches the AoS recursion (left before right) so
// the produced vertex indices are identical for well-formed
// input. Tie-breaking on argmax also matches (both use strict
// `>` which picks the first occurrence of the max).
func SimplifyDPFromXY(xs, ys []float64, tolerance float64) (outXs, outYs []float64) {
	n := min(len(xs), len(ys))
	if n < 3 || tolerance <= 0 {
		outXs = append([]float64(nil), xs[:n]...)
		outYs = append([]float64(nil), ys[:n]...)
		return
	}
	keep := make([]bool, n)
	keep[0] = true
	keep[n-1] = true

	type frame struct{ lo, hi int }
	stack := make([]frame, 0, 32)
	stack = append(stack, frame{0, n - 1})
	tol2 := tolerance * tolerance

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		lo, hi := f.lo, f.hi
		if hi-lo < 2 {
			continue
		}
		ax, ay := xs[lo], ys[lo]
		bx, by := xs[hi], ys[hi]
		dx := bx - ax
		dy := by - ay
		segLen2 := dx*dx + dy*dy

		var (
			maxMetric float64
			maxIdx    int
		)
		if segLen2 == 0 {
			// Coincident endpoints — distance is Euclidean to `a`.
			for i := lo + 1; i < hi; i++ {
				dxi := xs[i] - ax
				dyi := ys[i] - ay
				d2 := dxi*dxi + dyi*dyi
				if d2 > maxMetric {
					maxMetric = d2
					maxIdx = i
				}
			}
			if maxMetric <= tol2 {
				continue
			}
		} else {
			// Signed 2D cross product magnitude squared.
			// d = |cross| / segLen, so d ≤ tol iff cross² ≤ tol²·segLen².
			for i := lo + 1; i < hi; i++ {
				pxi := xs[i] - ax
				pyi := ys[i] - ay
				cross := dx*pyi - dy*pxi
				cross2 := cross * cross
				if cross2 > maxMetric {
					maxMetric = cross2
					maxIdx = i
				}
			}
			if maxMetric <= tol2*segLen2 {
				continue
			}
		}
		keep[maxIdx] = true
		// Push right first so left is processed first on pop —
		// matches the AoS recursion's left-then-right order.
		if hi-maxIdx > 1 {
			stack = append(stack, frame{maxIdx, hi})
		}
		if maxIdx-lo > 1 {
			stack = append(stack, frame{lo, maxIdx})
		}
	}

	m := 0
	for _, k := range keep {
		if k {
			m++
		}
	}
	outXs = make([]float64, m)
	outYs = make([]float64, m)
	j := 0
	for i, k := range keep {
		if k {
			outXs[j] = xs[i]
			outYs[j] = ys[i]
			j++
		}
	}
	return
}

// SimplifyDP applies Douglas-Peucker to the coordinates held by
// v, returning a new PointsView with the same CRS and HasZ. Z
// coordinates (when v.HasZ) are copied for retained indices —
// the split decisions use XY only, matching the AoS shape.
//
// This is the amortized-view entry point: callers holding a
// materialized PointsView (via LineString.View() or
// Polygon.RingViews()) can simplify without going through the
// AoS []Point round-trip.
func (v PointsView) SimplifyDP(tolerance float64) PointsView {
	n := v.Len()
	if n < 3 || tolerance <= 0 {
		out := PointsView{
			Xs:   append([]float64(nil), v.Xs...),
			Ys:   append([]float64(nil), v.Ys...),
			HasZ: v.HasZ,
			CRS:  v.CRS,
		}
		if v.HasZ {
			out.Zs = append([]float64(nil), v.Zs...)
		}
		return out
	}
	if !v.HasZ {
		outXs, outYs := SimplifyDPFromXY(v.Xs, v.Ys, tolerance)
		return PointsView{Xs: outXs, Ys: outYs, CRS: v.CRS}
	}
	// XYZ variant: run DP on XY, walk keep-bitmap for Z alongside.
	// Reproduces SimplifyDPFromXY body inline to keep Z coupled.
	keep := simplifyDPKeep(v.Xs, v.Ys, tolerance)
	m := 0
	for _, k := range keep {
		if k {
			m++
		}
	}
	out := PointsView{
		Xs:   make([]float64, m),
		Ys:   make([]float64, m),
		Zs:   make([]float64, m),
		HasZ: true,
		CRS:  v.CRS,
	}
	j := 0
	for i, k := range keep {
		if k {
			out.Xs[j] = v.Xs[i]
			out.Ys[j] = v.Ys[i]
			out.Zs[j] = v.Zs[i]
			j++
		}
	}
	return out
}

// simplifyDPKeep runs the DP argmax-and-split loop and returns
// the retained-index bitmap. Extracted so the XYZ path in
// SimplifyDP can drive its own coordinate copy without a second
// pass through SimplifyDPFromXY.
func simplifyDPKeep(xs, ys []float64, tolerance float64) []bool {
	n := min(len(xs), len(ys))
	keep := make([]bool, n)
	if n < 3 || tolerance <= 0 {
		for i := range keep {
			keep[i] = true
		}
		return keep
	}
	keep[0] = true
	keep[n-1] = true

	type frame struct{ lo, hi int }
	stack := make([]frame, 0, 32)
	stack = append(stack, frame{0, n - 1})
	tol2 := tolerance * tolerance

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		lo, hi := f.lo, f.hi
		if hi-lo < 2 {
			continue
		}
		ax, ay := xs[lo], ys[lo]
		bx, by := xs[hi], ys[hi]
		dx := bx - ax
		dy := by - ay
		segLen2 := dx*dx + dy*dy

		var (
			maxMetric float64
			maxIdx    int
		)
		if segLen2 == 0 {
			for i := lo + 1; i < hi; i++ {
				dxi := xs[i] - ax
				dyi := ys[i] - ay
				d2 := dxi*dxi + dyi*dyi
				if d2 > maxMetric {
					maxMetric = d2
					maxIdx = i
				}
			}
			if maxMetric <= tol2 {
				continue
			}
		} else {
			for i := lo + 1; i < hi; i++ {
				pxi := xs[i] - ax
				pyi := ys[i] - ay
				cross := dx*pyi - dy*pxi
				cross2 := cross * cross
				if cross2 > maxMetric {
					maxMetric = cross2
					maxIdx = i
				}
			}
			if maxMetric <= tol2*segLen2 {
				continue
			}
		}
		keep[maxIdx] = true
		if hi-maxIdx > 1 {
			stack = append(stack, frame{maxIdx, hi})
		}
		if maxIdx-lo > 1 {
			stack = append(stack, frame{lo, maxIdx})
		}
	}
	return keep
}

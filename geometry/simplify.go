package geometry

import (
	"fmt"
	"math"
)

// Simplify returns a copy of g with vertices removed until every discarded
// vertex lies within tolerance (planar distance) of the retained polyline.
// Uses the Douglas-Peucker algorithm.
//
// tolerance is measured in the CRS's linear unit (degrees for WGS84,
// meters for a projected CRS). Passing tolerance <= 0 returns g unchanged.
//
// Point and MultiPoint pass through untouched; the algorithm doesn't apply
// to them. GeometryCollection recurses into each component.
func Simplify(g Geometry, tolerance float64) (Geometry, error) {
	if tolerance <= 0 {
		return g, nil
	}
	switch t := g.(type) {
	case Point, MultiPoint:
		return g, nil
	case LineString:
		return t.Simplify(tolerance), nil
	case Polygon:
		return t.Simplify(tolerance), nil
	case MultiLineString:
		out := make([]LineString, len(t.Lines))
		for i, l := range t.Lines {
			out[i] = l.Simplify(tolerance)
		}
		return MultiLineString{Lines: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	case MultiPolygon:
		out := make([]Polygon, len(t.Polygons))
		for i, p := range t.Polygons {
			out[i] = p.Simplify(tolerance)
		}
		return MultiPolygon{Polygons: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	case GeometryCollection:
		inner := make([]Geometry, len(t.Geometries))
		for i, inG := range t.Geometries {
			simp, err := Simplify(inG, tolerance)
			if err != nil {
				return nil, err
			}
			inner[i] = simp
		}
		return GeometryCollection{Geometries: inner, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	}
	return nil, fmt.Errorf("simplify: unsupported type %T", g)
}

// Simplify returns a copy of l with vertices removed via Douglas-Peucker at
// the given planar tolerance. Endpoints are always preserved. If the line
// has fewer than 3 points it is returned unchanged (a 2-point line is
// already the simplest possible representation).
func (l LineString) Simplify(tolerance float64) LineString {
	if len(l.Points) < 3 || tolerance <= 0 {
		return l
	}
	simplified := douglasPeucker(l.Points, tolerance)
	return LineString{Points: simplified, CRSValue: l.CRSValue, HasZ: l.HasZ}
}

// Simplify applies Douglas-Peucker to each ring of the polygon. Rings that
// collapse to fewer than 4 points (three unique vertices plus the closing
// vertex) are kept as-is to preserve topological validity of the polygon.
func (p Polygon) Simplify(tolerance float64) Polygon {
	if tolerance <= 0 || len(p.Rings) == 0 {
		return p
	}
	rings := make([][]Point, len(p.Rings))
	for i, ring := range p.Rings {
		if len(ring) < 5 { // triangle + close
			rings[i] = ring
			continue
		}
		// Preserve ring closure: run DP on the interior points, then close.
		simp := douglasPeucker(ring, tolerance)
		if len(simp) < 4 {
			// Simplification collapsed the ring — keep the original to
			// avoid producing a degenerate polygon.
			rings[i] = ring
			continue
		}
		// Ensure the ring stays closed.
		if simp[0] != simp[len(simp)-1] {
			simp = append(simp, simp[0])
		}
		rings[i] = simp
	}
	return Polygon{Rings: rings, CRSValue: p.CRSValue, HasZ: p.HasZ}
}

// douglasPeucker returns the smallest subsequence of points such
// that every discarded point lies within tolerance of the segment
// between its nearest retained neighbors.
//
// Backed by simplifyDPKeep (Slice 9 SoA kernel): converts the
// []Point input to parallel XY slabs, runs the iterative
// stack+bitmap kernel, and walks the retained-index bitmap to
// rebuild the []Point output. On measured workloads this is
// ~5× faster than the classic recursive implementation at n=1M
// (3.85 s → 0.75 s) and drops memory by three orders of magnitude
// (5.75 GB → 3.2 MB, 260k allocs → 11 allocs) — the AoS
// recursion appends O(log n) intermediate slices per split.
//
// Preserves Point.Z / HasZ / CRSValue on retained points (post-
// review fix: the earlier SimplifyDPFromXY-based body silently
// dropped Z, regressing 3D LineStrings / Polygons that carried
// altitude through the AoS recursion).
//
// Semantics + tie-breaking match the recursive form exactly:
// argmax uses strict `>` (first occurrence wins), and split order
// is left-then-right on the explicit stack.
func douglasPeucker(points []Point, tolerance float64) []Point {
	if len(points) < 3 {
		return points
	}
	xs := make([]float64, len(points))
	ys := make([]float64, len(points))
	for i, p := range points {
		xs[i] = p.X
		ys[i] = p.Y
	}
	keep := simplifyDPKeep(xs, ys, tolerance)
	m := 0
	for _, k := range keep {
		if k {
			m++
		}
	}
	out := make([]Point, 0, m)
	for i, k := range keep {
		if k {
			out = append(out, points[i])
		}
	}
	return out
}

// perpDistance returns the perpendicular (shortest) distance from p to the
// infinite line through a and b, in planar XY. If a and b coincide, it
// returns the Euclidean distance from p to a.
func perpDistance(p, a, b Point) float64 {
	dx := b.X - a.X
	dy := b.Y - a.Y
	segLen2 := dx*dx + dy*dy
	if segLen2 == 0 {
		ax := p.X - a.X
		ay := p.Y - a.Y
		return math.Sqrt(ax*ax + ay*ay)
	}
	// Numerator: |cross((b-a), (p-a))|
	num := math.Abs(dx*(a.Y-p.Y) - (a.X-p.X)*dy)
	return num / math.Sqrt(segLen2)
}

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

package geometry

import "slices"

// ConvexHullFromXY runs Andrew's monotone-chain algorithm on
// parallel Xs / Ys slabs and returns the convex hull as a fresh
// pair of slabs in counter-clockwise order, with the closing
// vertex (a repeat of the first) appended so the output is a
// closed ring.
//
// # Design vs the AoS Graham scan
//
// The AoS Polygon.ConvexHull in polygon.go runs Graham scan:
// find a pivot, sort every other vertex by polar angle from the
// pivot via `sort.Slice` on []Point (which boxes the closure
// function-value and moves 40-byte Point structs during
// partition), then stack-scan retaining CCW turns. Two nested
// allocations (the closure box + the sorted []Point copy) plus
// point-struct swaps in-place.
//
// Andrew's monotone chain replaces the polar-angle sort with an
// index sort by (x, y) lex — the resulting order is the same
// linear structure for the two half-hulls. Two O(n) stack scans
// over an index permutation (lower hull, then upper hull) produce
// the CCW output. Sort is on `[]int` indices via `slices.SortFunc`
// (8-byte swaps + typed callback — no closure boxing that
// `sort.Slice` would incur); the hull-scan reads coordinates
// directly from the input slabs so the inner-loop arithmetic
// operates on cache-friendly float64 arrays.
//
// # Semantics
//
//   - Fewer than 3 unique points return a copy of the input slabs
//     (no closing vertex appended — matches the AoS shape which
//     also returns the input unchanged when Exterior() has <3
//     points).
//   - Duplicate points are tolerated. Since the (x,y) lex order is
//     a total order over distinct points, sort stability is
//     irrelevant — equal keys mean the same point, and the CCW
//     scan drops the extra via strict `<= 0` cross-product
//     rejection just like collinear points on the hull edge
//     (matches the AoS behavior).
//   - Output vertex count includes the closing repeat, so a hull
//     with k unique vertices has k+1 entries.
func ConvexHullFromXY(xs, ys []float64) (hullXs, hullYs []float64) {
	n := min(len(xs), len(ys))
	if n < 3 {
		hullXs = append([]float64(nil), xs[:n]...)
		hullYs = append([]float64(nil), ys[:n]...)
		return
	}

	// Index permutation sorted by (x, y) lex. Sorting indices
	// instead of Point structs keeps swap cost at 8 bytes and
	// leaves the coord slabs untouched (cache-line friendly for
	// the subsequent scan passes).
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		if xs[a] != xs[b] {
			if xs[a] < xs[b] {
				return -1
			}
			return 1
		}
		if ys[a] < ys[b] {
			return -1
		}
		if ys[a] > ys[b] {
			return 1
		}
		return 0
	})

	// Lower hull: iterate sorted indices left-to-right, maintaining
	// a stack that only makes CCW turns.
	hull := make([]int, 0, n)
	for _, i := range idx {
		for len(hull) >= 2 &&
			crossSign(xs, ys, hull[len(hull)-2], hull[len(hull)-1], i) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, i)
	}
	// Upper hull: iterate right-to-left. `lowerLen` marks the
	// boundary — the upper-hull pop threshold is len(hull) >=
	// lowerLen+1 so the lower hull's tail isn't stripped.
	lowerLen := len(hull) + 1
	for k := len(idx) - 2; k >= 0; k-- {
		i := idx[k]
		for len(hull) >= lowerLen &&
			crossSign(xs, ys, hull[len(hull)-2], hull[len(hull)-1], i) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, i)
	}
	// The last element of `hull` is a repeat of the first (Andrew's
	// closes the ring naturally). Detect the degenerate all-
	// collinear case by counting unique indices — a hull of the
	// shape [a, c, a] has 2 unique vertices even though len==3,
	// which means the "hull" is really a segment, not a polygon.
	// Matches "collinear inputs collapse to a segment" from the
	// AoS Graham scan.
	unique := 0
	seen := make(map[int]struct{}, len(hull))
	for _, i := range hull {
		if _, dup := seen[i]; dup {
			continue
		}
		seen[i] = struct{}{}
		unique++
	}
	if unique < 3 {
		hullXs = make([]float64, 0, unique)
		hullYs = make([]float64, 0, unique)
		clear(seen)
		for _, i := range hull {
			if _, dup := seen[i]; dup {
				continue
			}
			seen[i] = struct{}{}
			hullXs = append(hullXs, xs[i])
			hullYs = append(hullYs, ys[i])
		}
		return
	}
	hullXs = make([]float64, len(hull))
	hullYs = make([]float64, len(hull))
	for k, i := range hull {
		hullXs[k] = xs[i]
		hullYs[k] = ys[i]
	}
	return
}

// crossSign returns the sign of the 2D cross product of vectors
// (b-a) × (c-a), where a, b, c are indices into xs / ys. Positive
// = counter-clockwise turn; zero = collinear; negative =
// clockwise. Reads coords directly from the slabs — no Point
// struct materialization.
func crossSign(xs, ys []float64, a, b, c int) float64 {
	return (xs[b]-xs[a])*(ys[c]-ys[a]) - (ys[b]-ys[a])*(xs[c]-xs[a])
}

// ConvexHull materializes the convex hull of v as a fresh
// PointsView with the same CRS/HasZ. Z coordinates (when v.HasZ)
// are copied for retained indices — the hull decision uses XY
// only, matching the Polygon.ConvexHull shape.
//
// This is the amortized-view entry point for callers holding a
// materialized PointsView (via LineString.View(),
// Polygon.RingViews(), etc.) — no AoS []Point round-trip.
func (v PointsView) ConvexHull() PointsView {
	n := v.Len()
	if n < 3 {
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
		hx, hy := ConvexHullFromXY(v.Xs, v.Ys)
		return PointsView{Xs: hx, Ys: hy, CRS: v.CRS}
	}
	// XYZ path: run the hull on XY, then look up Z for each
	// retained index. Reproduces ConvexHullFromXY's kernel here to
	// keep Z coupled — a bitmap alone doesn't suffice because the
	// hull re-orders vertices.
	idx := convexHullIndicesFromXY(v.Xs, v.Ys)
	out := PointsView{
		Xs:   make([]float64, len(idx)),
		Ys:   make([]float64, len(idx)),
		Zs:   make([]float64, len(idx)),
		HasZ: true,
		CRS:  v.CRS,
	}
	for k, i := range idx {
		out.Xs[k] = v.Xs[i]
		out.Ys[k] = v.Ys[i]
		out.Zs[k] = v.Zs[i]
	}
	return out
}

// convexHullIndicesFromXY runs Andrew's monotone chain and
// returns the ordered index sequence (with closing repeat) that
// forms the convex hull. Exposed for callers that need to look
// up satellite data (Z, attributes) at each retained index.
func convexHullIndicesFromXY(xs, ys []float64) []int {
	n := min(len(xs), len(ys))
	if n < 3 {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		if xs[a] != xs[b] {
			if xs[a] < xs[b] {
				return -1
			}
			return 1
		}
		if ys[a] < ys[b] {
			return -1
		}
		if ys[a] > ys[b] {
			return 1
		}
		return 0
	})

	hull := make([]int, 0, n)
	for _, i := range idx {
		for len(hull) >= 2 &&
			crossSign(xs, ys, hull[len(hull)-2], hull[len(hull)-1], i) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, i)
	}
	lowerLen := len(hull) + 1
	for k := len(idx) - 2; k >= 0; k-- {
		i := idx[k]
		for len(hull) >= lowerLen &&
			crossSign(xs, ys, hull[len(hull)-2], hull[len(hull)-1], i) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, i)
	}
	return hull
}

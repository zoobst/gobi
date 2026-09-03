package geometry

// PIPRingFromXY reports whether the point (tx, ty) lies inside the
// polygon ring defined by parallel Xs / Ys slices. Even-odd
// crossing rule; matches the semantics of pointInRing on the AoS
// `[]Point` representation.
//
// Handles both closed rings (last point equals first) and unclosed
// rings — the crossing test walks segments (i, i+1) for i in
// [0, n-1) and (if the ring isn't already closed) implicitly the
// (n-1, 0) closing segment via the modulo-wrap loop shape.
//
// Points on the boundary have undefined containment, same as the
// AoS pointInRing kernel. Callers requiring boundary-inclusive
// semantics should combine with a separate on-boundary check
// (matches pointInPolygon → pointOnPolygonBoundary in the AoS path).
//
// Empty ring (fewer than 3 points) or mismatched-length slices
// return false. Zero-allocation on every input.
func PIPRingFromXY(xs, ys []float64, tx, ty float64) bool {
	n := min(len(xs), len(ys))
	if n < 3 {
		return false
	}
	// The classic Jordan-curve algorithm: for each segment (xi, yi)
	// → (xj, yj), test whether the horizontal ray from (tx, ty)
	// crosses the segment. Toggle `inside` on each crossing.
	//
	// This shape walks pairs (i, i+1) with i ranging [0, n-1) plus
	// the closing pair (n-1, 0). Coincident last==first points
	// contribute a zero-length segment which the (yi > ty) !=
	// (yj > ty) test rejects — safe to leave the closing pair
	// unconditional.
	inside := false
	j := n - 1
	for i := range n {
		yi := ys[i]
		yj := ys[j]
		if (yi > ty) != (yj > ty) {
			xi := xs[i]
			xj := xs[j]
			xIntersect := (xj-xi)*(ty-yi)/(yj-yi) + xi
			if tx < xIntersect {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

// PIPPolygonFromRings tests point-in-polygon over a full polygon
// represented as an ordered slice of PointsViews: rings[0] is the
// exterior ring; rings[1:] are holes. Returns true iff (tx, ty)
// lies inside the exterior and outside every hole.
//
// Semantics match Polygon.Contains on the AoS representation. Zero
// allocation. Callers holding a Polygon can obtain the RingViews
// once via `polygon.RingViews()` (Slice 1 amortization) and reuse
// the slice across many candidate points — the common pattern in
// spatial-join refine loops.
//
// Empty rings input returns false.
func PIPPolygonFromRings(rings []PointsView, tx, ty float64) bool {
	if len(rings) == 0 {
		return false
	}
	// Exterior test: fail fast when the point isn't inside the
	// outer boundary. Skips the hole tests entirely for the
	// common outside-the-polygon case.
	if !PIPRingFromXY(rings[0].Xs, rings[0].Ys, tx, ty) {
		return false
	}
	// Hole tests: any hole containing the point disqualifies.
	for i := 1; i < len(rings); i++ {
		if PIPRingFromXY(rings[i].Xs, rings[i].Ys, tx, ty) {
			return false
		}
	}
	return true
}

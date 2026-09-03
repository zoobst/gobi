package geometry

// Sutherland-Hodgman convex clipping.
//
// Fast-path substitute for the Martinez-Rueda sweep on the specific case
// of INTERSECTION between two convex, single-ring polygons. The
// algorithm is O(n + m) where n and m are vertex counts, versus
// O((n+m) log (n+m)) for the sweep, and it doesn't allocate an event
// queue or status structure — a hard win on the M-cell inner loop of a
// typical clip pipeline where both operands are convex (bounding boxes,
// tiles, discs approximated with a few dozen vertices).

// IsConvex reports whether p is a single-ring polygon whose vertices
// wind consistently (all left-turns or all right-turns). Polygons with
// holes always return false (a hole makes the polygon non-convex).
// Zero-area rings return false.
func (p Polygon) IsConvex() bool {
	if len(p.Rings) != 1 {
		return false
	}
	return ringIsConvex(p.Rings[0])
}

// ringIsConvex reports whether ring's consecutive edges all turn the
// same direction. Assumes the ring may or may not be closed (first ==
// last vertex); handles both.
func ringIsConvex(ring []Point) bool {
	if len(ring) < 3 {
		return false
	}
	// Strip trailing closing vertex if present so all-pairs indexing is
	// modular.
	n := len(ring)
	if n >= 2 && ring[0].X == ring[n-1].X && ring[0].Y == ring[n-1].Y {
		n--
	}
	if n < 3 {
		return false
	}
	sign := 0
	for i := range n {
		a := ring[i]
		b := ring[(i+1)%n]
		c := ring[(i+2)%n]
		cross := (b.X-a.X)*(c.Y-b.Y) - (b.Y-a.Y)*(c.X-b.X)
		if cross > 0 {
			if sign < 0 {
				return false
			}
			sign = 1
		} else if cross < 0 {
			if sign > 0 {
				return false
			}
			sign = -1
		}
		// cross == 0 means collinear — doesn't break convexity.
	}
	return sign != 0
}

// sutherlandHodgman clips subject against convex clip. Both inputs must
// be single-ring polygons; clip must be convex. Returns the intersection
// as a single-ring Polygon, or a zero-Ring Polygon if the result is
// empty. Output CRS is set from crs.
func sutherlandHodgman(subject, clip []Point, crs CRS) Polygon {
	if len(subject) < 3 || len(clip) < 3 {
		return Polygon{CRSValue: crs}
	}
	subject = openRing(subject)
	clip = openRing(clip)
	// Determine clip winding once. For CCW, the interior is to the LEFT
	// of each directed edge; for CW, to the right.
	clipCCW := ringSignedArea(clip) > 0

	output := append([]Point(nil), subject...)
	scratch := make([]Point, 0, len(subject)+len(clip))

	for i := range len(clip) {
		if len(output) == 0 {
			break
		}
		edgeStart := clip[i]
		edgeEnd := clip[(i+1)%len(clip)]
		scratch = scratch[:0]
		S := output[len(output)-1]
		sInside := insideClipEdge(edgeStart, edgeEnd, S, clipCCW)
		for _, E := range output {
			eInside := insideClipEdge(edgeStart, edgeEnd, E, clipCCW)
			switch {
			case eInside && sInside:
				scratch = append(scratch, E)
			case eInside && !sInside:
				scratch = append(scratch, lineIntersect(edgeStart, edgeEnd, S, E))
				scratch = append(scratch, E)
			case !eInside && sInside:
				scratch = append(scratch, lineIntersect(edgeStart, edgeEnd, S, E))
			}
			S = E
			sInside = eInside
		}
		// Swap output ← scratch; keep scratch's capacity for the next edge.
		output, scratch = scratch, output
	}
	if len(output) < 3 {
		return Polygon{CRSValue: crs}
	}
	// Close the ring, matching gobi's Polygon convention.
	closed := make([]Point, len(output)+1)
	copy(closed, output)
	closed[len(output)] = output[0]
	return Polygon{Rings: [][]Point{closed}, CRSValue: crs}
}

// boundsInsideBounds reports whether inner is fully contained by
// outer. Used by the Slice-18 convex-containment fast path in
// Boolean(): if the subject bbox is inside the convex clipper's
// bbox, the (expensive) per-vertex containment check runs; if
// not, the fast path bails immediately.
func boundsInsideBounds(inner, outer Bounds) bool {
	if inner.Empty() || outer.Empty() {
		return false
	}
	return outer.MinX <= inner.MinX && outer.MinY <= inner.MinY &&
		outer.MaxX >= inner.MaxX && outer.MaxY >= inner.MaxY
}

// allVerticesInsideConvexRing reports whether every point in pts
// lies inside (or on the boundary of) the convex clip ring. Clip
// must be convex per the caller's IsConvex check. Determines
// clip winding once via ringSignedArea, then runs the standard
// signed-cross-product test against each clip edge. Early exits
// on the first outside vertex.
//
// Reads directly off the []Point slabs — no Point allocations or
// Bounds materialization per vertex.
func allVerticesInsideConvexRing(pts []Point, clipRing []Point) bool {
	clip := openRing(clipRing)
	if len(clip) < 3 {
		return false
	}
	ccw := ringSignedArea(clip) > 0
	for _, p := range pts {
		if !pointInsideConvexRing(p, clip, ccw) {
			return false
		}
	}
	return true
}

// intersectionSimplyConnected reports whether the intersection
// of a simple subject ring with a convex clip ring has exactly
// one connected component. Necessary+sufficient condition for
// Sutherland-Hodgman correctness on a convex clipper × concave
// subject (Slice 19).
//
// # The math
//
// For a simple closed subject S and a convex clipper C:
//
//	number of components of (S ∩ C) = transitions / 2
//
// where `transitions` counts subject-vertex-inside-status flips
// as we walk S's boundary once. Convexity of C guarantees that
// any straight subject edge crosses C's boundary at most twice,
// and when both endpoints of an edge are inside a convex set
// the entire edge is inside — so no "hidden crossings" between
// same-status vertices. That collapses the general case
// (edge-based crossing count) to a simple vertex-based one.
//
// Cases:
//
//   - `transitions == 0` AND `allInside` → subject ⊆ C, single
//     component (matches the Slice-18 containment path; still
//     returned safe here for symmetry).
//   - `transitions == 0` AND `!allInside` → subject entirely
//     outside C; components = 0 iff C is also outside subject.
//     Not safe for SH (SH assumes non-empty intersection).
//   - `transitions == 2` → one enter + one exit → 1 component,
//     SH safe.
//   - `transitions >= 4` → multi-component, SH degenerate — fall
//     back to sweep.
//
// Returns (allInside, safe). `allInside` lets the caller skip
// SH entirely for the fully-contained case (return subject).
// Early-exits at transitions=4 to avoid the full ring walk for
// clearly-unsafe inputs.
func intersectionSimplyConnected(subject []Point, clip []Point, ccw bool) (allInside, safe bool) {
	sub := openRing(subject)
	n := len(sub)
	if n < 3 {
		return false, false
	}
	prevInside := pointInsideConvexRing(sub[n-1], clip, ccw)
	insideCount := 0
	transitions := 0
	if prevInside {
		insideCount = 1
	}
	for i := range n {
		inside := pointInsideConvexRing(sub[i], clip, ccw)
		if inside {
			insideCount++
		}
		if inside != prevInside {
			transitions++
			if transitions > 2 {
				return false, false
			}
		}
		prevInside = inside
	}
	// The closing wrap already accounted for above (started walk
	// from the last vertex, so the "closing edge" transition is
	// the first-iteration comparison).
	allInside = insideCount == n
	// Safe when either fully contained (transitions == 0) OR a
	// single enter/exit pair (transitions == 2).
	safe = transitions == 0 || transitions == 2
	// Additional guard for the transitions==0 all-outside case:
	// we don't have a non-empty intersection to feed SH; caller
	// must not use SH there. Fold into allInside check.
	if transitions == 0 && !allInside {
		safe = false
	}
	return allInside, safe
}

// pointInsideConvexRing reports whether p lies on the interior
// (or boundary) of the convex clip ring. Ring winding is provided
// via ccw so the caller can hoist ringSignedArea outside a per-
// vertex loop.
func pointInsideConvexRing(p Point, clip []Point, ccw bool) bool {
	n := len(clip)
	for i := range n {
		a := clip[i]
		b := clip[(i+1)%n]
		cross := (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
		if ccw {
			if cross < 0 {
				return false
			}
		} else {
			if cross > 0 {
				return false
			}
		}
	}
	return true
}

// openRing returns ring without a trailing closing vertex (if present).
// The returned slice may alias the input.
func openRing(ring []Point) []Point {
	n := len(ring)
	if n < 2 {
		return ring
	}
	if ring[0].X == ring[n-1].X && ring[0].Y == ring[n-1].Y {
		return ring[:n-1]
	}
	return ring
}

// ringSignedArea returns the signed shoelace area of an open ring.
// Positive = CCW, negative = CW.
func ringSignedArea(ring []Point) float64 {
	if len(ring) < 3 {
		return 0
	}
	var a float64
	n := len(ring)
	for i := range n {
		j := (i + 1) % n
		a += ring[i].X*ring[j].Y - ring[j].X*ring[i].Y
	}
	return a / 2
}

// insideClipEdge reports whether p lies on the interior side of the
// directed edge a → b, where "interior" is to the LEFT when the clip
// polygon is CCW and to the RIGHT when CW. Points exactly on the edge
// are considered inside — matches Sutherland-Hodgman's canonical
// treatment and avoids emitting sliver output for shared boundaries.
func insideClipEdge(a, b, p Point, ccw bool) bool {
	cross := (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
	if ccw {
		return cross >= 0
	}
	return cross <= 0
}

// lineIntersect returns the intersection point of the infinite lines
// through (a,b) and (c,d). Callers guarantee the two lines cross
// (Sutherland-Hodgman ensures this before calling — one endpoint of
// the subject segment is inside the clip edge's half-plane and the
// other outside, so the segments are not parallel to the clip edge).
// If they happen to be parallel due to numerical coincidence, returns
// the midpoint of (c, d) as a safe fallback.
func lineIntersect(a, b, c, d Point) Point {
	dx1 := b.X - a.X
	dy1 := b.Y - a.Y
	dx2 := d.X - c.X
	dy2 := d.Y - c.Y
	denom := dx1*dy2 - dy1*dx2
	if denom == 0 {
		return Point{X: (c.X + d.X) / 2, Y: (c.Y + d.Y) / 2}
	}
	t := ((c.X-a.X)*dy2 - (c.Y-a.Y)*dx2) / denom
	return Point{X: a.X + t*dx1, Y: a.Y + t*dy1}
}

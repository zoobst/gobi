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
	for i := 0; i < n; i++ {
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

	for i := 0; i < len(clip); i++ {
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
	for i := 0; i < n; i++ {
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

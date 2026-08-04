package geometry

// Additional binary spatial predicates beyond the Intersects/Contains/Within
// trio in predicates.go. These match shapely/geopandas semantics as
// closely as gobi's non-topological primitives allow.

// Touches reports whether a and b share at least one boundary point but
// no interior points. Matches shapely's a.touches(b). Empty geometries
// return false.
func Touches(a, b Geometry) bool {
	if a == nil || b == nil {
		return false
	}
	if !a.Bounds().Intersects(b.Bounds()) {
		return false
	}
	if !Intersects(a, b) {
		return false
	}
	// Touch = intersect but no interior overlap. Detect an interior
	// overlap via a proper "any point of a strictly inside b" check.
	if anyVertexStrictlyInside(a, b) || anyVertexStrictlyInside(b, a) {
		return false
	}
	// Also check edge-edge proper crossings, which create interior
	// overlap on both sides of the crossing.
	if anyEdgeProperlyCrosses(a, b) {
		return false
	}
	return true
}

// Overlaps reports whether a and b share interior points but neither
// contains the other, and both are of the same dimension (both areas or
// both lines). Matches shapely's a.overlaps(b).
func Overlaps(a, b Geometry) bool {
	if a == nil || b == nil {
		return false
	}
	if !sameDimension(a, b) {
		return false
	}
	if !Intersects(a, b) {
		return false
	}
	if Contains(a, b) || Contains(b, a) {
		return false
	}
	// At this point they intersect but neither contains the other and
	// they're of the same dimension → they must overlap in the interior.
	// Distinguish from Touches: at least one vertex of a is strictly
	// inside b, or vice versa, or edges properly cross.
	if anyVertexStrictlyInside(a, b) || anyVertexStrictlyInside(b, a) {
		return true
	}
	if anyEdgeProperlyCrosses(a, b) {
		return true
	}
	return false
}

// Crosses reports whether a and b have some interior points in common
// but not all — typically LineString vs Polygon or LineString vs
// LineString. Matches shapely's a.crosses(b).
func Crosses(a, b Geometry) bool {
	if a == nil || b == nil {
		return false
	}
	if !Intersects(a, b) {
		return false
	}
	if Contains(a, b) || Contains(b, a) {
		return false
	}
	// Crosses requires mixed dimensions (line vs polygon, line vs line
	// that don't lie along each other, etc.). If both are the same
	// dimension AND both are polygonal, this is an Overlap, not a Cross.
	da, db := dimension(a), dimension(b)
	if da == 2 && db == 2 {
		return false
	}
	// Cross fires when edges properly intersect or a vertex sits in
	// the interior of the other.
	if anyEdgeProperlyCrosses(a, b) {
		return true
	}
	if anyVertexStrictlyInside(a, b) || anyVertexStrictlyInside(b, a) {
		return true
	}
	return false
}

// dimension returns 0 for Points/MultiPoint, 1 for LineString/
// MultiLineString, 2 for Polygon/MultiPolygon. GeometryCollection
// returns the max dimension of its members; empty collection returns -1.
func dimension(g Geometry) int {
	switch t := g.(type) {
	case Point, MultiPoint:
		return 0
	case LineString, MultiLineString:
		return 1
	case Polygon, MultiPolygon:
		return 2
	case GeometryCollection:
		best := -1
		for _, inner := range t.Geometries {
			if d := dimension(inner); d > best {
				best = d
			}
		}
		return best
	}
	return -1
}

func sameDimension(a, b Geometry) bool { return dimension(a) == dimension(b) }

// anyVertexStrictlyInside reports whether any vertex of a — or, for
// polygonal a with edges, any midpoint of an edge of a — lies strictly
// in the interior of b. The edge-midpoint fallback catches the case
// where two aligned polygons share a boundary but overlap in the
// interior (e.g. axis-aligned rectangles offset along one axis by less
// than a side length: every vertex of the smaller sits on the larger's
// boundary but the interiors clearly overlap).
func anyVertexStrictlyInside(a, b Geometry) bool {
	if dimension(b) != 2 {
		return false
	}
	found := false
	forEachVertex(a, func(p Point) {
		if found {
			return
		}
		if isStrictlyInside(p, b) {
			found = true
		}
	})
	if found {
		return true
	}
	forEachSegment(a, func(s0, s1 Point) {
		if found {
			return
		}
		mid := Point{X: (s0.X + s1.X) / 2, Y: (s0.Y + s1.Y) / 2}
		if isStrictlyInside(mid, b) {
			found = true
		}
	})
	return found
}

// isStrictlyInside reports whether p is strictly inside polygonal g
// (not on its boundary). Uses Contains + a boundary check via
// point-on-segment scan.
func isStrictlyInside(p Point, g Geometry) bool {
	switch t := g.(type) {
	case Polygon:
		if !t.Contains(p) {
			return false
		}
		return !pointOnPolygonBoundary(p, t)
	case MultiPolygon:
		for _, poly := range t.Polygons {
			if poly.Contains(p) && !pointOnPolygonBoundary(p, poly) {
				return true
			}
		}
		return false
	}
	return false
}

// anyEdgeProperlyCrosses reports whether any edge of a properly crosses
// an edge of b — i.e. the two segments intersect at an interior point
// of both (not at a shared endpoint).
func anyEdgeProperlyCrosses(a, b Geometry) bool {
	found := false
	forEachSegment(a, func(a0, a1 Point) {
		if found {
			return
		}
		forEachSegment(b, func(b0, b1 Point) {
			if found {
				return
			}
			if segmentsProperlyCross(a0, a1, b0, b1) {
				found = true
			}
		})
	})
	return found
}

// segmentsProperlyCross reports whether closed segments (a0,a1) and
// (b0,b1) share exactly one point strictly interior to BOTH. Endpoint
// coincidences (touch at a shared vertex) are NOT proper crossings.
func segmentsProperlyCross(a0, a1, b0, b1 Point) bool {
	o1 := orient(a0, a1, b0)
	o2 := orient(a0, a1, b1)
	o3 := orient(b0, b1, a0)
	o4 := orient(b0, b1, a1)
	// All four orient results must be nonzero AND o1 != o2 AND o3 != o4.
	// Zero means one segment's endpoint lies on the other's supporting
	// line — treat that as a touch, not a proper crossing.
	if o1 == 0 || o2 == 0 || o3 == 0 || o4 == 0 {
		return false
	}
	return o1 != o2 && o3 != o4
}

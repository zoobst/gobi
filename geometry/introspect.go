package geometry

// IsEmpty reports whether g has no coordinates. Matches shapely's
// .is_empty semantics: a Point with the default (0,0) is NOT empty
// (shapely's convention), but a Polygon with no rings, a LineString
// with fewer than 2 points, or a MultiPolygon with no components IS.
func IsEmpty(g Geometry) bool {
	if g == nil {
		return true
	}
	switch t := g.(type) {
	case Point:
		return false
	case MultiPoint:
		return len(t.Points) == 0
	case LineString:
		return len(t.Points) < 2
	case MultiLineString:
		if len(t.Lines) == 0 {
			return true
		}
		for _, l := range t.Lines {
			if len(l.Points) >= 2 {
				return false
			}
		}
		return true
	case Polygon:
		return len(t.Rings) == 0 || len(t.Rings[0]) < 3
	case MultiPolygon:
		if len(t.Polygons) == 0 {
			return true
		}
		for _, p := range t.Polygons {
			if len(p.Rings) > 0 && len(p.Rings[0]) >= 3 {
				return false
			}
		}
		return true
	case GeometryCollection:
		if len(t.Geometries) == 0 {
			return true
		}
		for _, inner := range t.Geometries {
			if !IsEmpty(inner) {
				return false
			}
		}
		return true
	}
	return true
}

// IsValid reports whether g satisfies the OGC Simple Features validity
// rules gobi checks:
//
//   - LineString: >= 2 points, no consecutive duplicate vertices.
//   - Polygon: every ring closed (or auto-closable), >= 3 unique
//     vertices per ring, no ring self-intersection.
//   - Multi types: all components valid.
//   - Point / MultiPoint: always valid.
//
// This is deliberately a subset of GEOS's IsValid — full OGC validity
// includes checks like "holes lie inside exterior" and "rings don't
// touch except at points" which require more machinery than gobi
// currently exposes. Returns false for structurally-broken input;
// callers can rely on IsValid == true meaning "safe to pass to
// Boolean / Buffer without triggering degenerate-input paths."
func IsValid(g Geometry) bool {
	if g == nil {
		return false
	}
	switch t := g.(type) {
	case Point:
		return true
	case MultiPoint:
		return true
	case LineString:
		return validLineString(t.Points)
	case MultiLineString:
		for _, l := range t.Lines {
			if !validLineString(l.Points) {
				return false
			}
		}
		return true
	case Polygon:
		for _, r := range t.Rings {
			if !validRing(r) {
				return false
			}
		}
		return true
	case MultiPolygon:
		for _, p := range t.Polygons {
			if !IsValid(p) {
				return false
			}
		}
		return true
	case GeometryCollection:
		for _, inner := range t.Geometries {
			if !IsValid(inner) {
				return false
			}
		}
		return true
	}
	return false
}

func validLineString(pts []Point) bool {
	if len(pts) < 2 {
		return false
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].X == pts[i-1].X && pts[i].Y == pts[i-1].Y {
			return false // consecutive duplicate
		}
	}
	return true
}

func validRing(ring []Point) bool {
	if len(ring) < 3 {
		return false
	}
	closed := closedRing(ring)
	// Require enough unique vertices for a non-degenerate ring.
	unique := 0
	for i := range len(closed) - 1 {
		if i+1 < len(closed)-1 && closed[i].X == closed[i+1].X && closed[i].Y == closed[i+1].Y {
			continue
		}
		unique++
	}
	if unique < 3 {
		return false
	}
	// Self-intersection: any non-adjacent edge pair that crosses is
	// invalid. O(n²) but only runs when the caller explicitly asks;
	// typical Dissolve/Buffer outputs have simple rings.
	n := len(closed) - 1
	for i := range n {
		a0, a1 := closed[i], closed[i+1]
		// Skip adjacent edges (share a vertex).
		for j := i + 2; j < n; j++ {
			if i == 0 && j == n-1 {
				continue // wrap-around adjacent
			}
			b0, b1 := closed[j], closed[j+1]
			if segmentsProperlyCross(a0, a1, b0, b1) {
				return false
			}
		}
	}
	return true
}

// TypeString returns the OGC-style name for g's concrete type
// ("Point", "MultiPolygon", etc.). Matches shapely's .geom_type
// output.
func TypeString(g Geometry) string {
	if g == nil {
		return ""
	}
	return g.Type().String()
}

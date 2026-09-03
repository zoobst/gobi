package geometry

import "math"

// WithinDistance reports whether any two points in a and b are at
// most d coordinate units apart. Equivalent to
// `GeomDistance(a, b) <= d` but with a bbox-distance short-circuit
// that lets far-apart pairs return false without walking edges.
//
// d must be non-negative; d = 0 is equivalent to Intersects(a, b).
// Nil operands or NaN d return false. Coordinates are treated as
// planar — for lon/lat inputs, project to a suitable CRS first
// (Haversine + a per-row loop covers the geographic case).
//
// The bbox short-circuit computes the minimum distance between
// the two bounding rectangles: if that's already > d, no interior
// point pair could be closer. This is what makes DWithin's row-
// group pushdown pay off — a row whose bbox is far from an AOI
// bbox never gets its WKB decoded.
func WithinDistance(a, b Geometry, d float64) bool {
	if a == nil || b == nil {
		return false
	}
	if math.IsNaN(d) || d < 0 {
		return false
	}
	// d == 0 is exactly Intersects — take the direct route rather
	// than paying the bbox-min-distance + planarMinDistance overhead
	// for what's really a boundary-share test.
	if d == 0 {
		return Intersects(a, b)
	}
	// Bbox short-circuit: min bbox-to-bbox distance is a lower bound
	// on min geometry-to-geometry distance. If it exceeds d, no pair
	// of points can be closer than d.
	if bboxMinDistance(a.Bounds(), b.Bounds()) > d {
		return false
	}
	if Intersects(a, b) {
		return true
	}
	return planarMinDistance(a, b) <= d
}

// BoundsMinDistance returns the minimum Euclidean distance between
// two axis-aligned bounding rectangles. Zero when they overlap or
// touch. Empty bounds → +Inf.
//
// Used by WithinDistance's short-circuit; also exposed publicly
// so per-row `Series.GeomDWithin` callers can reject far rows via
// `BoundsFromWKB` + this helper without a full ParseWKB.
func BoundsMinDistance(a, b Bounds) float64 { return bboxMinDistance(a, b) }

// bboxMinDistance returns the minimum Euclidean distance between
// two axis-aligned bounding rectangles. Zero when they overlap or
// touch. Empty bounds → +Inf (a defensive value that makes the
// caller take the conservative branch).
func bboxMinDistance(a, b Bounds) float64 {
	if a.Empty() || b.Empty() {
		return math.Inf(1)
	}
	var dx, dy float64
	switch {
	case a.MaxX < b.MinX:
		dx = b.MinX - a.MaxX
	case b.MaxX < a.MinX:
		dx = a.MinX - b.MaxX
	}
	switch {
	case a.MaxY < b.MinY:
		dy = b.MinY - a.MaxY
	case b.MaxY < a.MinY:
		dy = a.MinY - b.MaxY
	}
	return math.Hypot(dx, dy)
}

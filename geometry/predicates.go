package geometry

// Predicate names a binary spatial predicate.
type Predicate uint8

const (
	// PredIntersects: a and b share any point.
	PredIntersects Predicate = iota
	// PredContains: a fully contains b (every point of b lies in a).
	PredContains
	// PredWithin: a lies fully within b (equivalent to Contains(b, a)).
	PredWithin
)

func (p Predicate) String() string {
	switch p {
	case PredIntersects:
		return "intersects"
	case PredContains:
		return "contains"
	case PredWithin:
		return "within"
	default:
		return "unknown"
	}
}

// Test evaluates pred on the ordered pair (a, b).
func Test(pred Predicate, a, b Geometry) bool {
	switch pred {
	case PredIntersects:
		return Intersects(a, b)
	case PredContains:
		return Contains(a, b)
	case PredWithin:
		return Contains(b, a)
	default:
		return false
	}
}

// Intersects reports whether a and b share any point. The relation is
// symmetric.
func Intersects(a, b Geometry) bool {
	if a == nil || b == nil {
		return false
	}
	// Cheap bbox reject.
	if !a.Bounds().Intersects(b.Bounds()) {
		return false
	}
	return intersects(a, b)
}

// Contains reports whether a fully contains b.
func Contains(a, b Geometry) bool {
	if a == nil || b == nil {
		return false
	}
	// a's bbox must cover b's bbox (necessary condition for containment).
	ab, bb := a.Bounds(), b.Bounds()
	if bb.Empty() {
		return false
	}
	if ab.MinX > bb.MinX || ab.MinY > bb.MinY ||
		ab.MaxX < bb.MaxX || ab.MaxY < bb.MaxY {
		return false
	}
	return contains(a, b)
}

// Within is Contains(b, a).
func Within(a, b Geometry) bool { return Contains(b, a) }

// ---------- Intersects dispatch ----------

func intersects(a, b Geometry) bool {
	switch t := a.(type) {
	case Point:
		return intersectsPoint(t, b)
	case LineString:
		return intersectsLineString(t, b)
	case Polygon:
		return intersectsPolygon(t, b)
	case MultiPoint:
		for _, p := range t.Points {
			if intersects(p, b) {
				return true
			}
		}
		return false
	case MultiLineString:
		for _, l := range t.Lines {
			if intersects(l, b) {
				return true
			}
		}
		return false
	case MultiPolygon:
		for _, p := range t.Polygons {
			if intersects(p, b) {
				return true
			}
		}
		return false
	case GeometryCollection:
		for _, g := range t.Geometries {
			if intersects(g, b) {
				return true
			}
		}
		return false
	}
	return false
}

func intersectsPoint(p Point, o Geometry) bool {
	switch t := o.(type) {
	case Point:
		return p.X == t.X && p.Y == t.Y
	case LineString:
		return pointOnLineString(p, t)
	case Polygon:
		return pointInPolygon(p, t)
	case MultiPoint:
		for _, q := range t.Points {
			if p.X == q.X && p.Y == q.Y {
				return true
			}
		}
		return false
	case MultiLineString:
		for _, l := range t.Lines {
			if pointOnLineString(p, l) {
				return true
			}
		}
		return false
	case MultiPolygon:
		for _, poly := range t.Polygons {
			if pointInPolygon(p, poly) {
				return true
			}
		}
		return false
	case GeometryCollection:
		for _, g := range t.Geometries {
			if intersects(p, g) {
				return true
			}
		}
		return false
	}
	return false
}

func intersectsLineString(l LineString, o Geometry) bool {
	switch t := o.(type) {
	case Point:
		return pointOnLineString(t, l)
	case LineString:
		return lineStringsIntersect(l, t)
	case Polygon:
		return lineIntersectsPolygon(l, t)
	case MultiPoint:
		for _, p := range t.Points {
			if pointOnLineString(p, l) {
				return true
			}
		}
		return false
	case MultiLineString:
		for _, other := range t.Lines {
			if lineStringsIntersect(l, other) {
				return true
			}
		}
		return false
	case MultiPolygon:
		for _, p := range t.Polygons {
			if lineIntersectsPolygon(l, p) {
				return true
			}
		}
		return false
	case GeometryCollection:
		for _, g := range t.Geometries {
			if intersects(l, g) {
				return true
			}
		}
		return false
	}
	return false
}

func intersectsPolygon(p Polygon, o Geometry) bool {
	switch t := o.(type) {
	case Point:
		return pointInPolygon(t, p)
	case LineString:
		return lineIntersectsPolygon(t, p)
	case Polygon:
		return polygonsIntersect(p, t)
	case MultiPoint:
		for _, q := range t.Points {
			if pointInPolygon(q, p) {
				return true
			}
		}
		return false
	case MultiLineString:
		for _, l := range t.Lines {
			if lineIntersectsPolygon(l, p) {
				return true
			}
		}
		return false
	case MultiPolygon:
		for _, other := range t.Polygons {
			if polygonsIntersect(p, other) {
				return true
			}
		}
		return false
	case GeometryCollection:
		for _, g := range t.Geometries {
			if intersects(p, g) {
				return true
			}
		}
		return false
	}
	return false
}

// ---------- Contains dispatch ----------
//
// Contains is asymmetric. Multi* on the LEFT side matches if any single
// component contains b; Multi* on the RIGHT side matches only if every
// component of b is contained.

func contains(a, b Geometry) bool {
	// b is Multi/Collection ⇒ all components of b must be contained by a.
	switch tb := b.(type) {
	case MultiPoint:
		for _, p := range tb.Points {
			if !contains(a, p) {
				return false
			}
		}
		return len(tb.Points) > 0
	case MultiLineString:
		for _, l := range tb.Lines {
			if !contains(a, l) {
				return false
			}
		}
		return len(tb.Lines) > 0
	case MultiPolygon:
		for _, p := range tb.Polygons {
			if !contains(a, p) {
				return false
			}
		}
		return len(tb.Polygons) > 0
	case GeometryCollection:
		for _, g := range tb.Geometries {
			if !contains(a, g) {
				return false
			}
		}
		return len(tb.Geometries) > 0
	}
	// a is Multi/Collection ⇒ any component of a may contain b.
	switch ta := a.(type) {
	case MultiPoint:
		for _, p := range ta.Points {
			if contains(p, b) {
				return true
			}
		}
		return false
	case MultiLineString:
		for _, l := range ta.Lines {
			if contains(l, b) {
				return true
			}
		}
		return false
	case MultiPolygon:
		for _, p := range ta.Polygons {
			if contains(p, b) {
				return true
			}
		}
		return false
	case GeometryCollection:
		for _, g := range ta.Geometries {
			if contains(g, b) {
				return true
			}
		}
		return false
	case Point:
		bp, ok := b.(Point)
		return ok && ta.X == bp.X && ta.Y == bp.Y
	case LineString:
		return lineContains(ta, b)
	case Polygon:
		return polygonContains(ta, b)
	}
	return false
}

// lineContains reports whether every point of b lies on l. Only b = Point or
// LineString is meaningful; a line cannot contain a polygon.
func lineContains(l LineString, b Geometry) bool {
	switch t := b.(type) {
	case Point:
		return pointOnLineString(t, l)
	case LineString:
		for _, p := range t.Points {
			if !pointOnLineString(p, l) {
				return false
			}
		}
		return len(t.Points) > 0
	}
	return false
}

// polygonContains reports whether every point of b lies in p. Holes are
// respected via Polygon.Contains(Point).
func polygonContains(p Polygon, b Geometry) bool {
	switch t := b.(type) {
	case Point:
		return p.Contains(t) || pointOnPolygonBoundary(t, p)
	case LineString:
		// Every vertex must lie in p (including boundary), and no edge of
		// the line may cross a ring edge.
		for _, pt := range t.Points {
			if !p.Contains(pt) && !pointOnPolygonBoundary(pt, p) {
				return false
			}
		}
		for i := 0; i < len(t.Points)-1; i++ {
			if lineSegmentCrossesPolygonBoundary(t.Points[i], t.Points[i+1], p) {
				return false
			}
		}
		return len(t.Points) > 0
	case Polygon:
		// Every vertex of b.exterior must lie in p (or on p's boundary),
		// and no exterior edge of b may cross an edge of p.
		ext := t.Exterior()
		if len(ext) == 0 {
			return false
		}
		for _, pt := range ext {
			if !p.Contains(pt) && !pointOnPolygonBoundary(pt, p) {
				return false
			}
		}
		ring := closedRing(ext)
		for i := 0; i < len(ring)-1; i++ {
			if lineSegmentCrossesPolygonBoundary(ring[i], ring[i+1], p) {
				return false
			}
		}
		return true
	}
	return false
}

// ---------- kernel primitives ----------

// pointOnLineString reports whether p lies on any segment of l (closed
// segments, so endpoints count).
func pointOnLineString(p Point, l LineString) bool {
	for i := 0; i < len(l.Points)-1; i++ {
		if pointOnSegment(p, l.Points[i], l.Points[i+1]) {
			return true
		}
	}
	return false
}

// pointOnSegment reports whether p lies on the closed segment ab.
func pointOnSegment(p, a, b Point) bool {
	if cross(a, b, p) != 0 {
		return false
	}
	return onSegmentBBox(a, p, b)
}

// onSegmentBBox reports whether q lies within the axis-aligned bbox of pr.
// Assumes p, q, r are collinear.
func onSegmentBBox(p, q, r Point) bool {
	if q.X < min(p.X, r.X) || q.X > max(p.X, r.X) {
		return false
	}
	if q.Y < min(p.Y, r.Y) || q.Y > max(p.Y, r.Y) {
		return false
	}
	return true
}

// segmentsIntersect reports whether closed segments (p1,p2) and (q1,q2)
// share any point, including endpoints and collinear overlap.
func segmentsIntersect(p1, p2, q1, q2 Point) bool {
	o1 := orient(p1, p2, q1)
	o2 := orient(p1, p2, q2)
	o3 := orient(q1, q2, p1)
	o4 := orient(q1, q2, p2)

	// Proper crossing.
	if o1 != o2 && o3 != o4 {
		return true
	}
	// Collinear touches.
	if o1 == 0 && onSegmentBBox(p1, q1, p2) {
		return true
	}
	if o2 == 0 && onSegmentBBox(p1, q2, p2) {
		return true
	}
	if o3 == 0 && onSegmentBBox(q1, p1, q2) {
		return true
	}
	if o4 == 0 && onSegmentBBox(q1, p2, q2) {
		return true
	}
	return false
}

// orient returns the sign of the cross product (b-a) x (c-a):
// +1 for CCW, -1 for CW, 0 for collinear.
func orient(a, b, c Point) int {
	v := cross(a, b, c)
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

// lineStringsIntersect reports whether any segment of a shares any point
// with any segment of b.
func lineStringsIntersect(a, b LineString) bool {
	for i := 0; i < len(a.Points)-1; i++ {
		for j := 0; j < len(b.Points)-1; j++ {
			if segmentsIntersect(a.Points[i], a.Points[i+1], b.Points[j], b.Points[j+1]) {
				return true
			}
		}
	}
	return false
}

// lineIntersectsPolygon reports whether l shares any point with p (either
// crosses a ring edge or lies inside p).
func lineIntersectsPolygon(l LineString, p Polygon) bool {
	// Point-in-polygon on any line vertex.
	for _, pt := range l.Points {
		if pointInPolygon(pt, p) {
			return true
		}
	}
	// Crossings against every ring edge (exterior + holes).
	for _, ring := range p.Rings {
		r := closedRing(ring)
		for j := 0; j < len(r)-1; j++ {
			for i := 0; i < len(l.Points)-1; i++ {
				if segmentsIntersect(l.Points[i], l.Points[i+1], r[j], r[j+1]) {
					return true
				}
			}
		}
	}
	return false
}

// pointInPolygon reports whether pt is inside p (respecting holes). Points
// on the boundary are considered inside.
func pointInPolygon(pt Point, p Polygon) bool {
	if p.Contains(pt) {
		return true
	}
	return pointOnPolygonBoundary(pt, p)
}

// pointOnPolygonBoundary reports whether pt lies exactly on any of p's ring
// edges.
func pointOnPolygonBoundary(pt Point, p Polygon) bool {
	for _, ring := range p.Rings {
		r := closedRing(ring)
		for i := 0; i < len(r)-1; i++ {
			if pointOnSegment(pt, r[i], r[i+1]) {
				return true
			}
		}
	}
	return false
}

// polygonsIntersect reports whether a and b share any point.
func polygonsIntersect(a, b Polygon) bool {
	// Any vertex of one inside the other?
	if ext := a.Exterior(); len(ext) > 0 && pointInPolygon(ext[0], b) {
		return true
	}
	if ext := b.Exterior(); len(ext) > 0 && pointInPolygon(ext[0], a) {
		return true
	}
	// Any ring edge crossings?
	for _, ra := range a.Rings {
		rA := closedRing(ra)
		for _, rb := range b.Rings {
			rB := closedRing(rb)
			for i := 0; i < len(rA)-1; i++ {
				for j := 0; j < len(rB)-1; j++ {
					if segmentsIntersect(rA[i], rA[i+1], rB[j], rB[j+1]) {
						return true
					}
				}
			}
		}
	}
	return false
}

// lineSegmentCrossesPolygonBoundary reports whether segment ab properly
// crosses any ring edge of p (touching at a vertex or overlapping an edge
// segment does NOT count).
func lineSegmentCrossesPolygonBoundary(a, b Point, p Polygon) bool {
	for _, ring := range p.Rings {
		r := closedRing(ring)
		for i := 0; i < len(r)-1; i++ {
			o1 := orient(a, b, r[i])
			o2 := orient(a, b, r[i+1])
			o3 := orient(r[i], r[i+1], a)
			o4 := orient(r[i], r[i+1], b)
			if o1 != o2 && o3 != o4 && o1 != 0 && o2 != 0 && o3 != 0 && o4 != 0 {
				return true
			}
		}
	}
	return false
}

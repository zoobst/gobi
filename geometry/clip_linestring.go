package geometry

import "sort"

// Clip returns the sub-LineStrings of l that lie inside p (inclusive of
// the polygon boundary). Fragments preserve order along l: fragment i
// occurs before fragment i+1 when walking l from its first to last
// vertex.
//
// Contract:
//   - l fully inside p                 -> []LineString{l}
//   - l fully outside p                -> nil
//   - l touches p at a single vertex   -> nil (zero-length overlap dropped)
//   - l coincident with a p edge       -> the coincident sub-segment is
//     returned as one fragment (convex path only; the concave/holed path
//     treats boundary-coincident sub-segments as unspecified — a follow-up
//     will tighten this)
//
// Coordinate system: planar (WGS84 lon/lat treated as x/y). Callers that
// need geodesic accuracy should densify l first. Antimeridian handling is
// the caller's responsibility (see SplitAtAntimeridian).
func (l LineString) Clip(p Polygon) []LineString {
	inside, _ := l.splitBy(p, false)
	return inside
}

// SplitBy returns the fragments of l that lie inside p and the fragments
// that lie outside p, in a single pass. Both slices preserve order along
// l. Equivalent to Clip(p) plus a complementary clip, but shares the
// per-segment edge-intersection work.
func (l LineString) SplitBy(p Polygon) (inside, outside []LineString) {
	return l.splitBy(p, true)
}

func (l LineString) splitBy(p Polygon, wantOutside bool) (inside, outside []LineString) {
	if len(l.Points) < 2 || len(p.Rings) == 0 {
		if wantOutside && len(l.Points) >= 2 {
			outside = []LineString{l}
		}
		return
	}
	if !l.Bounds().Intersects(p.Bounds()) {
		if wantOutside {
			outside = []LineString{l}
		}
		return
	}
	if p.IsConvex() {
		return clipLineConvex(l, p.Rings[0], wantOutside)
	}
	return clipLineGeneral(l, p, wantOutside)
}

// Clip returns the sub-LineStrings of m that lie inside p (inclusive of
// the polygon boundary). Fragments preserve two orderings: fragments from
// component i appear before fragments from component i+1, and within each
// component they appear in walk order. See LineString.Clip for the
// per-component edge-case contract.
func (m MultiLineString) Clip(p Polygon) []LineString {
	if len(m.Lines) == 0 || len(p.Rings) == 0 || !m.Bounds().Intersects(p.Bounds()) {
		return nil
	}
	// Each component inherits the container's CRSValue and HasZ, overriding
	// whatever the component carried — same precedent as AppendWKB.
	var inside []LineString
	for _, l := range m.Lines {
		l.CRSValue = m.CRSValue
		l.HasZ = m.HasZ
		inside = append(inside, l.Clip(p)...)
	}
	return inside
}

// SplitBy returns the fragments of m that lie inside p and the fragments
// that lie outside p. Both slices preserve component-and-walk order:
// component i's fragments appear before component i+1's, and within a
// component fragments appear in walk order.
func (m MultiLineString) SplitBy(p Polygon) (inside, outside []LineString) {
	if len(m.Lines) == 0 || len(p.Rings) == 0 || !m.Bounds().Intersects(p.Bounds()) {
		return nil, m.allComponents()
	}
	// Container CRSValue/HasZ overrides per-component values; see Clip.
	for _, l := range m.Lines {
		l.CRSValue = m.CRSValue
		l.HasZ = m.HasZ
		in, out := l.SplitBy(p)
		inside = append(inside, in...)
		outside = append(outside, out...)
	}
	return
}

// allComponents returns every non-degenerate component of m with the
// container's CRS/HasZ applied — used for the "no work needed, hand the
// input back as `outside`" fast paths in splitBy.
func (m MultiLineString) allComponents() []LineString {
	out := make([]LineString, 0, len(m.Lines))
	for _, l := range m.Lines {
		if len(l.Points) < 2 {
			continue
		}
		l.CRSValue = m.CRSValue
		l.HasZ = m.HasZ
		out = append(out, l)
	}
	return out
}

// lineBuilder accumulates a single LineString fragment. It dedupes
// consecutive coincident vertices so fragments stitched across shared
// endpoints don't repeat that vertex. flush emits the fragment (only when
// it has >= 2 distinct points, which is where single-vertex touches get
// dropped) into dst and resets the builder.
type lineBuilder struct {
	pts []Point
}

func (b *lineBuilder) empty() bool { return len(b.pts) == 0 }

func (b *lineBuilder) append(p Point) {
	if n := len(b.pts); n > 0 && b.pts[n-1].X == p.X && b.pts[n-1].Y == p.Y {
		return
	}
	b.pts = append(b.pts, p)
}

func (b *lineBuilder) flush(dst *[]LineString, template LineString) {
	if len(b.pts) >= 2 {
		out := make([]Point, len(b.pts))
		copy(out, b.pts)
		*dst = append(*dst, LineString{Points: out, CRSValue: template.CRSValue, HasZ: template.HasZ})
	}
	b.pts = b.pts[:0]
}

// clipLineConvex clips ls against a convex, single-ring polygon using
// Cyrus-Beck per segment. Handles the coincident-with-edge case correctly:
// a linestring segment lying exactly on a clip edge is emitted as one
// inside fragment because insideClipEdge treats boundary points as inside.
func clipLineConvex(ls LineString, ring []Point, wantOutside bool) (inside, outside []LineString) {
	ring = openRing(ring)
	if len(ring) < 3 {
		return
	}
	ccw := ringSignedArea(ring) > 0
	// Hoist the ring AABB so per-segment reject is four scalar compares
	// instead of the len(ring) Cyrus-Beck half-plane tests. For convex
	// rings, bbox-disjoint implies the segment can't intersect the ring
	// — this short-circuits the majority-outside workloads (long
	// linestrings clipped by small h3 cells) without changing results.
	rMinX, rMinY := ring[0].X, ring[0].Y
	rMaxX, rMaxY := rMinX, rMinY
	for i := 1; i < len(ring); i++ {
		if ring[i].X < rMinX {
			rMinX = ring[i].X
		} else if ring[i].X > rMaxX {
			rMaxX = ring[i].X
		}
		if ring[i].Y < rMinY {
			rMinY = ring[i].Y
		} else if ring[i].Y > rMaxY {
			rMaxY = ring[i].Y
		}
	}

	var curIn, curOut lineBuilder
	pts := ls.Points
	for i := range len(pts) - 1 {
		a, b := pts[i], pts[i+1]
		if a.X == b.X && a.Y == b.Y {
			continue
		}
		sMinX, sMaxX := min(a.X, b.X), max(a.X, b.X)
		sMinY, sMaxY := min(a.Y, b.Y), max(a.Y, b.Y)
		if sMaxX < rMinX || sMinX > rMaxX || sMaxY < rMinY || sMinY > rMaxY {
			curIn.flush(&inside, ls)
			if wantOutside {
				curOut.append(a)
				curOut.append(b)
			}
			continue
		}
		tEnter, tExit, ok := cyrusBeckSegment(a, b, ring, ccw)
		if !ok || tEnter >= tExit {
			curIn.flush(&inside, ls)
			if wantOutside {
				curOut.append(a)
				curOut.append(b)
			}
			continue
		}
		pEnter := lerp(a, b, tEnter)
		pExit := lerp(a, b, tExit)
		if tEnter > 0 {
			if wantOutside {
				curOut.append(a)
				curOut.append(pEnter)
				curOut.flush(&outside, ls)
			}
		} else if wantOutside {
			curOut.flush(&outside, ls)
		}
		if !curIn.empty() && tEnter == 0 {
			curIn.append(pExit)
		} else {
			curIn.flush(&inside, ls)
			curIn.append(pEnter)
			curIn.append(pExit)
		}
		if tExit < 1 {
			curIn.flush(&inside, ls)
			if wantOutside {
				curOut.append(pExit)
				curOut.append(b)
			}
		}
	}
	curIn.flush(&inside, ls)
	if wantOutside {
		curOut.flush(&outside, ls)
	}
	return
}

// cyrusBeckSegment computes the parametric window [tEnter, tExit] where
// segment a->b lies inside the convex ring. ccw is true when the ring
// winds counter-clockwise. Returns ok=false when the segment lies entirely
// outside; a zero-length window (tEnter == tExit) is returned as ok=true
// and the caller drops it as a single-vertex touch.
func cyrusBeckSegment(a, b Point, ring []Point, ccw bool) (tEnter, tExit float64, ok bool) {
	tEnter, tExit = 0, 1
	dx, dy := b.X-a.X, b.Y-a.Y
	for i := range ring {
		e0 := ring[i]
		e1 := ring[(i+1)%len(ring)]
		ex, ey := e1.X-e0.X, e1.Y-e0.Y
		crossA := ex*(a.Y-e0.Y) - ey*(a.X-e0.X)
		crossB := ex*(b.Y-e0.Y) - ey*(b.X-e0.X)
		aIn := (ccw && crossA >= 0) || (!ccw && crossA <= 0)
		bIn := (ccw && crossB >= 0) || (!ccw && crossB <= 0)
		if aIn && bIn {
			continue
		}
		if !aIn && !bIn {
			return 0, 0, false
		}
		denom := ex*dy - ey*dx
		if denom == 0 {
			return 0, 0, false
		}
		t := (ey*(a.X-e0.X) - ex*(a.Y-e0.Y)) / denom
		if aIn {
			tExit = min(tExit, t)
		} else {
			tEnter = max(tEnter, t)
		}
		if tEnter > tExit {
			return 0, 0, false
		}
	}
	return tEnter, tExit, true
}

// clipLineGeneral handles concave polygons and polygons with holes by
// intersecting each linestring segment with every polygon edge, sorting
// the parameter values, and classifying each sub-interval by testing its
// midpoint with Polygon.Contains. O(V*E) per call — correct but not
// tuned; convex polygons take the Cyrus-Beck fast path above.
//
// Known limitation: linestring segments that lie exactly on a polygon
// edge fall on Polygon.Contains's undefined boundary, so their
// classification is unspecified in this path. A follow-up will replace
// this with a proper vertex-marching Weiler-Atherton pass.
func clipLineGeneral(ls LineString, p Polygon, wantOutside bool) (inside, outside []LineString) {
	var curIn, curOut lineBuilder
	pts := ls.Points
	ts := make([]float64, 0, 8)
	for i := range len(pts) - 1 {
		a, b := pts[i], pts[i+1]
		if a.X == b.X && a.Y == b.Y {
			continue
		}
		ts = ts[:0]
		ts = append(ts, 0, 1)
		for _, ring := range p.Rings {
			r := openRing(ring)
			for j := range len(r) {
				e0, e1 := r[j], r[(j+1)%len(r)]
				if t, ok := segSegParam(a, b, e0, e1); ok {
					if t > 0 && t < 1 {
						ts = append(ts, t)
					}
				}
			}
		}
		sort.Float64s(ts)
		for k := 0; k+1 < len(ts); k++ {
			t0, t1 := ts[k], ts[k+1]
			if t1-t0 < 1e-15 {
				continue
			}
			mid := lerp(a, b, (t0+t1)/2)
			p0 := lerp(a, b, t0)
			p1 := lerp(a, b, t1)
			if p.Contains(mid) {
				if curIn.empty() {
					curIn.append(p0)
				}
				curIn.append(p1)
				if wantOutside {
					curOut.flush(&outside, ls)
				}
			} else {
				if wantOutside {
					if curOut.empty() {
						curOut.append(p0)
					}
					curOut.append(p1)
				}
				curIn.flush(&inside, ls)
			}
		}
	}
	curIn.flush(&inside, ls)
	if wantOutside {
		curOut.flush(&outside, ls)
	}
	return
}

// segSegParam returns the parameter t on segment a->b where it crosses
// segment c->d, if the two segments properly intersect in their interiors
// (or share an endpoint). Parallel/collinear segments return ok=false.
func segSegParam(a, b, c, d Point) (float64, bool) {
	dx1, dy1 := b.X-a.X, b.Y-a.Y
	dx2, dy2 := d.X-c.X, d.Y-c.Y
	denom := dx1*dy2 - dy1*dx2
	if denom == 0 {
		return 0, false
	}
	t := ((c.X-a.X)*dy2 - (c.Y-a.Y)*dx2) / denom
	u := ((c.X-a.X)*dy1 - (c.Y-a.Y)*dx1) / denom
	if t < 0 || t > 1 || u < 0 || u > 1 {
		return 0, false
	}
	return t, true
}

// lerp linearly interpolates between a and b at parameter t. Z is
// interpolated only when both endpoints carry Z; otherwise the result is
// 2D with Z zeroed.
func lerp(a, b Point, t float64) Point {
	p := Point{X: a.X + t*(b.X-a.X), Y: a.Y + t*(b.Y-a.Y)}
	if a.HasZ && b.HasZ {
		p.Z = a.Z + t*(b.Z-a.Z)
		p.HasZ = true
	}
	return p
}

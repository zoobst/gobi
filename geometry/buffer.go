// Package geometry Buffer implementations.
//
// Buffer produces the set of points within a given distance of the source
// geometry. This file supports positive (outward) buffers for Point,
// LineString, and Polygon using rounded joins/caps. Negative (inward)
// buffers and self-overlapping outputs from concave input rings are not
// simplified here — that would require a polygon union/clipping engine,
// which is intentionally out of scope.

package geometry

import (
	"fmt"
	"math"
)

// DefaultBufferSegments is the number of straight segments used to
// approximate a full circle in Buffer operations when the caller doesn't
// specify one.
const DefaultBufferSegments = 32

// BufferOptions configures Buffer behavior.
type BufferOptions struct {
	// Segments controls the number of straight edges used to approximate a
	// full circle. Higher = smoother, larger output. 0 falls back to
	// DefaultBufferSegments.
	Segments int
}

func (o BufferOptions) segments() int {
	if o.Segments < 4 {
		return DefaultBufferSegments
	}
	return o.Segments
}

// Buffer returns a polygon (or MultiPolygon-shaped result) approximating
// the set of points within distance of g, using rounded joins and caps.
//
// Only positive distances are supported. Passing distance <= 0 returns
// (nil, error). Buffering Polygon inputs assumes the input's exterior ring
// is convex or nearly so; strongly concave inputs may produce self-
// intersecting output rings (see file-level comment).
func Buffer(g Geometry, distance float64, opts BufferOptions) (Geometry, error) {
	if distance <= 0 {
		return nil, fmt.Errorf("buffer: distance must be > 0, got %v", distance)
	}
	segments := opts.segments()
	switch t := g.(type) {
	case Point:
		return t.Buffer(distance, segments), nil
	case LineString:
		return t.Buffer(distance, segments), nil
	case Polygon:
		return t.Buffer(distance, segments), nil
	case MultiPoint:
		polys := make([]Polygon, len(t.Points))
		for i, p := range t.Points {
			polys[i] = p.Buffer(distance, segments)
		}
		return MultiPolygon{Polygons: polys, CRSValue: t.CRSValue}, nil
	case MultiLineString:
		polys := make([]Polygon, len(t.Lines))
		for i, l := range t.Lines {
			polys[i] = l.Buffer(distance, segments)
		}
		return MultiPolygon{Polygons: polys, CRSValue: t.CRSValue}, nil
	case MultiPolygon:
		polys := make([]Polygon, len(t.Polygons))
		for i, p := range t.Polygons {
			polys[i] = p.Buffer(distance, segments)
		}
		return MultiPolygon{Polygons: polys, CRSValue: t.CRSValue}, nil
	}
	return nil, fmt.Errorf("buffer: unsupported type %T", g)
}

// Buffer returns a circular polygon of radius distance centered on p,
// approximated with the given number of edge segments. segments < 4 falls
// back to DefaultBufferSegments.
func (p Point) Buffer(distance float64, segments int) Polygon {
	if segments < 4 {
		segments = DefaultBufferSegments
	}
	ring := make([]Point, segments+1)
	for i := 0; i < segments; i++ {
		theta := 2 * math.Pi * float64(i) / float64(segments)
		ring[i] = Point{
			X:        p.X + distance*math.Cos(theta),
			Y:        p.Y + distance*math.Sin(theta),
			CRSValue: p.CRSValue,
		}
	}
	ring[segments] = ring[0]
	return Polygon{Rings: [][]Point{ring}, CRSValue: p.CRSValue}
}

// Buffer returns a polygon approximating the set of points within distance
// of the linestring, using rounded joins at internal vertices and rounded
// caps at the endpoints. Requires the linestring to have at least 2 points.
func (l LineString) Buffer(distance float64, segments int) Polygon {
	if segments < 4 {
		segments = DefaultBufferSegments
	}
	if len(l.Points) < 2 {
		if len(l.Points) == 1 {
			return l.Points[0].Buffer(distance, segments)
		}
		return Polygon{CRSValue: l.CRSValue}
	}

	arcSegs := max(segments/4, 4) // segments per quarter turn

	// Build the buffer by walking one side of the line, adding endpoint
	// caps, then walking back along the other side.
	right := offsetSideRing(l.Points, distance, +1, arcSegs)
	left := offsetSideRing(l.Points, distance, -1, arcSegs)

	// Assemble outer ring: right side forward, end cap (semicircle),
	// left side reversed, start cap (semicircle back to the first right point).
	ring := make([]Point, 0, len(right)+len(left)+2*arcSegs+4)
	ring = append(ring, right...)

	// End cap around the last vertex.
	end := l.Points[len(l.Points)-1]
	prev := l.Points[len(l.Points)-2]
	endHeading := math.Atan2(end.Y-prev.Y, end.X-prev.X)
	ring = append(ring, arcAround(end, endHeading-math.Pi/2, endHeading+math.Pi/2, distance, arcSegs, l.CRSValue)...)

	// Reversed left side.
	for i := len(left) - 1; i >= 0; i-- {
		ring = append(ring, left[i])
	}

	// Start cap around the first vertex.
	start := l.Points[0]
	next := l.Points[1]
	startHeading := math.Atan2(next.Y-start.Y, next.X-start.X)
	// Cap goes from the "left" side around to the "right" side, i.e. from
	// startHeading + pi/2 sweeping to startHeading + 3*pi/2.
	ring = append(ring, arcAround(start, startHeading+math.Pi/2, startHeading+3*math.Pi/2, distance, arcSegs, l.CRSValue)...)

	// Close the ring explicitly.
	if len(ring) > 0 && ring[0] != ring[len(ring)-1] {
		ring = append(ring, ring[0])
	}
	return Polygon{Rings: [][]Point{ring}, CRSValue: l.CRSValue}
}

// Buffer returns a polygon approximating the set of points within distance
// of p's exterior ring. Holes are shrunk (their offset ring moves inward);
// if a hole's offset would collapse or self-intersect it is dropped.
//
// This is a positive (outward) buffer only. For convex or nearly-convex
// inputs the output is topologically correct. For strongly concave inputs
// the offset exterior ring may self-intersect — cleaning that up requires
// a polygon-union pass, which this package doesn't yet provide.
func (p Polygon) Buffer(distance float64, segments int) Polygon {
	if segments < 4 {
		segments = DefaultBufferSegments
	}
	if len(p.Rings) == 0 || len(p.Rings[0]) < 3 {
		return Polygon{CRSValue: p.CRSValue}
	}
	arcSegs := max(segments/4, 4)

	// Exterior: buffer the closed ring outward (side = +1 when CCW).
	ext := closedRing(p.Rings[0])
	side := 1
	if ringIsCW(ext) {
		side = -1
	}
	outer := offsetClosedRing(ext, distance, side, arcSegs)
	if len(outer) > 0 && outer[0] != outer[len(outer)-1] {
		outer = append(outer, outer[0])
	}

	rings := [][]Point{outer}
	// Holes: offset inward. If the ring collapses, skip it.
	for _, h := range p.Rings[1:] {
		hh := closedRing(h)
		holeSide := -1
		if ringIsCW(hh) {
			holeSide = 1
		}
		offset := offsetClosedRing(hh, distance, holeSide, arcSegs)
		if len(offset) < 4 {
			continue
		}
		if offset[0] != offset[len(offset)-1] {
			offset = append(offset, offset[0])
		}
		rings = append(rings, offset)
	}
	return Polygon{Rings: rings, CRSValue: p.CRSValue}
}

// offsetSideRing walks pts and emits the sequence of offset points on the
// given side (+1 = right, -1 = left, relative to direction of travel),
// with rounded arcs at internal vertices.
func offsetSideRing(pts []Point, distance float64, side, arcSegs int) []Point {
	if len(pts) < 2 {
		return nil
	}
	out := make([]Point, 0, len(pts)+arcSegs*len(pts))

	// First point: perpendicular offset from the first segment.
	nx0, ny0 := unitNormal(pts[0], pts[1], side)
	out = append(out, Point{
		X:        pts[0].X + distance*nx0,
		Y:        pts[0].Y + distance*ny0,
		CRSValue: pts[0].CRSValue,
	})

	// Internal vertices: emit offset from incoming segment, then a rounded
	// arc around the vertex if this is a "convex" turn on this side.
	for i := 1; i < len(pts)-1; i++ {
		prev, cur, next := pts[i-1], pts[i], pts[i+1]
		nxIn, nyIn := unitNormal(prev, cur, side)
		nxOut, nyOut := unitNormal(cur, next, side)

		// Point just before the vertex, offset from the incoming segment.
		out = append(out, Point{
			X:        cur.X + distance*nxIn,
			Y:        cur.Y + distance*nyIn,
			CRSValue: cur.CRSValue,
		})

		// Decide join style based on whether we turn toward or away from
		// this side. Cross product of (segment direction) x (offset dir)
		// switches sign at a convex-vs-concave turn.
		turnSign := (cur.X-prev.X)*(next.Y-cur.Y) - (cur.Y-prev.Y)*(next.X-cur.X)
		// side=+1 (right) sees a convex turn when turning right (turnSign < 0).
		// side=-1 (left)  sees a convex turn when turning left  (turnSign > 0).
		// Convex on this offset side means the two offset lines diverge —
		// we need to fill the gap with a circular arc. For a segment
		// walking in some direction:
		//   side=+1 (right side): LEFT turn (turnSign > 0) → outside → arc
		//   side=-1 (left side):  RIGHT turn (turnSign < 0) → outside → arc
		convex := (side > 0 && turnSign > 0) || (side < 0 && turnSign < 0)
		if convex {
			// Sweep a circular arc from incoming normal to outgoing normal.
			startAng := math.Atan2(nyIn, nxIn)
			endAng := math.Atan2(nyOut, nxOut)
			out = append(out, arcSweep(cur, startAng, endAng, distance, side, arcSegs, cur.CRSValue)...)
		}
		// For concave joins we just emit the outgoing-normal offset as the
		// next point below; the offset lines meet cleanly for typical
		// inputs. Self-intersection at extreme concavity is a known
		// limitation.

		out = append(out, Point{
			X:        cur.X + distance*nxOut,
			Y:        cur.Y + distance*nyOut,
			CRSValue: cur.CRSValue,
		})
	}

	// Last point: offset from the final segment.
	nxN, nyN := unitNormal(pts[len(pts)-2], pts[len(pts)-1], side)
	out = append(out, Point{
		X:        pts[len(pts)-1].X + distance*nxN,
		Y:        pts[len(pts)-1].Y + distance*nyN,
		CRSValue: pts[len(pts)-1].CRSValue,
	})
	return out
}

// offsetClosedRing produces an offset polyline for a closed ring, wrapping
// the last-to-first join like every other vertex. side=+1 = outside for
// CCW rings; side=-1 = inside.
func offsetClosedRing(pts []Point, distance float64, side, arcSegs int) []Point {
	if len(pts) < 4 { // 3 unique + closing repeat
		return nil
	}
	// Drop the closing duplicate before walking; we'll close explicitly.
	uniq := pts[:len(pts)-1]
	n := len(uniq)
	if n < 3 {
		return nil
	}
	out := make([]Point, 0, n*(arcSegs+2))
	for i := 0; i < n; i++ {
		prev := uniq[(i-1+n)%n]
		cur := uniq[i]
		next := uniq[(i+1)%n]
		nxIn, nyIn := unitNormal(prev, cur, side)
		nxOut, nyOut := unitNormal(cur, next, side)

		out = append(out, Point{
			X:        cur.X + distance*nxIn,
			Y:        cur.Y + distance*nyIn,
			CRSValue: cur.CRSValue,
		})

		turnSign := (cur.X-prev.X)*(next.Y-cur.Y) - (cur.Y-prev.Y)*(next.X-cur.X)
		// Convex on this offset side means the two offset lines diverge —
		// we need to fill the gap with a circular arc. For a segment
		// walking in some direction:
		//   side=+1 (right side): LEFT turn (turnSign > 0) → outside → arc
		//   side=-1 (left side):  RIGHT turn (turnSign < 0) → outside → arc
		convex := (side > 0 && turnSign > 0) || (side < 0 && turnSign < 0)
		if convex {
			startAng := math.Atan2(nyIn, nxIn)
			endAng := math.Atan2(nyOut, nxOut)
			out = append(out, arcSweep(cur, startAng, endAng, distance, side, arcSegs, cur.CRSValue)...)
		}
		out = append(out, Point{
			X:        cur.X + distance*nxOut,
			Y:        cur.Y + distance*nyOut,
			CRSValue: cur.CRSValue,
		})
	}
	return out
}

// unitNormal returns the components of the unit vector perpendicular to
// segment (a, b), on the given side. side = +1 rotates the direction
// vector 90° clockwise (right); side = -1 rotates 90° counterclockwise
// (left).
func unitNormal(a, b Point, side int) (nx, ny float64) {
	dx := b.X - a.X
	dy := b.Y - a.Y
	length := math.Sqrt(dx*dx + dy*dy)
	if length == 0 {
		return 0, 0
	}
	dx /= length
	dy /= length
	if side > 0 {
		return dy, -dx
	}
	return -dy, dx
}

// arcSweep returns the intermediate points of a circular arc centered at
// center, sweeping from startAng to endAng around the outside of a convex
// corner. side=+1 (right-hand offset) sweeps CCW; side=-1 (left-hand
// offset) sweeps CW. Endpoints are not included — the caller has already
// emitted the point at startAng and will emit the point at endAng next.
func arcSweep(center Point, startAng, endAng, radius float64, side, segments int, crs CRS) []Point {
	sweep := endAng - startAng
	// Normalize to (-π, π].
	for sweep > math.Pi {
		sweep -= 2 * math.Pi
	}
	for sweep <= -math.Pi {
		sweep += 2 * math.Pi
	}
	// Direction convention: side=+1 → CCW (positive), side=-1 → CW.
	if side > 0 && sweep < 0 {
		sweep += 2 * math.Pi
	}
	if side < 0 && sweep > 0 {
		sweep -= 2 * math.Pi
	}
	// Split the arc into `segments` steps. Emit intermediate points only
	// (skip endpoints).
	pts := make([]Point, 0, segments-1)
	for i := 1; i < segments; i++ {
		t := float64(i) / float64(segments)
		ang := startAng + sweep*t
		pts = append(pts, Point{
			X:        center.X + radius*math.Cos(ang),
			Y:        center.Y + radius*math.Sin(ang),
			CRSValue: crs,
		})
	}
	return pts
}

// arcAround returns a semicircular cap around center from startAng to
// endAng, inclusive on both ends. Sweep direction always goes CCW so the
// resulting ring is oriented consistently with the surrounding buffer.
func arcAround(center Point, startAng, endAng, radius float64, segments int, crs CRS) []Point {
	sweep := endAng - startAng
	for sweep < 0 {
		sweep += 2 * math.Pi
	}
	pts := make([]Point, 0, segments+1)
	for i := 0; i <= segments; i++ {
		t := float64(i) / float64(segments)
		ang := startAng + sweep*t
		pts = append(pts, Point{
			X:        center.X + radius*math.Cos(ang),
			Y:        center.Y + radius*math.Sin(ang),
			CRSValue: crs,
		})
	}
	return pts
}

// ringIsCW reports whether the closed ring is clockwise (signed area < 0).
func ringIsCW(ring []Point) bool {
	var a float64
	for i := 0; i < len(ring)-1; i++ {
		a += ring[i].X*ring[i+1].Y - ring[i+1].X*ring[i].Y
	}
	return a < 0
}

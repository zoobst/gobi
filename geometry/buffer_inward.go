package geometry

import (
	"fmt"
	"math"
)

// bufferInward shrinks g by d > 0: the result is every point of g
// that is more than d from g's boundary. See Buffer for the result
// types.
func bufferInward(g Geometry, d float64, segments int) (Geometry, error) {
	switch t := g.(type) {
	case Point, LineString, MultiPoint, MultiLineString:
		// No interior, so nothing is left.
		return Polygon{CRSValue: g.CRS()}, nil
	case Polygon:
		return erodePolygon(t, d, segments)
	case MultiPolygon:
		// The members of a valid MultiPolygon don't overlap, so each
		// one shrinks on its own.
		var polys []Polygon
		for _, p := range t.Polygons {
			out, err := erodePolygon(p, d, segments)
			if err != nil {
				return nil, err
			}
			polys = appendPolygons(polys, out)
		}
		if len(polys) == 0 {
			return Polygon{CRSValue: t.CRSValue}, nil
		}
		return MultiPolygon{Polygons: polys, CRSValue: t.CRSValue}, nil
	}
	return nil, fmt.Errorf("buffer: unsupported type %T", g)
}

// erodePolygon returns p minus everything within d of p's boundary.
//
// The band within d of the boundary is built from simple convex
// pieces (boundaryBand), merged with Dissolve and subtracted with
// Difference, so the result is a valid polygon even when p is
// concave, has holes, or splits apart.
//
// The boolean ops refuse a geographic CRS, but a buffer is plain
// coordinate arithmetic in whatever unit the CRS has, so the work is
// done with the CRS unset and it is put back on the result.
func erodePolygon(p Polygon, d float64, segments int) (Geometry, error) {
	crs := p.CRSValue
	empty := Polygon{CRSValue: crs}
	if len(p.Rings) == 0 {
		return empty, nil
	}
	// Anything narrower than 2d in either axis vanishes entirely.
	if b := p.Bounds(); b.MaxX-b.MinX <= 2*d || b.MaxY-b.MinY <= 2*d {
		return empty, nil
	}
	var band []Geometry
	for _, r := range p.Rings {
		band = boundaryBand(band, distinctRingVertices(r), d, segments)
	}
	if len(band) == 0 {
		return empty, nil
	}
	removed, err := Dissolve(band)
	if err != nil {
		return nil, fmt.Errorf("buffer: %w", err)
	}
	out, err := Difference(Polygon{Rings: p.Rings}, removed)
	if err != nil {
		return nil, fmt.Errorf("buffer: %w", err)
	}
	return copyWithCRS(out, crs), nil
}

// straightTurn is the sine of the turn angle below which a vertex is
// treated as lying on a straight line (about 0.06°). See boundaryBand.
const straightTurn = 1e-3

// boundaryBand appends polygons whose union is every point within d
// of the closed ring through ring's vertices (given without the
// closing repeat), and returns the extended slice.
//
// There are two kinds of piece:
//
//   - one per edge: the rectangle 2d wide centered on the edge, with
//     the edge's two vertices added as corners of their own.
//   - one per vertex: a fan of radius d filling the wedge between the
//     two rectangles that meet there, on the side where they diverge.
//     This is what rounds the corners that point into the polygon.
//
// The pieces are arranged so that wherever two of them meet they
// share vertices and whole edges with exactly the same coordinates:
// a fan's two straight edges are edges of the rectangles on either
// side of it. Pieces that only nearly touch leave slivers thinner
// than the boolean ops' tolerance, which they cannot resolve. For the
// same reason the two rectangles at a vertex where the ring barely
// turns (see straightTurn) share one pair of corners on the bisector
// instead of getting a fan.
//
// The union covers a whole neighborhood of every point of the ring,
// so the ring itself never touches the union's boundary.
//
// The fans are inscribed in the true circle, so a rounded corner
// keeps slightly more than an exact buffer would: less than
// d × (1 − cos(π / segments)).
func boundaryBand(band []Geometry, ring []Point, d float64, segments int) []Geometry {
	n := len(ring)
	if n < 3 {
		return band
	}
	// The corners beside vertex i: to the right and left of the edge
	// arriving at it, and of the edge leaving it.
	type corners struct{ rightIn, leftIn, rightOut, leftOut Point }
	at := make([]corners, n)
	offset := func(v Point, nx, ny, sign float64) Point {
		return Point{X: v.X + sign*d*nx, Y: v.Y + sign*d*ny}
	}
	for i, v := range ring {
		prev, next := ring[(i-1+n)%n], ring[(i+1)%n]
		inX, inY := unitNormal(prev, v, 1)
		outX, outY := unitNormal(v, next, 1)
		// Unit normals, so these are the sine and cosine of the turn.
		sin := inX*outY - inY*outX
		cos := inX*outX + inY*outY
		if cos > 0 && math.Abs(sin) < straightTurn {
			// Straight on: both rectangles end on the bisector.
			mx, my := inX+outX, inY+outY
			l := math.Hypot(mx, my)
			mx, my = mx/l, my/l
			right, left := offset(v, mx, my, 1), offset(v, mx, my, -1)
			at[i] = corners{rightIn: right, rightOut: right, leftIn: left, leftOut: left}
			continue
		}
		c := corners{
			rightIn: offset(v, inX, inY, 1), rightOut: offset(v, outX, outY, 1),
			leftIn: offset(v, inX, inY, -1), leftOut: offset(v, outX, outY, -1),
		}
		at[i] = c
		// A left turn opens a wedge on the right, swept
		// counter-clockwise from the arriving edge's normal to the
		// leaving edge's; a right turn opens one on the left, swept
		// clockwise.
		from, to := c.rightIn, c.rightOut
		start := math.Atan2(inY, inX)
		sweep := math.Atan2(sin, cos)
		if sin < 0 {
			from, to = c.leftIn, c.leftOut
			start = math.Atan2(-inY, -inX)
		} else if sweep <= 0 {
			sweep = math.Pi // a full reversal
		}
		steps := int(math.Ceil(math.Abs(sweep) / (2 * math.Pi / float64(segments))))
		fan := make([]Point, 0, steps+3)
		fan = append(fan, Point{X: v.X, Y: v.Y}, from)
		for k := 1; k < steps; k++ {
			a := start + sweep*float64(k)/float64(steps)
			fan = append(fan, Point{X: v.X + d*math.Cos(a), Y: v.Y + d*math.Sin(a)})
		}
		fan = append(fan, to, fan[0])
		band = append(band, Polygon{Rings: [][]Point{fan}})
	}
	for i, v := range ring {
		j := (i + 1) % n
		a, b := Point{X: v.X, Y: v.Y}, Point{X: ring[j].X, Y: ring[j].Y}
		band = append(band, Polygon{Rings: [][]Point{{
			a, at[i].rightOut, at[j].rightIn, b, at[j].leftIn, at[i].leftOut, a,
		}}})
	}
	return band
}

// distinctRingVertices returns ring's vertices without the closing
// repeat and without consecutive duplicates, so every edge between
// neighbors (wrapping around) has a length.
func distinctRingVertices(ring []Point) []Point {
	out := make([]Point, 0, len(ring))
	for _, p := range ring {
		if n := len(out); n > 0 && out[n-1].X == p.X && out[n-1].Y == p.Y {
			continue
		}
		out = append(out, p)
	}
	for n := len(out); n > 1 && out[0].X == out[n-1].X && out[0].Y == out[n-1].Y; n = len(out) {
		out = out[:n-1]
	}
	return out
}

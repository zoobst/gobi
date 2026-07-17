package geometry

import (
	"fmt"
	"math"
)

// Simplify returns a copy of g with vertices removed until every discarded
// vertex lies within tolerance (planar distance) of the retained polyline.
// Uses the Douglas-Peucker algorithm.
//
// tolerance is measured in the CRS's linear unit (degrees for WGS84,
// meters for a projected CRS). Passing tolerance <= 0 returns g unchanged.
//
// Point and MultiPoint pass through untouched; the algorithm doesn't apply
// to them. GeometryCollection recurses into each component.
func Simplify(g Geometry, tolerance float64) (Geometry, error) {
	if tolerance <= 0 {
		return g, nil
	}
	switch t := g.(type) {
	case Point, MultiPoint:
		return g, nil
	case LineString:
		return t.Simplify(tolerance), nil
	case Polygon:
		return t.Simplify(tolerance), nil
	case MultiLineString:
		out := make([]LineString, len(t.Lines))
		for i, l := range t.Lines {
			out[i] = l.Simplify(tolerance)
		}
		return MultiLineString{Lines: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	case MultiPolygon:
		out := make([]Polygon, len(t.Polygons))
		for i, p := range t.Polygons {
			out[i] = p.Simplify(tolerance)
		}
		return MultiPolygon{Polygons: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	case GeometryCollection:
		inner := make([]Geometry, len(t.Geometries))
		for i, inG := range t.Geometries {
			simp, err := Simplify(inG, tolerance)
			if err != nil {
				return nil, err
			}
			inner[i] = simp
		}
		return GeometryCollection{Geometries: inner, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	}
	return nil, fmt.Errorf("simplify: unsupported type %T", g)
}

// Simplify returns a copy of l with vertices removed via Douglas-Peucker at
// the given planar tolerance. Endpoints are always preserved. If the line
// has fewer than 3 points it is returned unchanged (a 2-point line is
// already the simplest possible representation).
func (l LineString) Simplify(tolerance float64) LineString {
	if len(l.Points) < 3 || tolerance <= 0 {
		return l
	}
	simplified := douglasPeucker(l.Points, tolerance)
	return LineString{Points: simplified, CRSValue: l.CRSValue, HasZ: l.HasZ}
}

// Simplify applies Douglas-Peucker to each ring of the polygon. Rings that
// collapse to fewer than 4 points (three unique vertices plus the closing
// vertex) are kept as-is to preserve topological validity of the polygon.
func (p Polygon) Simplify(tolerance float64) Polygon {
	if tolerance <= 0 || len(p.Rings) == 0 {
		return p
	}
	rings := make([][]Point, len(p.Rings))
	for i, ring := range p.Rings {
		if len(ring) < 5 { // triangle + close
			rings[i] = ring
			continue
		}
		// Preserve ring closure: run DP on the interior points, then close.
		simp := douglasPeucker(ring, tolerance)
		if len(simp) < 4 {
			// Simplification collapsed the ring — keep the original to
			// avoid producing a degenerate polygon.
			rings[i] = ring
			continue
		}
		// Ensure the ring stays closed.
		if simp[0] != simp[len(simp)-1] {
			simp = append(simp, simp[0])
		}
		rings[i] = simp
	}
	return Polygon{Rings: rings, CRSValue: p.CRSValue, HasZ: p.HasZ}
}

// douglasPeucker is the classic recursive implementation. Given a polyline
// and a tolerance, it returns the smallest subsequence of points such that
// every discarded point lies within tolerance of the segment between its
// nearest retained neighbors.
func douglasPeucker(points []Point, tolerance float64) []Point {
	if len(points) < 3 {
		return points
	}
	// Find the point farthest from the endpoint-to-endpoint chord.
	maxDist := 0.0
	maxIdx := 0
	first := points[0]
	last := points[len(points)-1]
	for i := 1; i < len(points)-1; i++ {
		d := perpDistance(points[i], first, last)
		if d > maxDist {
			maxDist = d
			maxIdx = i
		}
	}
	if maxDist <= tolerance {
		// Every intermediate point is within tolerance of the chord; drop them.
		return []Point{first, last}
	}
	// Recurse on both halves and stitch (dropping the duplicated split point).
	left := douglasPeucker(points[:maxIdx+1], tolerance)
	right := douglasPeucker(points[maxIdx:], tolerance)
	out := make([]Point, 0, len(left)+len(right)-1)
	out = append(out, left...)
	out = append(out, right[1:]...)
	return out
}

// perpDistance returns the perpendicular (shortest) distance from p to the
// infinite line through a and b, in planar XY. If a and b coincide, it
// returns the Euclidean distance from p to a.
func perpDistance(p, a, b Point) float64 {
	dx := b.X - a.X
	dy := b.Y - a.Y
	segLen2 := dx*dx + dy*dy
	if segLen2 == 0 {
		ax := p.X - a.X
		ay := p.Y - a.Y
		return math.Sqrt(ax*ax + ay*ay)
	}
	// Numerator: |cross((b-a), (p-a))|
	num := math.Abs(dx*(a.Y-p.Y) - (a.X-p.X)*dy)
	return num / math.Sqrt(segLen2)
}

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

// douglasPeucker returns the smallest subsequence of points such
// that every discarded point lies within tolerance of the segment
// between its nearest retained neighbors.
//
// Backed by simplifyDPKeep (Slice 9 SoA kernel): converts the
// []Point input to parallel XY slabs, runs the iterative
// stack+bitmap kernel, and walks the retained-index bitmap to
// rebuild the []Point output. On measured workloads this is
// ~5× faster than the classic recursive implementation at n=1M
// (3.85 s → 0.75 s) and drops memory by three orders of magnitude
// (5.75 GB → 3.2 MB, 260k allocs → 11 allocs) — the AoS
// recursion appends O(log n) intermediate slices per split.
//
// Preserves Point.Z / HasZ / CRSValue on retained points (post-
// review fix: the earlier SimplifyDPFromXY-based body silently
// dropped Z, regressing 3D LineStrings / Polygons that carried
// altitude through the AoS recursion).
//
// Semantics + tie-breaking match the recursive form exactly:
// argmax uses strict `>` (first occurrence wins), and split order
// is left-then-right on the explicit stack.
func douglasPeucker(points []Point, tolerance float64) []Point {
	if len(points) < 3 {
		return points
	}
	xs := make([]float64, len(points))
	ys := make([]float64, len(points))
	for i, p := range points {
		xs[i] = p.X
		ys[i] = p.Y
	}
	keep := simplifyDPKeep(xs, ys, tolerance)
	m := 0
	for _, k := range keep {
		if k {
			m++
		}
	}
	out := make([]Point, 0, m)
	for i, k := range keep {
		if k {
			out = append(out, points[i])
		}
	}
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

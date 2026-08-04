package geometry

import (
	"fmt"
	"math"
)

// GeomDistance returns the minimum planar (Euclidean) distance between
// any two points in a and b. Returns 0 when they intersect. Uses
// point-to-segment distance across all vertex-vs-edge pairs from both
// sides — O(V_a·E_b + V_b·E_a) in the general case.
//
// Coordinates are treated as planar meters; for geographic
// (WGS84 lon/lat) inputs the result is Euclidean on degrees, which is
// meaningless. Project to a suitable CRS first (see Point.Distance /
// Haversine for lon/lat point pairs).
func GeomDistance(a, b Geometry, u Unit) (float64, error) {
	if a == nil || b == nil {
		return 0, fmt.Errorf("GeomDistance: nil geometry")
	}
	if Intersects(a, b) {
		return 0, nil
	}
	d := planarMinDistance(a, b)
	if u == UnitMeters || u == "" {
		return d, nil
	}
	perM, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	return d / perM, nil
}

// planarMinDistance returns the min Euclidean distance between a and b
// in coord units. Assumes non-intersecting inputs.
func planarMinDistance(a, b Geometry) float64 {
	best := math.Inf(1)
	forEachVertex(a, func(p Point) {
		forEachSegment(b, func(s0, s1 Point) {
			if d := pointToSegmentDistance(p, s0, s1); d < best {
				best = d
			}
		})
	})
	forEachVertex(b, func(p Point) {
		forEachSegment(a, func(s0, s1 Point) {
			if d := pointToSegmentDistance(p, s0, s1); d < best {
				best = d
			}
		})
	})
	// Point-to-Point fallback when neither side has segments (both are
	// Points / MultiPoints). forEachSegment produces nothing for those,
	// so `best` stays at +Inf; do a vertex-to-vertex pass instead.
	if math.IsInf(best, 1) {
		forEachVertex(a, func(pa Point) {
			forEachVertex(b, func(pb Point) {
				d := math.Hypot(pa.X-pb.X, pa.Y-pb.Y)
				if d < best {
					best = d
				}
			})
		})
	}
	if math.IsInf(best, 1) {
		return 0
	}
	return best
}

// pointToSegmentDistance returns Euclidean distance from p to the closed
// segment (a, b). Handles a==b as point-to-point.
func pointToSegmentDistance(p, a, b Point) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		return math.Hypot(p.X-a.X, p.Y-a.Y)
	}
	t := ((p.X-a.X)*dx + (p.Y-a.Y)*dy) / lenSq
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	fx := a.X + t*dx
	fy := a.Y + t*dy
	return math.Hypot(p.X-fx, p.Y-fy)
}

func forEachVertex(g Geometry, fn func(Point)) {
	switch t := g.(type) {
	case Point:
		fn(t)
	case MultiPoint:
		for _, p := range t.Points {
			fn(p)
		}
	case LineString:
		for _, p := range t.Points {
			fn(p)
		}
	case MultiLineString:
		for _, l := range t.Lines {
			for _, p := range l.Points {
				fn(p)
			}
		}
	case Polygon:
		for _, r := range t.Rings {
			for _, p := range r {
				fn(p)
			}
		}
	case MultiPolygon:
		for _, p := range t.Polygons {
			for _, r := range p.Rings {
				for _, pt := range r {
					fn(pt)
				}
			}
		}
	case GeometryCollection:
		for _, inner := range t.Geometries {
			forEachVertex(inner, fn)
		}
	}
}

func forEachSegment(g Geometry, fn func(a, b Point)) {
	switch t := g.(type) {
	case Point, MultiPoint:
		// no edges
	case LineString:
		for i := range len(t.Points) - 1 {
			fn(t.Points[i], t.Points[i+1])
		}
	case MultiLineString:
		for _, l := range t.Lines {
			for i := range len(l.Points) - 1 {
				fn(l.Points[i], l.Points[i+1])
			}
		}
	case Polygon:
		for _, r := range t.Rings {
			ring := closedRing(r)
			for i := range len(ring) - 1 {
				fn(ring[i], ring[i+1])
			}
		}
	case MultiPolygon:
		for _, p := range t.Polygons {
			for _, r := range p.Rings {
				ring := closedRing(r)
				for i := range len(ring) - 1 {
					fn(ring[i], ring[i+1])
				}
			}
		}
	case GeometryCollection:
		for _, inner := range t.Geometries {
			forEachSegment(inner, fn)
		}
	}
}

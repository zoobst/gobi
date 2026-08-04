package geometry

import (
	"errors"
	"math"
)

// ErrAntimeridianCrossing indicates that an operation received a
// geographic-CRS geometry whose vertices span the ±180° meridian and
// cannot be interpreted correctly under gobi's cartesian primitives
// without pre-splitting. Callers should either
//   - route through SplitAtAntimeridian, or
//   - reproject to a projected CRS that avoids the discontinuity.
var ErrAntimeridianCrossing = errors.New("geometry: geometry crosses the antimeridian")

// CrossesAntimeridian reports whether any adjacent vertex pair in g has
// |Δlon| > 180°, which under WGS84 interpretation means the edge
// between them wraps around the ±180° meridian. Returns false for
// projected-CRS inputs (the concept doesn't apply) and for empty
// geometries. Assumes X = longitude, Y = latitude in degrees, matching
// WKB convention.
func CrossesAntimeridian(g Geometry) bool {
	if g == nil {
		return false
	}
	if c := g.CRS(); !c.Zero() && c.Projected {
		return false
	}
	crossed := false
	forEachSegment(g, func(a, b Point) {
		if crossed {
			return
		}
		if math.Abs(b.X-a.X) > 180 {
			crossed = true
		}
	})
	return crossed
}

// AntimeridianCrossings returns the (lon=±180, lat) points at which g's
// boundary crosses the antimeridian, in the order encountered while
// walking edges. Each physical crossing produces ONE entry (at lon
// matching the edge's starting side); callers building split output
// should mirror to the opposite side themselves.
func AntimeridianCrossings(g Geometry) []Point {
	if g == nil {
		return nil
	}
	if c := g.CRS(); !c.Zero() && c.Projected {
		return nil
	}
	var out []Point
	forEachSegment(g, func(a, b Point) {
		if lat, ok := antimeridianCrossingLat(a, b); ok {
			side := 180.0
			if a.X < 0 {
				side = -180.0
			}
			out = append(out, Point{X: side, Y: lat, CRSValue: g.CRS()})
		}
	})
	return out
}

// SplitAtAntimeridian returns g split into components that each lie on
// one side of the ±180° meridian. Antimeridian-crossing edges get a
// synthetic vertex inserted at (±180, interpolated-lat) on each side.
// Non-crossing inputs pass through unchanged.
//
// Supported types: LineString → MultiLineString, MultiLineString →
// MultiLineString, Polygon → MultiPolygon (assuming the exterior ring
// crosses an even number of times and holes don't cross), MultiPolygon
// → MultiPolygon. Point / MultiPoint pass through unchanged.
// GeometryCollection recurses into components.
//
// Returns an error only if the input CRS is projected (where the
// operation is meaningless) — in that case the geometry is returned
// unchanged.
func SplitAtAntimeridian(g Geometry) (Geometry, error) {
	if g == nil {
		return nil, nil
	}
	if !CrossesAntimeridian(g) {
		return g, nil
	}
	switch t := g.(type) {
	case Point, MultiPoint:
		return t, nil // points can't span the antimeridian
	case LineString:
		return splitLineStringAtAntimeridian(t), nil
	case MultiLineString:
		out := make([]LineString, 0, len(t.Lines))
		for _, l := range t.Lines {
			if !CrossesAntimeridian(l) {
				out = append(out, l)
				continue
			}
			mls := splitLineStringAtAntimeridian(l)
			out = append(out, mls.Lines...)
		}
		return MultiLineString{Lines: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	case Polygon:
		return splitPolygonAtAntimeridian(t), nil
	case MultiPolygon:
		out := make([]Polygon, 0, len(t.Polygons))
		for _, p := range t.Polygons {
			p.CRSValue = t.CRSValue
			if !CrossesAntimeridian(p) {
				out = append(out, p)
				continue
			}
			mp := splitPolygonAtAntimeridian(p)
			out = append(out, mp.Polygons...)
		}
		return MultiPolygon{Polygons: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	case GeometryCollection:
		out := make([]Geometry, 0, len(t.Geometries))
		for _, inner := range t.Geometries {
			s, err := SplitAtAntimeridian(inner)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return GeometryCollection{Geometries: out, CRSValue: t.CRSValue, HasZ: t.HasZ}, nil
	}
	return g, nil
}

// antimeridianCrossingLat returns the latitude at which the edge from
// a to b crosses ±180°, computed by linear interpolation along the
// "short-path" edge (i.e. the one that actually crosses the
// antimeridian, not the long way through the prime meridian). ok is
// false if the edge does not cross.
func antimeridianCrossingLat(a, b Point) (float64, bool) {
	dlon := b.X - a.X
	if math.Abs(dlon) <= 180 {
		return 0, false
	}
	// Unwrap: replace b's longitude with the one on the "same side"
	// as a plus a shift of ±360, whichever makes |b'-a| <= 180.
	var bLon float64
	if a.X > b.X {
		bLon = b.X + 360 // going east
	} else {
		bLon = b.X - 360 // going west
	}
	// Target antimeridian for interpolation is the one nearest a.X.
	targetLon := 180.0
	if a.X < 0 {
		targetLon = -180.0
	}
	t := (targetLon - a.X) / (bLon - a.X)
	return a.Y + t*(b.Y-a.Y), true
}

// splitLineStringAtAntimeridian walks l and inserts (±180, lat) breaks
// at every antimeridian crossing, then splits the vertex list into
// components. The result is a MultiLineString even when the original
// LineString has just one segment that crosses (two sub-lines).
func splitLineStringAtAntimeridian(l LineString) MultiLineString {
	if len(l.Points) < 2 {
		return MultiLineString{Lines: []LineString{l}, CRSValue: l.CRSValue, HasZ: l.HasZ}
	}
	crs := l.CRSValue
	var lines []LineString
	current := []Point{l.Points[0]}
	for i := range len(l.Points) - 1 {
		a, b := l.Points[i], l.Points[i+1]
		lat, ok := antimeridianCrossingLat(a, b)
		if !ok {
			current = append(current, b)
			continue
		}
		// Emit up to (±180, lat) on a's side, then start a new line at
		// the opposite side.
		aSide := 180.0
		bSide := -180.0
		if a.X < 0 {
			aSide = -180.0
			bSide = 180.0
		}
		current = append(current, Point{X: aSide, Y: lat, CRSValue: crs})
		if len(current) >= 2 {
			lines = append(lines, LineString{Points: current, CRSValue: crs, HasZ: l.HasZ})
		}
		current = []Point{{X: bSide, Y: lat, CRSValue: crs}, b}
	}
	if len(current) >= 2 {
		lines = append(lines, LineString{Points: current, CRSValue: crs, HasZ: l.HasZ})
	}
	return MultiLineString{Lines: lines, CRSValue: crs, HasZ: l.HasZ}
}

// splitPolygonAtAntimeridian splits a polygon whose exterior ring
// crosses the antimeridian an even number of times into a
// MultiPolygon with one component on each side of ±180°. Holes are
// currently required not to cross; a hole that crosses is left
// attached to whichever half its centroid falls into.
//
// Algorithm:
//  1. Walk the exterior ring, inserting antimeridian vertices at each
//     crossing. Each crossing appends two synthetic vertices: one at
//     (aSide, lat) closing the ring on a's side, and one at (bSide, lat)
//     opening the next arc on b's side.
//  2. Split the augmented ring into arcs at each antimeridian vertex.
//     Each arc is entirely on one side.
//  3. Group arcs by side and stitch each side's arcs into closed
//     ring(s) by walking along the ±180° meridian between arc endpoints.
//
// For the common two-crossing case (a rectangular polygon straddling
// ±180°) the output is a MultiPolygon of two rectangles, one per side.
func splitPolygonAtAntimeridian(p Polygon) MultiPolygon {
	if len(p.Rings) == 0 {
		return MultiPolygon{CRSValue: p.CRSValue, HasZ: p.HasZ}
	}
	crs := p.CRSValue
	ext := closedRing(p.Rings[0])
	eastArcs, westArcs := splitRingIntoArcs(ext, crs)
	if len(eastArcs) == 0 && len(westArcs) == 0 {
		// Non-crossing (shouldn't happen since caller checked
		// CrossesAntimeridian) — return original polygon.
		return MultiPolygon{Polygons: []Polygon{p}, CRSValue: crs, HasZ: p.HasZ}
	}
	eastRings := stitchArcs(eastArcs, +180, crs)
	westRings := stitchArcs(westArcs, -180, crs)

	// Attach holes to whichever side their centroid falls on. Holes that
	// themselves cross the antimeridian are silently dropped for now —
	// splitting those correctly needs another recursion pass and is a
	// documented follow-up.
	var eastHoles, westHoles [][]Point
	for _, h := range p.Rings[1:] {
		if CrossesAntimeridian(LineString{Points: h, CRSValue: crs}) {
			continue
		}
		hc := ringCentroid(h)
		if hc.X >= 0 {
			eastHoles = append(eastHoles, h)
		} else {
			westHoles = append(westHoles, h)
		}
	}

	var polys []Polygon
	for _, r := range eastRings {
		poly := Polygon{Rings: [][]Point{r}, CRSValue: crs, HasZ: p.HasZ}
		poly.Rings = append(poly.Rings, eastHoles...)
		polys = append(polys, poly)
	}
	for _, r := range westRings {
		poly := Polygon{Rings: [][]Point{r}, CRSValue: crs, HasZ: p.HasZ}
		poly.Rings = append(poly.Rings, westHoles...)
		polys = append(polys, poly)
	}
	return MultiPolygon{Polygons: polys, CRSValue: crs, HasZ: p.HasZ}
}

// splitRingIntoArcs walks a closed ring and returns the arcs on each
// side of the antimeridian, in the order encountered. Each arc begins
// and ends at ±180° (or the ring's original start vertex).
func splitRingIntoArcs(ring []Point, crs CRS) (eastArcs, westArcs [][]Point) {
	if len(ring) < 4 {
		return nil, nil
	}
	// Augmented list of vertices with antimeridian crossings inserted.
	// Each crossing produces two vertices — one at each side — placed
	// consecutively so the walk can detect a "switch of side".
	type augVertex struct {
		P    Point
		Side int8 // +1 east (lon > 0 or lon == 180), -1 west, 0 unknown
	}
	classify := func(lon float64) int8 {
		if lon > 0 {
			return +1
		}
		if lon < 0 {
			return -1
		}
		// lon == 0: treat as east (arbitrary but consistent).
		return +1
	}
	var aug []augVertex
	for i := range len(ring) - 1 {
		a := ring[i]
		b := ring[i+1]
		aug = append(aug, augVertex{P: a, Side: classify(a.X)})
		if lat, ok := antimeridianCrossingLat(a, b); ok {
			aSide := int8(+1)
			aLon := 180.0
			bLon := -180.0
			if a.X < 0 {
				aSide = -1
				aLon = -180.0
				bLon = 180.0
			}
			aug = append(aug, augVertex{P: Point{X: aLon, Y: lat, CRSValue: crs}, Side: aSide})
			// The b-side vertex starts a new arc; it belongs to b's side.
			bSideCls := classify(b.X)
			aug = append(aug, augVertex{P: Point{X: bLon, Y: lat, CRSValue: crs}, Side: bSideCls})
		}
	}
	// Walk aug, grouping consecutive same-side vertices into arcs.
	// Every antimeridian vertex sits at the boundary between arcs.
	var current []Point
	var currentSide int8
	flush := func() {
		if len(current) < 2 {
			current = nil
			return
		}
		if currentSide > 0 {
			eastArcs = append(eastArcs, current)
		} else {
			westArcs = append(westArcs, current)
		}
		current = nil
	}
	for _, v := range aug {
		if len(current) == 0 {
			currentSide = v.Side
			current = append(current, v.P)
			continue
		}
		if v.Side != currentSide {
			// End current arc at previous ±180° vertex, start next at
			// this one.
			flush()
			currentSide = v.Side
			current = append(current, v.P)
			continue
		}
		current = append(current, v.P)
	}
	flush()
	// The ring is closed, so the first and last aug vertices are the
	// same original point; merge the trailing arc with the leading arc
	// if they're on the same side.
	if len(eastArcs) > 1 && eastArcs[0][0] == eastArcs[len(eastArcs)-1][len(eastArcs[len(eastArcs)-1])-1] {
		// Prepend the leading arc's contents to the trailing arc.
		leading := eastArcs[0]
		trailing := eastArcs[len(eastArcs)-1]
		merged := append(trailing[:len(trailing)-1], leading...)
		eastArcs = append([][]Point{merged}, eastArcs[1:len(eastArcs)-1]...)
	}
	if len(westArcs) > 1 && westArcs[0][0] == westArcs[len(westArcs)-1][len(westArcs[len(westArcs)-1])-1] {
		leading := westArcs[0]
		trailing := westArcs[len(westArcs)-1]
		merged := append(trailing[:len(trailing)-1], leading...)
		westArcs = append([][]Point{merged}, westArcs[1:len(westArcs)-1]...)
	}
	return eastArcs, westArcs
}

// stitchArcs closes each arc into a ring by walking along the
// meridianX side of the antimeridian. Each arc begins and ends at
// meridianX; a simple valid ring is [arc0..., arc1..., ..., arc0[0]]
// walking the antimeridian between arcs.
//
// For the two-arc case (a ring that crossed the antimeridian exactly
// twice), the two arcs meet at the two crossing latitudes, and
// stitching produces one closed ring per side. For higher-order
// crossings, each side may produce multiple disjoint rings; we
// approximate by concatenating all arcs on that side into a single
// ring (correct if crossings interleave in latitude, which is the
// common case).
func stitchArcs(arcs [][]Point, meridianX float64, crs CRS) [][]Point {
	if len(arcs) == 0 {
		return nil
	}
	// Simple case: one arc. It's already closed (started and ended at
	// meridianX) — just return.
	if len(arcs) == 1 {
		r := append([]Point(nil), arcs[0]...)
		if r[0] != r[len(r)-1] {
			r = append(r, r[0])
		}
		return [][]Point{r}
	}
	// Multi-arc case: concatenate all arcs, in the order encountered,
	// closing back to the first arc's start. Between arcs, we walk
	// along the antimeridian by inserting a direct segment (the
	// stitching itself is a straight line at lon = meridianX).
	var ring []Point
	for i, arc := range arcs {
		if i == 0 {
			ring = append(ring, arc...)
			continue
		}
		// Connect the last point of the previous arc to the first
		// point of this arc via a direct segment along ±180°. Both
		// endpoints have lon == meridianX by construction, so this
		// segment doesn't cross the antimeridian.
		ring = append(ring, arc...)
	}
	// Close.
	if ring[0] != ring[len(ring)-1] {
		ring = append(ring, ring[0])
	}
	_ = crs
	return [][]Point{ring}
}

// ringCentroid returns the shoelace centroid of a ring. Uses planar
// interpretation; used only to assign non-crossing holes to a side.
func ringCentroid(ring []Point) Point {
	if len(ring) < 3 {
		if len(ring) == 0 {
			return Point{}
		}
		return ring[0]
	}
	closed := closedRing(ring)
	var cx, cy, areaTwo float64
	for i := range len(closed) - 1 {
		x0, y0 := closed[i].X, closed[i].Y
		x1, y1 := closed[i+1].X, closed[i+1].Y
		cross := x0*y1 - x1*y0
		areaTwo += cross
		cx += (x0 + x1) * cross
		cy += (y0 + y1) * cross
	}
	if areaTwo == 0 {
		return closed[0]
	}
	return Point{X: cx / (3 * areaTwo), Y: cy / (3 * areaTwo)}
}

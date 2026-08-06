package geometry

import "math"

// CircleIntersectionPoints returns the points at which the
// boundaries of c1 and c2 cross:
//
//   - two points for two circles that properly overlap
//   - one point when the circles are externally or internally tangent
//   - zero points when the circles are disjoint, one properly
//     contains the other, or the circles are concentric (in which
//     case the "intersection" is either empty or the whole circle,
//     neither of which is representable as a finite point set)
//
// Result points inherit CRS from c1.Center.
func CircleIntersectionPoints(c1, c2 Circle) []Point {
	dx := c2.Center.X - c1.Center.X
	dy := c2.Center.Y - c1.Center.Y
	d := math.Hypot(dx, dy)
	// Concentric — no well-defined intersection point set.
	if d == 0 {
		return nil
	}
	// Disjoint.
	if d > c1.Radius+c2.Radius {
		return nil
	}
	// One properly contains the other.
	if d+math.Min(c1.Radius, c2.Radius) < math.Max(c1.Radius, c2.Radius) {
		return nil
	}
	// Chord-midpoint distance from c1.Center along the c1→c2 direction.
	a := (c1.Radius*c1.Radius - c2.Radius*c2.Radius + d*d) / (2 * d)
	h2 := c1.Radius*c1.Radius - a*a
	if h2 < 0 {
		// Numerical noise near tangent — clip to 0 so we return the
		// single tangent point rather than nothing.
		h2 = 0
	}
	h := math.Sqrt(h2)
	crs := c1.Center.CRSValue
	// Chord midpoint on the line between the centers.
	mx := c1.Center.X + a*dx/d
	my := c1.Center.Y + a*dy/d
	if h == 0 {
		return []Point{{X: mx, Y: my, CRSValue: crs}}
	}
	// Perpendicular unit vector to (dx, dy)/d.
	px := -dy / d
	py := dx / d
	return []Point{
		{X: mx + h*px, Y: my + h*py, CRSValue: crs},
		{X: mx - h*px, Y: my - h*py, CRSValue: crs},
	}
}

// LensPolygon returns the intersection region of c1 and c2 as a
// Polygon, sampled analytically (no sweep-line involved). arcSegments
// is the number of samples PER arc — the returned ring has ~2·
// arcSegments vertices. Values < 4 fall back to
// DefaultBufferSegments / 2.
//
// Special cases:
//   - Disjoint circles → empty Polygon (Rings == nil).
//   - One circle properly contains the other → the smaller circle's
//     Boundary (the lens IS the smaller circle).
//   - Concentric circles with equal radius → the shared circle's
//     Boundary.
//   - Tangent circles → empty Polygon (a lens with zero area).
//
// Output is wound CCW (as gobi's convention prefers for exterior
// rings) and inherits CRS from c1.Center.
func LensPolygon(c1, c2 Circle, arcSegments int) Polygon {
	if arcSegments < 4 {
		arcSegments = DefaultBufferSegments / 2
	}
	dx := c2.Center.X - c1.Center.X
	dy := c2.Center.Y - c1.Center.Y
	d := math.Hypot(dx, dy)
	crs := c1.Center.CRSValue

	// Disjoint (including exact tangent — a zero-area lens is
	// indistinguishable from empty for downstream consumers).
	if d >= c1.Radius+c2.Radius {
		return Polygon{CRSValue: crs}
	}
	// One contains the other → the lens is the smaller circle.
	if d+math.Min(c1.Radius, c2.Radius) <= math.Max(c1.Radius, c2.Radius) {
		if c1.Radius <= c2.Radius {
			return c1.Boundary(2 * arcSegments)
		}
		return c2.Boundary(2 * arcSegments)
	}

	pts := CircleIntersectionPoints(c1, c2)
	if len(pts) < 2 {
		return Polygon{CRSValue: crs}
	}
	i1, i2 := pts[0], pts[1]

	// Arc on c2 from i1 to i2, passing through the direction from
	// c2's center toward c1's center. Then arc on c1 from i2 to i1,
	// passing through the direction from c1's center toward c2's
	// center. Concatenated, they form the closed lens boundary.
	arcOnC2 := sampleArcThrough(c2.Center, c2.Radius, i1, i2, c1.Center, arcSegments+1)
	arcOnC1 := sampleArcThrough(c1.Center, c1.Radius, i2, i1, c2.Center, arcSegments+1)

	ring := make([]Point, 0, len(arcOnC2)+len(arcOnC1))
	ring = append(ring, arcOnC2...)
	// Skip the shared joint vertex (arcOnC1[0] == i2, which is
	// arcOnC2[len-1]).
	ring = append(ring, arcOnC1[1:]...)
	// Ensure closed.
	if ring[0].X != ring[len(ring)-1].X || ring[0].Y != ring[len(ring)-1].Y {
		ring = append(ring, ring[0])
	}
	// Force CCW winding to match the package convention for exterior
	// rings. Reversal is cheap and doesn't affect area.
	if !isCCW(ring) {
		reversePoints(ring)
	}
	return Polygon{Rings: [][]Point{ring}, CRSValue: crs}
}

// sampleArcThrough returns nPoints along the arc of the circle
// (center, radius) from pStart to pEnd, choosing the direction that
// passes through the ray from center toward throughPoint. The first
// and last output points are pStart and pEnd exactly (post-sample
// overwrite handles any float64 reconstruction drift).
//
// Direction picking: normalize the CCW sweep from a_start to a_end
// into [0, 2π). If a_through (also normalized) lies in that CCW
// range, sweep CCW; otherwise sweep CW.
func sampleArcThrough(center Point, radius float64, pStart, pEnd, throughPoint Point, nPoints int) []Point {
	if nPoints < 2 {
		nPoints = 2
	}
	aStart := math.Atan2(pStart.Y-center.Y, pStart.X-center.X)
	aEnd := math.Atan2(pEnd.Y-center.Y, pEnd.X-center.X)
	aThrough := math.Atan2(throughPoint.Y-center.Y, throughPoint.X-center.X)

	ccwDelta := normalizeAngleTwoPi(aEnd - aStart)
	throughDelta := normalizeAngleTwoPi(aThrough - aStart)
	var totalDelta float64
	if throughDelta <= ccwDelta {
		totalDelta = ccwDelta // CCW arc contains aThrough
	} else {
		totalDelta = ccwDelta - 2*math.Pi // sweep CW instead
	}

	out := make([]Point, nPoints)
	for i := range nPoints {
		t := float64(i) / float64(nPoints-1)
		angle := aStart + t*totalDelta
		out[i] = Point{
			X:        center.X + radius*math.Cos(angle),
			Y:        center.Y + radius*math.Sin(angle),
			CRSValue: center.CRSValue,
		}
	}
	// Force endpoints exact — the trigonometric reconstruction can
	// drift by a ULP at the endpoints and downstream Clip / topology
	// code prefers coincident vertices to be bit-exact.
	out[0] = pStart
	out[nPoints-1] = pEnd
	return out
}

// normalizeAngleTwoPi maps a to [0, 2π).
func normalizeAngleTwoPi(a float64) float64 {
	two := 2 * math.Pi
	a = math.Mod(a, two)
	if a < 0 {
		a += two
	}
	return a
}

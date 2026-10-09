package geometry

import (
	"fmt"
	"math"
)

// EarthRadiusKM is the mean Earth radius used by haversine calculations.
const EarthRadiusKM = 6371.0088

// Unit represents a linear distance unit.
type Unit string

const (
	UnitMeters        Unit = "m"
	UnitKilometers    Unit = "km"
	UnitMiles         Unit = "mi"
	UnitFeet          Unit = "ft"
	UnitNauticalMiles Unit = "nmi"
)

// MetersPerUnit returns the number of meters in one of the given
// unit. Exported so callers building their own bulk distance
// kernels can hoist the scale factor outside a hot loop instead
// of paying a per-call `metersPerUnit` lookup.
func MetersPerUnit(u Unit) (float64, error) {
	switch u {
	case UnitMeters, "":
		return 1, nil
	case UnitKilometers:
		return 1000, nil
	case UnitMiles:
		return 1609.344, nil
	case UnitFeet:
		return 0.3048, nil
	case UnitNauticalMiles:
		return 1852, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrInvalidUnit, u)
	}
}

// metersPerUnit is the unexported alias kept so the internal call
// sites don't churn. Delegates to MetersPerUnit.
func metersPerUnit(u Unit) (float64, error) { return MetersPerUnit(u) }

// convertMeters converts a value in meters to the specified unit.
func convertMeters(meters float64, u Unit) (float64, error) {
	m, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	return meters / m, nil
}

func degToRad(d float64) float64 { return d * math.Pi / 180 }

// Haversine returns the great-circle distance between two lon/lat
// Points on a sphere of Earth radius, in the requested unit. Point
// X = longitude, Y = latitude (WKB convention); Z / CRS are ignored.
//
// Breaking change in v0.2.16: previously took four float64 args
// (lon1, lat1, lon2, lat2). The new signature aligns with
// HaversineBatch and Point.Distance so callers holding geometry
// types can pass them directly. Migration: replace
// `Haversine(a.X, a.Y, b.X, b.Y, u)` with `Haversine(a, b, u)`.
func Haversine(from, to Point, u Unit) (float64, error) {
	perM, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	φ1 := degToRad(from.Y)
	φ2 := degToRad(to.Y)
	dφ := degToRad(to.Y - from.Y)
	dλ := degToRad(to.X - from.X)
	a := math.Sin(dφ/2)*math.Sin(dφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	distMeters := EarthRadiusKM * 1000 * c
	return distMeters / perM, nil
}

// HaversineBatch returns per-pair great-circle distances between
// from[i] and to[i] in the requested unit. Semantically equivalent
// to calling Haversine(from[i], to[i], u) in a loop, but
// bulk-optimized:
//
//   - The unit conversion factor, Earth-radius constant, and
//     degree-to-radian scale are hoisted outside the inner loop
//     (one metersPerUnit call for the whole batch).
//   - Per-row math runs in a fixed-count vars body — Go's inliner
//     keeps it tight vs. the per-call scalar Haversine which pays
//     a function-call boundary + defer + err-check per row.
//
// Both input slices must be the same length; a mismatch returns
// ErrColumnLenMismatch. Empty slices are legal and return an empty
// (non-nil) result.
//
// CRS on each Point is ignored — Haversine is a lon/lat sphere
// computation that expects Y=latitude, X=longitude in degrees.
// Points in a projected CRS give nonsensical distances; converting
// via ToCRS(WGS84) before calling is the caller's responsibility.
//
// Return type is a flat []float64 — same shape a downstream SIMD
// kernel or arrow builder wants. Nulls in the input aren't
// signaled (Point isn't a nullable type); to skip-and-preserve
// row positions, either pre-filter or pass sentinel points and
// mask the output.
func HaversineBatch(from, to []Point, u Unit) ([]float64, error) {
	if len(from) != len(to) {
		return nil, fmt.Errorf("HaversineBatch: length mismatch: from=%d to=%d",
			len(from), len(to))
	}
	perM, err := metersPerUnit(u)
	if err != nil {
		return nil, err
	}
	// scale converts the great-circle central angle (radians) to
	// the requested output unit. Factored out so the inner loop
	// only pays for the trig, not the constants.
	scale := EarthRadiusKM * 1000 / perM
	const deg2rad = math.Pi / 180

	out := make([]float64, len(from))
	for i := range from {
		p := from[i]
		q := to[i]
		phi1 := p.Y * deg2rad
		phi2 := q.Y * deg2rad
		dphi := (q.Y - p.Y) * deg2rad
		dlam := (q.X - p.X) * deg2rad
		sinHP := math.Sin(dphi / 2)
		sinHL := math.Sin(dlam / 2)
		a := sinHP*sinHP + math.Cos(phi1)*math.Cos(phi2)*sinHL*sinHL
		c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
		out[i] = scale * c
	}
	return out, nil
}

// Euclidean returns the planar distance between two Points, in the
// requested unit. The input coordinates are assumed to already be
// in meters (projected CRS); Z / CRS are ignored.
//
// Breaking change in v0.2.16: previously took four float64 args
// (x1, y1, x2, y2). The new signature aligns with Haversine +
// HaversineBatch + Point.Distance. Migration: replace
// `Euclidean(a.X, a.Y, b.X, b.Y, u)` with `Euclidean(a, b, u)`.
func Euclidean(from, to Point, u Unit) (float64, error) {
	dx := to.X - from.X
	dy := to.Y - from.Y
	return convertMeters(math.Sqrt(dx*dx+dy*dy), u)
}

// PointToSegmentDistanceSqXY returns the squared Euclidean distance
// from (px, py) to the closed line segment ((ax, ay), (bx, by)).
// Handles a == b (zero-length segment) as squared point-to-point
// distance.
//
// # Why squared
//
// Min-distance loops repeatedly compare distances and only need
// the sqrt on the final answer. The classic AoS
// pointToSegmentDistance (in this file) calls math.Hypot per
// segment; on a polygon×polygon distance with ~100 vertices each,
// that's 10k sqrts per per-row call. Squared form defers to a
// single sqrt at the outermost call — see planarMinDistance's
// slab-form rewrite (Slice 11).
//
// The formula is the standard projection-onto-line-segment:
//
//	t = ((p-a) · (b-a)) / |b-a|²   clamped to [0, 1]
//	f = a + t·(b-a)                 nearest point on segment
//	d² = (p.x - f.x)² + (p.y - f.y)²
//
// When |b-a|² == 0 the segment is a single point; return
// squared distance from p to a directly.
func PointToSegmentDistanceSqXY(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		ex, ey := px-ax, py-ay
		return ex*ex + ey*ey
	}
	t := ((px-ax)*dx + (py-ay)*dy) / lenSq
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	fx := ax + t*dx
	fy := ay + t*dy
	ex, ey := px-fx, py-fy
	return ex*ex + ey*ey
}

// PointToPolylineMinDistanceSq returns the minimum squared
// Euclidean distance from (px, py) to any point on the polyline
// held in parallel Xs / Ys slabs. When closed=true the closing
// segment (last, first) is also considered — matching the ring
// closure that Polygon.Segments enumerates.
//
// Empty polyline returns math.Inf(1) (no segments to compare
// against). Single-point polyline returns squared distance from
// (px, py) to that single vertex.
//
// Zero-alloc; single pass over the polyline slabs.
func PointToPolylineMinDistanceSq(px, py float64, xs, ys []float64, closed bool) float64 {
	n := min(len(xs), len(ys))
	if n == 0 {
		return math.Inf(1)
	}
	if n == 1 {
		ex, ey := px-xs[0], py-ys[0]
		return ex*ex + ey*ey
	}
	best := math.Inf(1)
	for i := 0; i < n-1; i++ {
		d2 := PointToSegmentDistanceSqXY(px, py, xs[i], ys[i], xs[i+1], ys[i+1])
		if d2 < best {
			best = d2
		}
	}
	if closed {
		d2 := PointToSegmentDistanceSqXY(px, py, xs[n-1], ys[n-1], xs[0], ys[0])
		if d2 < best {
			best = d2
		}
	}
	return best
}

// distanceGeometry is the slab-form representation of a geometry
// for the SoA min-distance kernel. Every input geometry is
// flattened into two collections:
//
//   - `points`: standalone vertex slabs from Point / MultiPoint
//     inputs, plus any degenerate < 2-point lines. Used for the
//     vertex-to-vertex fallback branch.
//   - `polylines`: PointsView slabs for each polyline (rings from
//     polygons, lines from LineString / MultiLineString). The
//     `closed` slice parallels `polylines` and marks which entries
//     get the (n-1 → 0) closing segment appended.
//
// Materialized once per (geometry, geometry) distance call;
// avoids the forEachVertex/forEachSegment closure allocations
// the AoS path pays per row.
type distanceGeometry struct {
	pointXs   []float64
	pointYs   []float64
	polylines []PointsView
	closed    []bool
}

// extractDistanceGeometry walks g and populates a distanceGeometry
// with fresh slabs. Empty input (nil g, empty containers) produces
// an empty distanceGeometry. Recurses into GeometryCollection.
func extractDistanceGeometry(g Geometry, dst *distanceGeometry) {
	switch t := g.(type) {
	case Point:
		dst.pointXs = append(dst.pointXs, t.X)
		dst.pointYs = append(dst.pointYs, t.Y)
	case MultiPoint:
		for _, p := range t.Points {
			dst.pointXs = append(dst.pointXs, p.X)
			dst.pointYs = append(dst.pointYs, p.Y)
		}
	case LineString:
		if len(t.Points) < 2 {
			for _, p := range t.Points {
				dst.pointXs = append(dst.pointXs, p.X)
				dst.pointYs = append(dst.pointYs, p.Y)
			}
			return
		}
		dst.polylines = append(dst.polylines, t.View())
		dst.closed = append(dst.closed, false)
	case MultiLineString:
		for _, l := range t.Lines {
			if len(l.Points) < 2 {
				for _, p := range l.Points {
					dst.pointXs = append(dst.pointXs, p.X)
					dst.pointYs = append(dst.pointYs, p.Y)
				}
				continue
			}
			dst.polylines = append(dst.polylines, l.View())
			dst.closed = append(dst.closed, false)
		}
	case Polygon:
		for _, ring := range t.Rings {
			if len(ring) == 0 {
				continue
			}
			if len(ring) == 1 {
				dst.pointXs = append(dst.pointXs, ring[0].X)
				dst.pointYs = append(dst.pointYs, ring[0].Y)
				continue
			}
			dst.polylines = append(dst.polylines, viewFromPoints(ring, false, t.CRSValue))
			// Polygon rings are logically closed; the closing
			// segment is added by the kernel via closed=true, so
			// callers don't need to duplicate the first vertex.
			// Rings that came in already-closed still get the
			// extra segment considered but the (last, first) pair
			// is zero-length in that case and folds to 0.
			dst.closed = append(dst.closed, true)
		}
	case MultiPolygon:
		for _, poly := range t.Polygons {
			extractDistanceGeometry(poly, dst)
		}
	case GeometryCollection:
		for _, inner := range t.Geometries {
			extractDistanceGeometry(inner, dst)
		}
	}
}

// planarMinDistanceSquared runs the SoA min-distance nested loop
// over the extracted slab-form representations of a and b. Uses
// PointToPolylineMinDistanceSq inline and defers sqrt to the
// caller. Returns math.Inf(1) if both sides are empty.
//
// # Loop structure
//
// For each vertex in a's flat vertex slab: min-distance to every
// polyline in b (and vertex-to-vertex to every vertex in b).
// Same swapped. This matches the AoS planarMinDistance
// symmetric-loop shape.
//
// Point-to-point fallback: when neither side has any polylines
// (both are Point / MultiPoint or degenerate lines), the outer
// polyline loops don't produce any comparisons — the vertex-to-
// vertex sub-loop handles it.
func planarMinDistanceSquared(a, b *distanceGeometry) float64 {
	best := math.Inf(1)
	// a's vertices vs b's polylines + b's vertices.
	for i := range a.pointXs {
		px, py := a.pointXs[i], a.pointYs[i]
		for j, pl := range b.polylines {
			d2 := PointToPolylineMinDistanceSq(px, py, pl.Xs, pl.Ys, b.closed[j])
			if d2 < best {
				best = d2
			}
		}
		for k := range b.pointXs {
			ex := px - b.pointXs[k]
			ey := py - b.pointYs[k]
			d2 := ex*ex + ey*ey
			if d2 < best {
				best = d2
			}
		}
	}
	// b's vertices vs a's polylines. (b's vertices vs a's vertices
	// already covered by the loop above via symmetry.)
	for i := range b.pointXs {
		px, py := b.pointXs[i], b.pointYs[i]
		for j, pl := range a.polylines {
			d2 := PointToPolylineMinDistanceSq(px, py, pl.Xs, pl.Ys, a.closed[j])
			if d2 < best {
				best = d2
			}
		}
	}
	// a's polyline vertices vs b's polylines.
	for _, apl := range a.polylines {
		for i := range apl.Xs {
			px, py := apl.Xs[i], apl.Ys[i]
			for j, bpl := range b.polylines {
				d2 := PointToPolylineMinDistanceSq(px, py, bpl.Xs, bpl.Ys, b.closed[j])
				if d2 < best {
					best = d2
				}
			}
		}
	}
	// b's polyline vertices vs a's polylines.
	for _, bpl := range b.polylines {
		for i := range bpl.Xs {
			px, py := bpl.Xs[i], bpl.Ys[i]
			for j, apl := range a.polylines {
				d2 := PointToPolylineMinDistanceSq(px, py, apl.Xs, apl.Ys, a.closed[j])
				if d2 < best {
					best = d2
				}
			}
		}
	}
	return best
}

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
//
// Slice-11 SoA rewrite: extracts each input's polylines + vertices
// into slab form once, then runs a slab-based nested loop that
// tracks running-min *squared* distance and calls sqrt exactly
// once at the end. Replaces the AoS forEachVertex/forEachSegment
// closure walk, which paid one math.Hypot per (vertex, segment)
// pair — for a Polygon×Polygon distance on ~100-vertex inputs
// that's 10k Hypot calls (10k sqrts) per row, all discarded
// except the minimum.
func planarMinDistance(a, b Geometry) float64 {
	var ag, bg distanceGeometry
	extractDistanceGeometry(a, &ag)
	extractDistanceGeometry(b, &bg)
	best := planarMinDistanceSquared(&ag, &bg)
	if math.IsInf(best, 1) {
		return 0
	}
	return math.Sqrt(best)
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

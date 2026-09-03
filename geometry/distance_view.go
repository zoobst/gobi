package geometry

import "math"

// PointToSegmentDistanceSqXY returns the squared Euclidean distance
// from (px, py) to the closed line segment ((ax, ay), (bx, by)).
// Handles a == b (zero-length segment) as squared point-to-point
// distance.
//
// # Why squared
//
// Min-distance loops repeatedly compare distances and only need
// the sqrt on the final answer. The classic AoS
// pointToSegmentDistance in distance_geom.go calls math.Hypot per
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

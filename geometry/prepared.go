package geometry

// PreparedGeometry wraps a Geometry with precomputed SoA views +
// a cached bounds for accelerated repeated predicate evaluation.
// The primary use case is spatial joins where each right-side
// polygon is tested against many left-side candidate points —
// materializing the SoA views once amortizes the O(n) AoS→SoA
// conversion tax across every predicate call.
//
// # When to use
//
// Prepare + TestPrepared wins on hot loops where the amortization
// ratio — number of predicate calls per prepared geometry —
// exceeds the break-even point measured on the target shape. The
// amortized-view microbench in pip_bench_test.go shows a 3× win
// on a 64-vertex polygon with 100 candidate points held-view; the
// break-even is around 5-10 candidates per polygon for that
// shape.
//
// **Important**: gobi's built-in SJoin does NOT use PreparedGeometry.
// The R-tree pre-filter in SJoin is effective enough on
// non-overlapping polygon workloads that each polygon typically
// gets ~1 candidate point on average — well below the break-even
// where Prepare's up-front view materialization pays off. Callers
// with denser workloads (overlapping polygons, spatial-index-free
// refine loops, per-polygon many-candidate tests) SHOULD use
// PreparedGeometry directly.
//
// Rule of thumb: Prepare + TestPrepared beats Test when you'll
// evaluate the predicate against the same geometry ≥10 times.
// Below that, the plain Test path (which skips the up-front
// materialization) is faster.
//
// The SoA fast paths kick in for the following pair shapes,
// covering the dominant spatial-join workloads:
//
//   - Point × Polygon (both orderings) — PredContains, PredIntersects,
//     PredWithin. Uses PIPPolygonFromRings on the polygon's cached
//     ring views. Boundary points fall through to the AoS path via
//     pointOnPolygonBoundary so results match Test() exactly.
//   - Point × MultiPolygon (both orderings) — same predicates.
//     Iterates the polygon views with early-exit on first match.
//
// Every other shape (Point×Point, LineString×Polygon,
// Polygon×Polygon, etc.) transparently falls through to Test(a.G, b.G)
// so callers can pass a PreparedGeometry into TestPrepared without
// worrying about which shapes are optimized. Adding a new fast
// path only requires a case in TestPrepared and matching correctness
// tests — no changes to callers.
//
// # Memory
//
// Prepare(polygon) materializes fresh Xs / Ys slabs for every ring
// (see PointsView docstring). Memory usage roughly doubles for the
// polygon corpus during the operation; the win comes from
// eliminating per-call throwaway allocations in the refine loop.
// For SJoin against 10k right polygons of ~10 vertices each,
// materialization costs ~1.6 MB total; per-call throwaway
// allocations at ~10 candidates per left row across 100k rows
// would otherwise be 100× that.
type PreparedGeometry struct {
	// G is the source geometry. Retained so the fallback Test(G, other)
	// path stays available for shapes without a fast path.
	G Geometry

	// Bounds is g.Bounds() cached at Prepare time. Used for the
	// cheap bbox-reject step in TestPrepared without a repeat
	// Bounds() call per predicate evaluation.
	Bounds Bounds

	// polyRings holds the exterior + hole PointsViews when G is a
	// Polygon. rings[0] is the exterior; rings[1:] are holes.
	// Nil for non-Polygon geometries.
	polyRings []PointsView

	// multiPolyRings holds one []PointsView per sub-polygon when G
	// is a MultiPolygon. Outer index selects the polygon; inner
	// slice follows the polygon's ring layout (exterior first,
	// then holes). Nil for non-MultiPolygon geometries.
	multiPolyRings [][]PointsView
}

// Prepare returns a PreparedGeometry for g. For Polygon and
// MultiPolygon inputs this materializes the RingViews upfront so
// downstream TestPrepared calls skip the AoS []Point walk on the
// point-in-polygon fast paths.
//
// For every other geometry type Prepare only caches the bounds —
// the fast-path table doesn't have entries for non-polygon
// geometries yet, so materializing views up front would be pure
// overhead. Adding a fast path for e.g. LineString×LineString
// intersection means (1) add a PointsView cache field, (2)
// populate it here, (3) add a TestPrepared case that consumes it.
func Prepare(g Geometry) PreparedGeometry {
	p := PreparedGeometry{G: g}
	if g == nil {
		return p
	}
	p.Bounds = g.Bounds()
	switch t := g.(type) {
	case Polygon:
		p.polyRings = t.RingViews()
	case MultiPolygon:
		p.multiPolyRings = t.PolygonRingViews()
	}
	return p
}

// TestPrepared evaluates pred on the ordered pair (a, b) using
// precomputed SoA views when the pair shape has a fast path.
// Semantically equivalent to Test(pred, a.G, b.G).
//
// Nil geometry on either side returns false for every predicate
// (matches Test). Non-fast-path shapes fall through to Test — the
// only cost of using TestPrepared over Test on those shapes is
// the bounds-reject double-check + the type-switch overhead
// (single-digit nanoseconds per call).
func TestPrepared(pred Predicate, a, b PreparedGeometry) bool {
	if a.G == nil || b.G == nil {
		return false
	}
	// Cheap bbox reject for the intersects/contains-family. Skipped
	// for PredDisjoint (whose short-circuit is inverted).
	switch pred {
	case PredIntersects, PredContains, PredWithin, PredTouches, PredCrosses, PredOverlaps:
		if !boundsCompatible(pred, a.Bounds, b.Bounds) {
			return false
		}
	}

	// Fast path: Point × Polygon / MultiPolygon (both orderings).
	// PredContains / PredIntersects / PredWithin cover the SJoin
	// dominant workloads. Boundary-inclusive semantics preserved
	// via a fall-through to pointOnPolygonBoundary on Contains-false.
	if aPt, ok := a.G.(Point); ok {
		if hit, done := testPointVsPolygonPrepared(pred, aPt, b, false); done {
			return hit
		}
	}
	if bPt, ok := b.G.(Point); ok {
		if hit, done := testPointVsPolygonPrepared(pred, bPt, a, true); done {
			return hit
		}
	}

	// Fall through to the AoS path for everything else. Includes
	// PredDisjoint (implemented as !Intersects, no dedicated
	// fast path yet) and every non-point-vs-polygon combination.
	return Test(pred, a.G, b.G)
}

// testPointVsPolygonPrepared evaluates pred on (Point, Polygon-or-
// MultiPolygon) using SoA views. swapped=true means the input
// pair was (poly, pt) — the predicate is inverted accordingly so
// the caller can pass either ordering without pre-swapping.
//
// Returns done=false when the polySide geometry isn't a Polygon /
// MultiPolygon — caller falls through to Test.
func testPointVsPolygonPrepared(pred Predicate, pt Point, polySide PreparedGeometry, swapped bool) (hit bool, done bool) {
	// PredWithin(pt, poly) ≡ PredContains(poly, pt). Normalize by
	// treating "does the polygon contain the point" as the primary
	// question and re-mapping the predicate.
	//
	//	original (a, b)     effective question
	//	Contains(pt, poly)  -> no fast path (pt can't contain a poly);
	//	                       fall through
	//	Contains(poly, pt)  -> "poly contains pt"
	//	Within(pt, poly)    -> "poly contains pt"
	//	Within(poly, pt)    -> no fast path
	//	Intersects(_, _)    -> "pt lies inside poly (interior or boundary)"
	//	Disjoint            -> unhandled — falls through
	var question containmentQuestion
	switch pred {
	case PredIntersects:
		question = qContainsOrBoundary
	case PredContains:
		if swapped {
			// original was Contains(poly, pt) — polygon side is 'a' ==
			// polySide. Question: does polySide contain pt?
			question = qContainsOrBoundary
		} else {
			// original was Contains(pt, poly) — a point can't contain
			// a polygon (except a degenerate one). AoS Test returns false;
			// fall through.
			return false, false
		}
	case PredWithin:
		if swapped {
			// original was Within(poly, pt) — no fast path.
			return false, false
		}
		// original was Within(pt, poly) ≡ Contains(poly, pt).
		question = qContainsOrBoundary
	default:
		return false, false
	}

	switch {
	case polySide.polyRings != nil:
		return polygonPreparedContainsPoint(polySide.polyRings, pt, question, polySide.G), true
	case polySide.multiPolyRings != nil:
		return multiPolygonPreparedContainsPoint(polySide.multiPolyRings, pt, question, polySide.G), true
	}
	// polySide is not a polygon-typed geometry — fall through.
	return false, false
}

// containmentQuestion enumerates the interior/boundary semantics
// the SoA-fast-path caller wants. qContainsOrBoundary matches
// the AoS pointInPolygon shape (interior OR on-boundary).
type containmentQuestion uint8

const (
	qContainsOrBoundary containmentQuestion = iota
)

// polygonPreparedContainsPoint runs the SoA PIP against the
// precomputed rings; falls back to pointOnPolygonBoundary via the
// original Polygon.G when qContainsOrBoundary is requested and the
// PIP was false (matches AoS pointInPolygon exactly).
func polygonPreparedContainsPoint(rings []PointsView, pt Point, q containmentQuestion, srcG Geometry) bool {
	if PIPPolygonFromRings(rings, pt.X, pt.Y) {
		return true
	}
	if q == qContainsOrBoundary {
		if poly, ok := srcG.(Polygon); ok {
			return pointOnPolygonBoundary(pt, poly)
		}
	}
	return false
}

// multiPolygonPreparedContainsPoint iterates the constituent
// polygons with early-exit; falls back to boundary check on the
// AoS geometry only if requested and every SoA interior test was
// false.
func multiPolygonPreparedContainsPoint(polys [][]PointsView, pt Point, q containmentQuestion, srcG Geometry) bool {
	for _, rings := range polys {
		if PIPPolygonFromRings(rings, pt.X, pt.Y) {
			return true
		}
	}
	if q == qContainsOrBoundary {
		if mp, ok := srcG.(MultiPolygon); ok {
			for _, sub := range mp.Polygons {
				if pointOnPolygonBoundary(pt, sub) {
					return true
				}
			}
		}
	}
	return false
}

// BoundsCompatible reports whether the two bounds are compatible
// under pred's necessary-condition. Wrong answers here would
// falsely reject valid predicate matches, so err on the side of
// "compatible" for any predicate we don't specifically handle.
//
// Used by TestPrepared internally and by the per-row bbox-reject
// fast paths in Series predicate ops (which read row bboxes via
// BoundsFromWKB and can short-circuit false without a full
// ParseWKB when this returns false).
//
// PredIntersects / PredTouches / PredCrosses / PredOverlaps:
// bboxes must overlap.
// PredContains: a's bbox must cover b's bbox.
// PredWithin: b's bbox must cover a's bbox.
func BoundsCompatible(pred Predicate, ab, bb Bounds) bool { return boundsCompatible(pred, ab, bb) }

func boundsCompatible(pred Predicate, ab, bb Bounds) bool {
	if ab.Empty() || bb.Empty() {
		// Empty geometries never satisfy any positive predicate;
		// Test() defers to per-type logic which returns false, so
		// matching by refusing bounds-compatibility is safe.
		return false
	}
	switch pred {
	case PredContains:
		return ab.MinX <= bb.MinX && ab.MinY <= bb.MinY &&
			ab.MaxX >= bb.MaxX && ab.MaxY >= bb.MaxY
	case PredWithin:
		return bb.MinX <= ab.MinX && bb.MinY <= ab.MinY &&
			bb.MaxX >= ab.MaxX && bb.MaxY >= ab.MaxY
	default:
		return ab.Intersects(bb)
	}
}

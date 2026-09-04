package geometry

import (
	"sync"
	"sync/atomic"
)

// mpTreeSearchBufPool amortizes the RTree.Search result-slice
// allocation across MultiPolygon prepared queries. Each Get
// returns a *[]int32 (pointer-to-slice, per sync.Pool best
// practice — storing []int32 directly boxes the slice header
// on every Put, which defeats the pool's purpose).
//
// Callers drain the buffer with SearchInto (which truncates to
// zero length + appends), then Put the pointer back on the
// exit path. The pool retains the largest capacity seen, so
// hot loops converge on a single reused backing array per
// worker goroutine.
//
// The initial cap of 16 handles the typical MultiPolygon point
// query (0-2 candidate polygons) without a first-call grow.
// Larger candidate lists (overlapping polys) grow through
// append and the pool retains the grown capacity.
var mpTreeSearchBufPool = sync.Pool{
	New: func() any {
		buf := make([]int32, 0, 16)
		return &buf
	},
}

// mpTreeSearchBufAcquire / mpTreeSearchBufRelease wrap the pool
// Get/Put so callers can spell the lifecycle as
//
//	bufPtr := mpTreeSearchBufAcquire()
//	defer mpTreeSearchBufRelease(bufPtr)
//
// rather than duplicating the reset-then-Put closure inline at
// every use site. Kept as non-closure functions so the defer
// stays open-coded (stack-allocated) and matches the inline
// idiom's overhead.
func mpTreeSearchBufAcquire() *[]int32 {
	return mpTreeSearchBufPool.Get().(*[]int32)
}

func mpTreeSearchBufRelease(bufPtr *[]int32) {
	*bufPtr = (*bufPtr)[:0]
	mpTreeSearchBufPool.Put(bufPtr)
}

// mpTreeMinSubPolys is the sub-polygon count at which Prepare(MP)
// builds an R-tree over the sub-polygon bboxes. Below this the
// linear scan through the bbox slab is faster (cache-friendly,
// no tree-walk overhead); above this the O(log N + k) tree lookup
// dominates. Measured break-even is workload-dependent but sits
// in the 8-32 range for typical point-in-MP shapes; 16 is a
// conservative pick that keeps the linear path for small
// MultiPolygons (2-3 sub-polys) where the tree would be pure
// overhead.
const mpTreeMinSubPolys = 16

// PreparedGeometry wraps a Geometry with precomputed indexes +
// cached bounds for accelerated repeated predicate evaluation.
// The primary use case is spatial joins where each right-side
// polygon is tested against many left-side candidate points —
// caching the indexes once amortizes the per-call setup tax
// across every predicate call.
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
// # Cost model
//
//   - Polygon: Prepare materializes ring PointsViews upfront (2
//     allocs per ring). Query cost is one PIP walk per call.
//   - MultiPolygon: Prepare caches one Bounds per sub-polygon
//     (single small slab) and — when N ≥ 16 — builds an R-tree over
//     those bboxes. Per-sub-polygon ring views are populated LAZILY
//     on first hit (atomic publish; concurrent readers race
//     benignly). Query cost is O(log N + k) via the tree, or O(N)
//     bbox-compares + k PIPs via the linear scan.
//
// The lazy MultiPolygon path is what makes Prepare(landMP) with
// hundreds of small islands cheap: only bbox slabs are allocated
// upfront (24 bytes × N), and only sub-polygons that a query
// actually touches ever pay their ring-view cost. The pre-review
// implementation materialized every ring of every sub-poly at
// Prepare time, which was a strict regression on many-small-poly
// shapes because the upfront work dwarfed the per-query savings.
//
// # Fast paths
//
// The SoA fast paths kick in for the following pair shapes,
// covering the dominant spatial-join workloads:
//
//   - Point × Polygon (both orderings) — PredContains, PredIntersects,
//     PredWithin. Uses PIPPolygonFromRings on the polygon's cached
//     ring views. Boundary points fall through to the AoS path via
//     pointOnPolygonBoundary so results match Test() exactly.
//   - Point × MultiPolygon (both orderings) — same predicates.
//     Tree-indexed (N ≥ 16) or linear-with-bbox-reject (N < 16),
//     then per-candidate PIP with lazy ring materialization.
//
// Every other shape (Point×Point, LineString×Polygon,
// Polygon×Polygon, etc.) transparently falls through to Test(a.G, b.G)
// so callers can pass a PreparedGeometry into TestPrepared without
// worrying about which shapes are optimized. Adding a new fast
// path only requires a case in TestPrepared and matching correctness
// tests — no changes to callers.
//
// # gobi's built-in SJoin
//
// SJoin does NOT use PreparedGeometry today: its R-tree pre-filter
// on the RIGHT frame drives the candidate ratio to ~1 point per
// right-polygon on non-overlapping workloads, well below the
// amortization break-even. Callers with denser workloads
// (overlapping polygons, spatial-index-free refine loops,
// per-polygon many-candidate tests) SHOULD use PreparedGeometry
// directly.
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
	// Materialized eagerly at Prepare time — polygons typically
	// have 1-few rings so the upfront cost is negligible.
	// Nil for non-Polygon geometries.
	polyRings []PointsView

	// mpSubBounds holds one bbox per sub-polygon when G is a
	// MultiPolygon. Always populated (small: 32 bytes per sub-poly).
	// Nil for non-MultiPolygon geometries.
	mpSubBounds []Bounds

	// mpSubRings holds lazy ring-view slots parallel to mpSubBounds.
	// A nil load means "not yet materialized"; a non-nil load returns
	// the ring views. Concurrent readers on the same slot race
	// benignly — atomic.Pointer.Store publishes the winning slice
	// and the loser's allocation is GC'd. Both writes are semantically
	// equivalent (same rings, same order).
	mpSubRings []atomic.Pointer[mpRingSlot]

	// mpTree indexes mpSubBounds for O(log N + k) candidate lookup.
	// Nil when len(mpSubBounds) < mpTreeMinSubPolys — the linear
	// bbox-reject scan is faster below the threshold.
	mpTree *RTree
}

// mpRingSlot is the payload of a single atomic.Pointer[mpRingSlot]
// entry in PreparedGeometry.mpSubRings. Wrapping []PointsView in a
// named struct rather than storing atomic.Pointer[[]PointsView]
// directly avoids the sub-slice-header allocation quirk (atomic
// pointer to a slice header stored on the heap) and keeps the
// atomic load path a plain pointer chase.
type mpRingSlot struct {
	rings []PointsView
}

// Prepare returns a PreparedGeometry for g.
//
// Polygon: materializes ring views upfront.
// MultiPolygon: caches per-sub-polygon bounds; builds an R-tree
// index when N ≥ 16; leaves per-sub-polygon ring views nil for
// lazy on-demand materialization.
// Every other geometry type: bounds only. The fast-path table
// doesn't have entries for non-polygon geometries yet, so
// materializing views up front would be pure overhead.
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
		n := len(t.Polygons)
		if n == 0 {
			return p
		}
		p.mpSubBounds = make([]Bounds, n)
		p.mpSubRings = make([]atomic.Pointer[mpRingSlot], n)
		for i, sub := range t.Polygons {
			p.mpSubBounds[i] = sub.Bounds()
		}
		if n >= mpTreeMinSubPolys {
			p.mpTree = NewRTree(p.mpSubBounds)
		}
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
	case polySide.mpSubBounds != nil:
		return multiPolygonPreparedContainsPoint(polySide, pt, question), true
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

// multiPolygonPreparedContainsPoint drives the MultiPolygon fast
// path. Wraps mpQueryPointCore with a pooled scratch buffer for
// the RTree Search result — the pool amortizes the alloc across
// per-call queries. Batch callers (TestPointsPrepared) should
// invoke the core directly with a locally-held scratch to avoid
// pool Get/Put per point.
//
// Two-stage inside the core:
//
//  1. Candidate lookup: R-tree Search when the index exists,
//     otherwise a linear bbox-reject scan over mpSubBounds. Both
//     restrict subsequent work to sub-polygons whose bbox
//     actually contains the query point — for non-overlapping
//     island geometries the candidate list is typically 0 or 1.
//  2. Per-candidate PIP: ring views are loaded via loadSubRings
//     (materializes on first hit and caches atomically), then
//     PIPPolygonFromRings runs the standard SoA even-odd test.
//     Boundary-inclusive fallback (pointOnPolygonBoundary on the
//     source AoS Polygon) fires only when the interior test misses
//     AND the caller requested qContainsOrBoundary.
//
// Skipping ring materialization for bbox-miss sub-polygons is
// what makes many-small-polys shapes (landMP, admin boundaries)
// cheap: allocs scale with hits, not with total sub-polygon count.
func multiPolygonPreparedContainsPoint(p PreparedGeometry, pt Point, q containmentQuestion) bool {
	if len(p.mpSubBounds) == 0 {
		return false
	}
	mp, ok := p.G.(MultiPolygon)
	if !ok {
		return false
	}
	if p.mpTree == nil {
		// Linear scan path takes no scratch — go direct.
		return mpQueryPointCore(p, mp, pt.X, pt.Y, q, nil)
	}
	// Tree path: pool the RTree.Search scratch. mpQueryPointCore
	// updates *bufPtr to reflect any append growth; the release
	// helper resets len to 0 (preserves cap) and returns it to
	// the pool.
	bufPtr := mpTreeSearchBufAcquire()
	defer mpTreeSearchBufRelease(bufPtr)
	return mpQueryPointCore(p, mp, pt.X, pt.Y, q, bufPtr)
}

// mpQueryPointCore is the shared body for single-point and batch
// MultiPolygon prepared queries. `searchBuf` is the caller's
// held scratch for the RTree Search result — nil is allowed and
// forces the linear path (only used when p.mpTree == nil).
//
// Callers pass a pointer so the function can update the slice
// after append growth without needing a return value for the
// scratch. Concurrent-safe against the prep (atomic ring-view
// slots), but the searchBuf itself must not be shared across
// goroutines.
func mpQueryPointCore(p PreparedGeometry, mp MultiPolygon, x, y float64, q containmentQuestion, searchBuf *[]int32) bool {
	checkOne := func(i int) bool {
		rings := loadSubRings(p, i, mp)
		if PIPPolygonFromRings(rings, x, y) {
			return true
		}
		if q == qContainsOrBoundary {
			return pointOnPolygonBoundary(Point{X: x, Y: y}, mp.Polygons[i])
		}
		return false
	}
	if p.mpTree != nil && searchBuf != nil {
		qb := Bounds{MinX: x, MaxX: x, MinY: y, MaxY: y}
		hits := p.mpTree.SearchInto(*searchBuf, qb)
		*searchBuf = hits
		for _, ci := range hits {
			if checkOne(int(ci)) {
				return true
			}
		}
		return false
	}
	// Linear scan with bbox reject.
	for i, b := range p.mpSubBounds {
		if !b.Contains(x, y) {
			continue
		}
		if checkOne(i) {
			return true
		}
	}
	return false
}

// TestPointPrepared evaluates pred on (Point{x, y}, prep.G) without
// the interface-boxing allocation that Prepare(Point{...}) + TestPrepared
// would incur. The atomic query underlying TestPointsPrepared — the
// batch API is essentially a loop over this with a held R-tree scratch.
//
// Fast-path shapes (same as TestPrepared, non-swapped ordering):
//   - Point × Polygon: pred ∈ {Intersects, Within}
//   - Point × MultiPolygon: pred ∈ {Intersects, Within}
//
// Every other pred / prep shape falls through to per-call Test — no
// panic, no error, just AoS semantics.
//
// Concurrent-safe against the prep (atomic lazy slots).
func TestPointPrepared(pred Predicate, x, y float64, prep PreparedGeometry) bool {
	if prep.G == nil {
		return false
	}
	if pred != PredIntersects && pred != PredWithin {
		return Test(pred, Point{X: x, Y: y}, prep.G)
	}
	q := qContainsOrBoundary
	switch {
	case prep.polyRings != nil:
		b := prep.Bounds
		if x < b.MinX || x > b.MaxX || y < b.MinY || y > b.MaxY {
			return false
		}
		if PIPPolygonFromRings(prep.polyRings, x, y) {
			return true
		}
		if poly, ok := prep.G.(Polygon); ok {
			return pointOnPolygonBoundary(Point{X: x, Y: y}, poly)
		}
		return false
	case prep.mpSubBounds != nil:
		mp, ok := prep.G.(MultiPolygon)
		if !ok {
			return false
		}
		b := prep.Bounds
		if x < b.MinX || x > b.MaxX || y < b.MinY || y > b.MaxY {
			return false
		}
		if prep.mpTree == nil {
			return mpQueryPointCore(prep, mp, x, y, q, nil)
		}
		bufPtr := mpTreeSearchBufAcquire()
		defer mpTreeSearchBufRelease(bufPtr)
		return mpQueryPointCore(prep, mp, x, y, q, bufPtr)
	default:
		return Test(pred, Point{X: x, Y: y}, prep.G)
	}
}

// TestPointsPrepared evaluates pred for each (Point{xs[i], ys[i]},
// prep.G) pair, writing results into out. Semantics match a per-
// index Test(pred, Point{X: xs[i], Y: ys[i]}, prep.G).
//
// The batch amortizes prep-side work across every point:
//   - Ring views / R-tree / bounds are materialized once by
//     Prepare and shared across the entire call.
//   - No per-point interface-boxing of the point into a
//     Geometry (single-point TestPrepared costs one alloc per
//     Prepare(Point{...})).
//   - MultiPolygon R-tree Search scratch is held for the whole
//     batch, not Get/Put'd per point.
//   - Bbox reject is inlined into the loop with no dispatch
//     overhead.
//
// Fast-path shapes (same as TestPrepared, non-swapped ordering):
//   - Point × Polygon: pred ∈ {Intersects, Within}
//   - Point × MultiPolygon: pred ∈ {Intersects, Within}
//
// Every other pred (Contains, Disjoint, ...) or prep shape falls
// through to per-point Test — correctness preserved, no batch
// speedup.
//
// len(xs) must equal len(ys); len(out) must be ≥ len(xs). Panics
// on mismatch (matches compute-package convention).
//
// Concurrent-safe against the prep — atomic lazy slots let
// multiple goroutines batch-query the same prep. The out buffer
// obviously must not be shared across goroutines.
func TestPointsPrepared(pred Predicate, xs, ys []float64, prep PreparedGeometry, out []bool) {
	if len(xs) != len(ys) {
		panic("geometry: TestPointsPrepared: xs and ys length mismatch")
	}
	if len(out) < len(xs) {
		panic("geometry: TestPointsPrepared: out length < xs length")
	}
	if prep.G == nil {
		for i := range xs {
			out[i] = false
		}
		return
	}

	// PredIntersects and PredWithin both map to "does prep.G contain
	// the point (interior or boundary)". Every other pred takes the
	// per-point Test fallback — Contains(pt, poly) is trivially
	// false, Disjoint is !Intersects, Touches/Crosses/Overlaps have
	// no batch fast path.
	if pred != PredIntersects && pred != PredWithin {
		for i := range xs {
			out[i] = Test(pred, Point{X: xs[i], Y: ys[i]}, prep.G)
		}
		return
	}
	q := qContainsOrBoundary

	switch {
	case prep.polyRings != nil:
		testPointsVsPolygonBatch(prep, xs, ys, q, out)
	case prep.mpSubBounds != nil:
		testPointsVsMultiPolygonBatch(prep, xs, ys, q, out)
	default:
		// Non-poly prep — no fast path available.
		for i := range xs {
			out[i] = Test(pred, Point{X: xs[i], Y: ys[i]}, prep.G)
		}
	}
}

// testPointsVsPolygonBatch runs the Polygon fast path over xs/ys.
// Bbox reject inlined; PIP on cached rings; boundary fallback on
// the source AoS Polygon.
func testPointsVsPolygonBatch(prep PreparedGeometry, xs, ys []float64, q containmentQuestion, out []bool) {
	b := prep.Bounds
	poly, hasPoly := prep.G.(Polygon)
	for i := range xs {
		x, y := xs[i], ys[i]
		if x < b.MinX || x > b.MaxX || y < b.MinY || y > b.MaxY {
			out[i] = false
			continue
		}
		if PIPPolygonFromRings(prep.polyRings, x, y) {
			out[i] = true
			continue
		}
		if q == qContainsOrBoundary && hasPoly {
			out[i] = pointOnPolygonBoundary(Point{X: x, Y: y}, poly)
			continue
		}
		out[i] = false
	}
}

// testPointsVsMultiPolygonBatch runs the MultiPolygon fast path
// over xs/ys. Holds ONE RTree Search scratch buffer across the
// whole batch — the pool-Get/Put per point that
// multiPolygonPreparedContainsPoint pays gets amortized here into
// a single Get at the batch's start.
func testPointsVsMultiPolygonBatch(prep PreparedGeometry, xs, ys []float64, q containmentQuestion, out []bool) {
	mp, ok := prep.G.(MultiPolygon)
	if !ok {
		for i := range xs {
			out[i] = false
		}
		return
	}
	b := prep.Bounds
	// Held scratch for the whole batch. Linear path passes nil
	// (the core routes to the mpSubBounds scan). Tree path uses
	// the pool ONCE at batch entry.
	var bufPtr *[]int32
	if prep.mpTree != nil {
		bufPtr = mpTreeSearchBufAcquire()
		defer mpTreeSearchBufRelease(bufPtr)
	}
	for i := range xs {
		x, y := xs[i], ys[i]
		if x < b.MinX || x > b.MaxX || y < b.MinY || y > b.MaxY {
			out[i] = false
			continue
		}
		out[i] = mpQueryPointCore(prep, mp, x, y, q, bufPtr)
	}
}

// loadSubRings returns the ring views for sub-polygon i. Cached
// entries are returned via a single atomic load (fast path);
// missing entries are materialized on-demand via viewFromPoints
// and published with an atomic Store. Concurrent callers on the
// same slot may both allocate — the second Store overwrites the
// first, and the loser's slice is garbage-collected. Both writes
// produce semantically identical views (same slabs, same order)
// so the race is benign.
//
// Called only from the MultiPolygon prepared-fast-path — polygon
// callers have their rings materialized eagerly by Prepare.
func loadSubRings(p PreparedGeometry, i int, mp MultiPolygon) []PointsView {
	slot := &p.mpSubRings[i]
	if v := slot.Load(); v != nil {
		return v.rings
	}
	sub := mp.Polygons[i]
	fresh := &mpRingSlot{rings: make([]PointsView, len(sub.Rings))}
	for j, ring := range sub.Rings {
		fresh.rings[j] = viewFromPoints(ring, mp.HasZ, mp.CRSValue)
	}
	slot.Store(fresh)
	return fresh.rings
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

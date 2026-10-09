package geometry

import (
	"container/heap"
	"math"
	"sort"
	"sync"
)

// BoolOp names a binary boolean operation on polygons.
type BoolOp uint8

const (
	// OpIntersection returns the area contained by both operands.
	OpIntersection BoolOp = iota
	// OpUnion returns the area contained by either operand.
	OpUnion
	// OpDifference returns subject minus clipping (subject \ clipping).
	OpDifference
	// OpSymDifference returns the area in exactly one of the operands.
	OpSymDifference
)

func (op BoolOp) String() string {
	switch op {
	case OpIntersection:
		return "intersection"
	case OpUnion:
		return "union"
	case OpDifference:
		return "difference"
	case OpSymDifference:
		return "symmetric-difference"
	}
	return "unknown"
}

// ClipOptions tunes the boolean-op engine. The zero value picks sensible
// defaults; callers rarely need to override.
type ClipOptions struct {
	// Tolerance controls when two coordinates are treated as coincident.
	// Comparisons scale by max(|x|, |y|), so this is a relative tolerance.
	// Zero picks DefaultClipTolerance.
	Tolerance float64
}

// DefaultClipTolerance is the relative tolerance used by the boolean-op
// engine when the caller doesn't set one. 1e-10 gives ~13 significant
// figures — enough for coastal-scale UTM coordinates (~1e6 m).
const DefaultClipTolerance = 1e-10

func (o ClipOptions) tolerance() float64 {
	if o.Tolerance > 0 {
		return o.Tolerance
	}
	return DefaultClipTolerance
}

// clipSession bundles the mutable state for a single boolean-op run so we
// can carry tolerance and pool bookkeeping through helper calls without
// stuffing them into globals.
type clipSession struct {
	queue     eventQueue
	status    sweepStatus
	op        BoolOp
	tol       float64
	allocated []*sweepEvent // every event we produced, for return-to-pool
}

func (s *clipSession) track(e *sweepEvent) *sweepEvent {
	s.allocated = append(s.allocated, e)
	return e
}

func (s *clipSession) done() {
	for _, e := range s.allocated {
		releaseEvent(e)
	}
	s.allocated = s.allocated[:0]
}

// almostEqual reports whether |a-b| falls within the session's relative
// tolerance around max(|a|,|b|).
func (s *clipSession) almostEqual(a, b float64) bool {
	scale := math.Abs(a)
	if v := math.Abs(b); v > scale {
		scale = v
	}
	if scale < 1 {
		scale = 1
	}
	return math.Abs(a-b) <= s.tol*scale
}

// pointsCoincide reports whether two points sit within tolerance.
func (s *clipSession) pointsCoincide(p, q Point) bool {
	return s.almostEqual(p.X, q.X) && s.almostEqual(p.Y, q.Y)
}

// enqueueRing pushes the events for a single ring onto the queue. Ring must
// be closed (first == last); a zero-length or degenerate ring is skipped.
func (s *clipSession) enqueueRing(ring []Point, role polyRole) {
	if len(ring) < 4 {
		return
	}
	for i := range len(ring) - 1 {
		a := ring[i]
		b := ring[i+1]
		if s.pointsCoincide(a, b) {
			continue
		}
		e1, e2 := newEventPair(a, b, role)
		s.track(e1)
		s.track(e2)
		s.queue.push(e1)
		s.queue.push(e2)
	}
}

// enqueuePolygon pushes every ring of p onto the queue as segments of role.
func (s *clipSession) enqueuePolygon(p Polygon, role polyRole) {
	for _, r := range p.Rings {
		s.enqueueRing(closedRing(r), role)
	}
}

// enqueueMultiPolygon pushes every ring of every component of m.
func (s *clipSession) enqueueMultiPolygon(m MultiPolygon, role polyRole) {
	for _, p := range m.Polygons {
		s.enqueuePolygon(p, role)
	}
}

// sweep runs the main event loop and returns the ordered list of "in-result"
// left events, ready for contour reconnection. The returned events are also
// tracked in s.allocated for pool return via s.done().
func (s *clipSession) sweep() []*sweepEvent {
	var sorted []*sweepEvent
	for s.queue.Len() > 0 {
		ev := s.queue.pop()
		if ev.left {
			s.status.insert(ev)
			prev := s.status.prev(ev)
			next := s.status.next(ev)
			s.computeFields(ev, prev)
			// Fields at ev were computed against ev's LEFT endpoint. Splits
			// created by possibleIntersection shorten segments but leave
			// left-endpoint state intact, so we do NOT re-classify here —
			// re-classifying against a stale prev-in-status was the source
			// of a subtle correctness bug on collinear/touching inputs.
			if next != nil {
				s.possibleIntersection(ev, next)
			}
			if prev != nil {
				s.possibleIntersection(prev, ev)
			}
		} else {
			other := ev.otherEvent
			if other.pos >= 0 && other.pos < len(s.status.items) && s.status.items[other.pos] == other {
				prev := s.status.prev(other)
				next := s.status.next(other)
				s.status.remove(other)
				if prev != nil && next != nil {
					s.possibleIntersection(prev, next)
				}
			}
			sorted = append(sorted, other)
		}
	}
	return sorted
}

// computeFields sets ev.inOut, ev.otherInOut, and ev.inResult given its
// immediate predecessor in the status structure (may be nil). If prev
// is edgeCancelled (a coincident same-role duplicate that we've
// erased), we walk backward until we find a real predecessor — the
// sweep-line state at ev is the same as at the last non-cancelled
// edge below.
func (s *clipSession) computeFields(ev, prev *sweepEvent) {
	for prev != nil && prev.kind == edgeCancelled {
		prev = s.status.prev(prev)
	}
	if prev == nil {
		ev.inOut = false
		ev.otherInOut = true
	} else if ev.role == prev.role {
		ev.inOut = !prev.inOut
		ev.otherInOut = prev.otherInOut
	} else {
		ev.inOut = !prev.otherInOut
		if isVertical(prev) {
			ev.otherInOut = !prev.inOut
		} else {
			ev.otherInOut = prev.inOut
		}
	}
	ev.inResult = inResult(ev, s.op)
}

// isVertical reports whether ev's segment is vertical (same X on both ends).
func isVertical(ev *sweepEvent) bool {
	return ev.point.X == ev.otherEvent.point.X
}

// inResult applies the boolean-op filter to a left event with populated
// inOut/otherInOut/kind fields.
func inResult(ev *sweepEvent, op BoolOp) bool {
	switch ev.kind {
	case edgeNormal:
		switch op {
		case OpIntersection:
			return !ev.otherInOut
		case OpUnion:
			return ev.otherInOut
		case OpDifference:
			if ev.role == roleSubject {
				return ev.otherInOut
			}
			return !ev.otherInOut
		case OpSymDifference:
			return true
		}
	case edgeSameTransition:
		return op == OpIntersection || op == OpUnion
	case edgeDifferentTransition:
		return op == OpDifference || op == OpSymDifference
	case edgeNonContributing, edgeCancelled:
		return false
	}
	return false
}

// possibleIntersection resolves what happens when adjacent status events e1
// (below) and e2 (above) share space. Returns true if it modified the event
// queue or event graph (endpoints changed, new events inserted).
func (s *clipSession) possibleIntersection(e1, e2 *sweepEvent) bool {
	p1, p2 := e1.point, e1.otherEvent.point
	p3, p4 := e2.point, e2.otherEvent.point

	// Cheap bbox rejection.
	if math.Max(p1.X, p2.X) < math.Min(p3.X, p4.X)-s.tol ||
		math.Max(p1.Y, p2.Y) < math.Min(p3.Y, p4.Y)-s.tol ||
		math.Max(p3.X, p4.X) < math.Min(p1.X, p2.X)-s.tol ||
		math.Max(p3.Y, p4.Y) < math.Min(p1.Y, p2.Y)-s.tol {
		return false
	}

	ip1, ip2, kind := segmentSegmentIntersect(p1, p2, p3, p4, s.tol)
	switch kind {
	case intersectNone:
		return false
	case intersectPoint:
		// Split both segments at ip1 unless ip1 is already an endpoint.
		split := false
		if !s.pointsCoincide(ip1, p1) && !s.pointsCoincide(ip1, p2) {
			s.divideSegment(e1, ip1)
			split = true
		}
		if !s.pointsCoincide(ip1, p3) && !s.pointsCoincide(ip1, p4) {
			s.divideSegment(e2, ip1)
			split = true
		}
		return split
	case intersectOverlap:
		return s.handleOverlap(e1, e2, ip1, ip2)
	}
	return false
}

type intersectKind uint8

const (
	intersectNone intersectKind = iota
	intersectPoint
	intersectOverlap
)

// segmentSegmentIntersect returns:
//   - intersectPoint at p (single crossing)
//   - intersectOverlap between q1 and q2 (collinear overlap)
//   - intersectNone otherwise
//
// tol is the coordinate-scale tolerance used for parallelism and endpoint
// coincidence checks.
func segmentSegmentIntersect(a1, a2, b1, b2 Point, tol float64) (p, q Point, kind intersectKind) {
	dx1 := a2.X - a1.X
	dy1 := a2.Y - a1.Y
	dx2 := b2.X - b1.X
	dy2 := b2.Y - b1.Y
	denom := dx1*dy2 - dy1*dx2

	if math.Abs(denom) < tol*math.Max(1, math.Max(math.Abs(dx1)+math.Abs(dy1), math.Abs(dx2)+math.Abs(dy2))) {
		// Parallel. Test collinearity via signed area at b1 against line a1-a2.
		if math.Abs(signedArea(a1, a2, b1)) > tol*math.Max(1, math.Abs(dx1)+math.Abs(dy1)) {
			return Point{}, Point{}, intersectNone
		}
		// Collinear. Project all four points onto the a1→a2 axis and find
		// the overlap interval.
		var t1, t2, t3, t4 float64
		if math.Abs(dx1) >= math.Abs(dy1) {
			t1 = 0
			t2 = 1
			t3 = (b1.X - a1.X) / dx1
			t4 = (b2.X - a1.X) / dx1
		} else {
			t1 = 0
			t2 = 1
			t3 = (b1.Y - a1.Y) / dy1
			t4 = (b2.Y - a1.Y) / dy1
		}
		lo := math.Max(math.Min(t1, t2), math.Min(t3, t4))
		hi := math.Min(math.Max(t1, t2), math.Max(t3, t4))
		if lo > hi+tol {
			return Point{}, Point{}, intersectNone
		}
		p = Point{X: a1.X + lo*dx1, Y: a1.Y + lo*dy1}
		q = Point{X: a1.X + hi*dx1, Y: a1.Y + hi*dy1}
		if lo >= hi-tol {
			// Touching at a single collinear point — treat as intersectPoint.
			return p, Point{}, intersectPoint
		}
		return p, q, intersectOverlap
	}

	t := ((b1.X-a1.X)*dy2 - (b1.Y-a1.Y)*dx2) / denom
	u := ((b1.X-a1.X)*dy1 - (b1.Y-a1.Y)*dx1) / denom
	if t < -tol || t > 1+tol || u < -tol || u > 1+tol {
		return Point{}, Point{}, intersectNone
	}
	p = Point{X: a1.X + t*dx1, Y: a1.Y + t*dy1}
	return p, Point{}, intersectPoint
}

// divideSegment splits the segment owned by left-event e at point p by
// creating a new (right, left) pair inheriting e's polygon role and pushing
// them onto the queue. The graph is rewired so e's original otherEvent
// becomes the new left event's otherEvent. Returns the newly created LEFT
// event (i.e., the left endpoint of the second half of the split segment)
// so callers can chain further splits on the same original segment.
func (s *clipSession) divideSegment(e *sweepEvent, p Point) *sweepEvent {
	right := e.otherEvent
	// New right endpoint at p — closes off e's segment.
	newRight := acquireEvent()
	s.track(newRight)
	newRight.point = p
	newRight.role = e.role
	newRight.left = false
	newRight.otherEvent = e
	newRight.polyForward = e.polyForward
	// New left endpoint at p — starts the "second half".
	newLeft := acquireEvent()
	s.track(newLeft)
	newLeft.point = p
	newLeft.role = e.role
	newLeft.left = true
	newLeft.otherEvent = right
	newLeft.polyForward = e.polyForward
	// Rewire the pair.
	right.otherEvent = newLeft
	e.otherEvent = newRight
	// Ordering safety: if the new right endpoint happens to sort before its
	// left counterpart (numerical noise), swap them.
	if lessEvent(newRight, e) {
		e.left = false
		newRight.left = true
	}
	if lessEvent(right, newLeft) {
		newLeft.left = false
		right.left = true
	}
	s.queue.push(newRight)
	s.queue.push(newLeft)
	return newLeft
}

// handleOverlap resolves the collinear-overlap case: e1 and e2 lie on the
// same line and share the interval [q1, q2]. Chains splits so the shared
// portion becomes a single edge on each side, then tags that portion as
// sameTransition, differentTransition, or nonContributing.
func (s *clipSession) handleOverlap(e1, e2 *sweepEvent, q1, q2 Point) bool {
	// Normalize orientation so q1 sorts before q2 in event order.
	if pointLess(q2, q1) {
		q1, q2 = q2, q1
	}
	changed := false
	// Isolate the [q1, q2] portion of e1 as its own left event.
	shared1, c1 := s.isolateSubsegment(e1, q1, q2)
	changed = changed || c1
	shared2, c2 := s.isolateSubsegment(e2, q1, q2)
	changed = changed || c2
	if shared1 == nil || shared2 == nil {
		return changed
	}
	if shared1.role == shared2.role {
		// Two coincident edges from the same polygon (or two components of
		// the same MultiPolygon touching along an edge). Both edges cancel
		// out — the shared boundary is interior to the polygon's own union
		// and doesn't contribute a transition. Marking both edgeCancelled
		// (rather than edgeNonContributing) tells computeFields to walk
		// past them when computing state for events above.
		shared1.kind = edgeCancelled
		shared2.kind = edgeCancelled
	} else {
		// Different roles: compare the two polygons' ring-walking
		// directions along this shared edge. Matching directions →
		// both interiors on the same side → sameTransition. Opposite
		// directions → interiors on opposite sides → differentTransition.
		//
		// Using polyForward here (a stable per-event property set at
		// enqueue) rather than inOut equality (which is derived from
		// sweep-line state at insertion time and can drift when the
		// coincident pair is inserted against different predecessors).
		if shared1.polyForward == shared2.polyForward {
			shared1.kind = edgeSameTransition
		} else {
			shared1.kind = edgeDifferentTransition
		}
		shared2.kind = edgeNonContributing
	}
	return changed
}

// isolateSubsegment ensures the interval [lo, hi] of e's segment exists as
// its own left event, splitting e at lo and hi as needed. Returns the left
// event whose segment is exactly [lo, hi], and whether any split occurred.
// Assumes lo precedes hi in event order and both lie within e's segment
// (endpoints inclusive).
func (s *clipSession) isolateSubsegment(e *sweepEvent, lo, hi Point) (*sweepEvent, bool) {
	changed := false
	cur := e
	// Split at lo if lo is interior to cur's segment.
	if !s.pointsCoincide(lo, cur.point) && !s.pointsCoincide(lo, cur.otherEvent.point) {
		second := s.divideSegment(cur, lo)
		changed = true
		// The [lo, ...] portion is now `second`.
		cur = second
	} else if s.pointsCoincide(lo, cur.otherEvent.point) {
		// lo lies at cur's right endpoint — the [lo, hi] portion doesn't
		// live on cur; something upstream missed a split. Fall back to nil.
		return nil, changed
	}
	// Now cur's left endpoint equals lo (or is close). Split cur at hi if
	// hi is interior.
	if !s.pointsCoincide(hi, cur.point) && !s.pointsCoincide(hi, cur.otherEvent.point) {
		s.divideSegment(cur, hi)
		changed = true
	}
	return cur, changed
}

// polyRole tags a sweep event's source polygon in a binary boolean op.
type polyRole uint8

const (
	roleSubject polyRole = iota
	roleClipping
)

// edgeKind classifies a segment for the boolean op's result filter. Set
// during the subdivision phase when two subject/clipping segments are found
// to be collinear-overlapping.
type edgeKind uint8

const (
	// edgeNormal: an edge belonging to a single polygon; the default.
	edgeNormal edgeKind = iota
	// edgeNonContributing: the "duplicate partner" of a sameTransition /
	// differentTransition edge (i.e. the coincident edge from the OTHER
	// polygon that we skip in the output). Excluded from the result but
	// still marks that the other polygon has a boundary here — events
	// inserted above must not walk past it or they lose the other
	// polygon's state.
	edgeNonContributing
	// edgeCancelled: same-role coincident duplicate (two components of
	// one MultiPolygon touching along an edge, or self-overlapping input).
	// The two edges cancel each other from the boundary entirely; events
	// inserted above should treat them as if they never existed.
	edgeCancelled
	// edgeSameTransition: this edge coincides with an edge of the other
	// polygon and both polygons transition in the same direction across it
	// (both "outside → inside" or both "inside → outside"). Kept for
	// intersection and union.
	edgeSameTransition
	// edgeDifferentTransition: coincident with an edge of the other polygon
	// but the two polygons transition in opposite directions across it.
	// Kept for difference and symmetric difference.
	edgeDifferentTransition
)

// sweepEvent represents one endpoint of a polygon edge that the sweepline
// will visit. Events come in pairs (left, right) linked via otherEvent; each
// pair represents one directed edge.
//
// Invariants once emitted onto the sweep line:
//   - left && point.X <= otherEvent.point.X (or equal-X with point.Y ≤ other.Y)
//   - otherEvent.otherEvent == self
//   - inOut and otherInOut are set at insertion time and stay stable
//     unless the event is re-inserted after a mid-sweep segment split
type sweepEvent struct {
	point      Point
	otherEvent *sweepEvent
	role       polyRole
	left       bool
	kind       edgeKind
	inOut      bool
	otherInOut bool
	inResult   bool
	// polyForward encodes the ring-walking direction of this event's
	// segment. Set on both left and right endpoints from the enqueuing
	// ring: true if the LEFT event of this segment corresponds to the
	// ring's starting vertex for this edge (i.e., the ring walks A→B
	// where A < B in sweep-event order), false if the ring walks B→A.
	//
	// handleOverlap uses this to classify different-role coincident
	// edges: matching polyForward = both polygons walk the shared edge
	// in the same direction, both interiors on the same side →
	// sameTransition. Opposite polyForward = polygons walk it in
	// opposite directions, interiors on opposite sides →
	// differentTransition. This is more robust than comparing
	// inOut/otherInOut (which drift when the coincident partners are
	// inserted into the sweep-line status against different immediate
	// predecessors — see the identical-octagon regression fix).
	polyForward bool
	// outputIdx: position in the sorted result-event slice during
	// contour reconnection. For right events it's swapped with its
	// left partner's slot so the tracer jumps to the other endpoint
	// in O(1).
	outputIdx int
	// pos tracks the event's slot inside the sweep-line status structure so
	// we can find prev/next without a linear scan. Managed by sweepStatus.
	pos int
}

// segmentBelow returns +1 if e's segment lies strictly above other's segment
// at the point where they enter the sweep line, -1 if strictly below, and 0
// if collinear. e and other must both be left events.
func segmentBelow(e, other *sweepEvent) int {
	// Position of other's endpoints relative to e's segment.
	c1 := signedArea(e.point, e.otherEvent.point, other.point)
	c2 := signedArea(e.point, e.otherEvent.point, other.otherEvent.point)
	if c1 != 0 || c2 != 0 {
		// Not collinear. If the two segments share their left point, we
		// compare by the *other* endpoint.
		if e.point.X == other.point.X && e.point.Y == other.point.Y {
			if c2 > 0 {
				return -1
			}
			return 1
		}
		// Otherwise, place `other` relative to `e`.
		if lessEvent(e, other) {
			if c1 > 0 {
				return -1
			}
			return 1
		}
		// e comes after other in x-order: flip sign so the answer stays
		// self-consistent regardless of insertion order.
		c := signedArea(other.point, other.otherEvent.point, e.point)
		if c > 0 {
			return 1
		}
		return -1
	}
	// Collinear: fall back to polygon role, then event-queue order.
	if e.role != other.role {
		if e.role < other.role {
			return -1
		}
		return 1
	}
	if lessEvent(e, other) {
		return -1
	}
	if lessEvent(other, e) {
		return 1
	}
	return 0
}

// lessEvent returns true if a should be processed before b in the sweep.
// Ordering (per Martinez-Rueda):
//  1. smaller X first,
//  2. smaller Y first,
//  3. right endpoints before left endpoints (so a right event pops before a
//     coincident left event, ending an old segment before beginning a new one),
//  4. among two coincident endpoints of the same handedness, the segment whose
//     other endpoint is "below" comes first.
func lessEvent(a, b *sweepEvent) bool {
	if a.point.X != b.point.X {
		return a.point.X < b.point.X
	}
	if a.point.Y != b.point.Y {
		return a.point.Y < b.point.Y
	}
	if a.left != b.left {
		// right (false) before left (true)
		return !a.left && b.left
	}
	// Same point, same handedness: the segment that is "below" comes first.
	c := signedArea(a.point, a.otherEvent.point, b.otherEvent.point)
	if c != 0 {
		return c > 0
	}
	// Fully coincident: subject before clipping for determinism.
	return a.role < b.role
}

// signedArea returns 2× the signed area of triangle (a, b, c). Sign
// indicates orientation: >0 CCW, <0 CW, 0 collinear.
func signedArea(a, b, c Point) float64 {
	return (b.X-a.X)*(c.Y-a.Y) - (c.X-a.X)*(b.Y-a.Y)
}

// eventQueue is a min-heap of sweep events ordered by lessEvent.
type eventQueue struct {
	items []*sweepEvent
}

func (q *eventQueue) Len() int           { return len(q.items) }
func (q *eventQueue) Less(i, j int) bool { return lessEvent(q.items[i], q.items[j]) }
func (q *eventQueue) Swap(i, j int)      { q.items[i], q.items[j] = q.items[j], q.items[i] }
func (q *eventQueue) Push(x any)         { q.items = append(q.items, x.(*sweepEvent)) }
func (q *eventQueue) Pop() any {
	n := len(q.items)
	x := q.items[n-1]
	q.items = q.items[:n-1]
	return x
}

func (q *eventQueue) push(e *sweepEvent) { heap.Push(q, e) }
func (q *eventQueue) pop() *sweepEvent   { return heap.Pop(q).(*sweepEvent) }
func (q *eventQueue) peek() *sweepEvent {
	if len(q.items) == 0 {
		return nil
	}
	return q.items[0]
}

// sweepStatus holds the active-segment status structure. Segments are kept
// ordered by segmentBelow (Y at the current sweep X, tie-breaking by the
// right endpoint). Implemented as a sorted slice — O(n) per op, but for the
// polygon sizes gobi targets (up to a few thousand segments) this beats a
// tree due to cache locality. Swap for a treap if profiles say otherwise.
type sweepStatus struct {
	items []*sweepEvent
}

// insert adds e into the correct position and records e.pos. Returns the
// index at which e was inserted.
func (s *sweepStatus) insert(e *sweepEvent) int {
	lo, hi := 0, len(s.items)
	for lo < hi {
		mid := (lo + hi) / 2
		if segmentBelow(s.items[mid], e) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	s.items = append(s.items, nil)
	copy(s.items[lo+1:], s.items[lo:])
	s.items[lo] = e
	e.pos = lo
	// Fix up pos of everything to the right of the insertion.
	for i := lo + 1; i < len(s.items); i++ {
		s.items[i].pos = i
	}
	return lo
}

// remove deletes e from the status. e.pos is used as a hint; if stale, a
// linear search recovers.
func (s *sweepStatus) remove(e *sweepEvent) {
	idx := e.pos
	if idx < 0 || idx >= len(s.items) || s.items[idx] != e {
		idx = -1
		for i, x := range s.items {
			if x == e {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
	}
	copy(s.items[idx:], s.items[idx+1:])
	s.items = s.items[:len(s.items)-1]
	for i := idx; i < len(s.items); i++ {
		s.items[i].pos = i
	}
	e.pos = -1
}

// prev returns the event immediately below e in the status, or nil.
func (s *sweepStatus) prev(e *sweepEvent) *sweepEvent {
	if e.pos <= 0 || e.pos >= len(s.items) || s.items[e.pos] != e {
		return nil
	}
	return s.items[e.pos-1]
}

// next returns the event immediately above e, or nil.
func (s *sweepStatus) next(e *sweepEvent) *sweepEvent {
	if e.pos < 0 || e.pos+1 >= len(s.items) || s.items[e.pos] != e {
		return nil
	}
	return s.items[e.pos+1]
}

// eventPool recycles sweepEvent allocations across boolean ops. Any call to
// a boolean op returns its events to the pool on exit, which drops
// per-cell-loop allocations in the hot path to near zero.
var eventPool = sync.Pool{
	New: func() any { return &sweepEvent{pos: -1} },
}

func acquireEvent() *sweepEvent {
	e := eventPool.Get().(*sweepEvent)
	*e = sweepEvent{pos: -1}
	return e
}

func releaseEvent(e *sweepEvent) {
	if e == nil {
		return
	}
	e.otherEvent = nil
	eventPool.Put(e)
}

// newEventPair returns a linked (left, right) pair of events for segment
// (a, b) on the given polygon role, where a and b are given in the
// ring's walking order (a is the "start" vertex of this edge in the
// ring, b is the "end"). The pair is oriented so that left.point
// precedes right.point in event order. Both events' polyForward field
// records whether the LEFT event corresponds to the ring's start
// vertex (true → ring walks A→B in sweep order, false → ring walks
// B→A). See sweepEvent.polyForward for why this matters.
func newEventPair(a, b Point, role polyRole) (*sweepEvent, *sweepEvent) {
	e1 := acquireEvent()
	e2 := acquireEvent()
	e1.point = a
	e2.point = b
	e1.role = role
	e2.role = role
	e1.otherEvent = e2
	e2.otherEvent = e1
	// aIsLeft: does a (the ring-order start) become the LEFT sweep
	// event? True when a < b in sweep event order.
	aIsLeft := pointLess(a, b)
	if aIsLeft {
		e1.left = true
	} else {
		e2.left = true
	}
	e1.polyForward = aIsLeft
	e2.polyForward = aIsLeft
	return e1, e2
}

// pointLess is the event-order comparator on raw points (no left/right, no
// otherEvent).
func pointLess(a, b Point) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	return a.Y < b.Y
}

// connectContours turns the post-sweep list of "in-result" left events into
// output rings. Rings are traced first, then hole/exterior classification
// is done via point-in-polygon against larger rings (see
// classifyHolesByContainment); the older prevInRes-chain classification
// broke on Dissolve outputs with many overlapping intermediate regions.
// Exterior rings are emitted CCW and holes CW, matching the GeoJSON / WKB
// convention.
func (s *clipSession) connectContours(sorted []*sweepEvent) []ringResult {
	resultEvents := gatherResultEvents(sorted, s.op)
	if len(resultEvents) == 0 {
		return nil
	}
	// Ensure a stable, event-order traversal for reproducible contours.
	sort.SliceStable(resultEvents, func(i, j int) bool {
		return lessEvent(resultEvents[i], resultEvents[j])
	})
	for i, e := range resultEvents {
		e.outputIdx = i
	}
	// For right events, cross-reference so outputIdx of a right event points
	// at the position of its left partner and vice versa. This lets the
	// contour tracer jump from one endpoint of an edge to the other in O(1).
	for _, e := range resultEvents {
		if !e.left {
			tmp := e.outputIdx
			e.outputIdx = e.otherEvent.outputIdx
			e.otherEvent.outputIdx = tmp
		}
	}
	processed := make([]bool, len(resultEvents))
	var contours []ringResult

	for i := range resultEvents {
		if processed[i] {
			continue
		}
		result := ringResult{parent: -1, depth: 0, isHole: false}

		pos := i
		initial := resultEvents[pos].point
		result.points = append(result.points, initial)
		for {
			processed[pos] = true
			// Jump across the edge to its other endpoint.
			pos = resultEvents[pos].outputIdx
			if pos < 0 || pos >= len(resultEvents) {
				break
			}
			processed[pos] = true
			pt := resultEvents[pos].point
			// Loop closed?
			if pointsEqual(pt, initial) {
				result.points = append(result.points, initial)
				break
			}
			result.points = append(result.points, pt)
			// Find the next un-processed event at this vertex.
			next := nextResultPos(pos, resultEvents, processed)
			if next < 0 {
				// Contour did not close cleanly — force close by appending
				// the initial point. This happens on degenerate inputs and
				// gives a well-formed ring even when the algorithm bailed.
				if !pointsEqual(result.points[len(result.points)-1], initial) {
					result.points = append(result.points, initial)
				}
				break
			}
			pos = next
		}
		contours = append(contours, result)
	}

	// Reclassify holes/exteriors via geometric containment. Only after this
	// pass do we know each ring's true depth in the nesting tree.
	classifyHolesByContainment(contours)

	// Orient exteriors CCW and holes CW, per the final classification.
	for i := range contours {
		if contours[i].isHole == isCCW(contours[i].points) {
			reversePoints(contours[i].points)
		}
	}
	return contours
}

// classifyHolesByContainment sets parent/depth/isHole on every ring based
// on actual geometric containment. Rings are sorted by absolute planar
// area descending; each ring's parent is the smallest strictly-larger
// ring whose interior contains it. depth is (parent.depth + 1) if a
// parent exists, else 0. isHole is (depth & 1) == 1.
//
// Complexity: O(R² × V̄) where R is ring count and V̄ is mean vertex
// count per ring. For typical Dissolve outputs (R ≤ few hundred, V̄ ≤
// tens), this is sub-millisecond.
func classifyHolesByContainment(rings []ringResult) {
	n := len(rings)
	if n <= 1 {
		if n == 1 {
			rings[0].parent = -1
			rings[0].depth = 0
			rings[0].isHole = false
		}
		return
	}
	areas := make([]float64, n)
	for i := range rings {
		areas[i] = planarRingArea(rings[i].points)
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return areas[order[a]] > areas[order[b]]
	})
	for i := range rings {
		rings[i].parent = -1
		rings[i].depth = 0
		rings[i].isHole = false
	}
	for pos, idx := range order {
		r := rings[idx]
		if len(r.points) < 3 {
			continue
		}
		// Test the ring's first vertex against each strictly-larger
		// candidate ring. Sorted descending, so earlier positions in
		// `order` are all strictly larger — the first match walking
		// backward from idx-1 is the smallest enclosing ring.
		testPt := r.points[0]
		for j := pos - 1; j >= 0; j-- {
			candidateIdx := order[j]
			if pointInRing(testPt, rings[candidateIdx].points) {
				rings[idx].parent = candidateIdx
				rings[idx].depth = rings[candidateIdx].depth + 1
				rings[idx].isHole = (rings[idx].depth & 1) == 1
				break
			}
		}
	}
}

// ringResult holds a single traced contour plus its hole/exterior linkage.
type ringResult struct {
	points []Point
	// parent is the index in contours of the exterior ring this hole
	// belongs to, or -1 for an exterior ring.
	parent int
	// depth is the number of enclosing exterior rings; 0 for an outer, 1
	// for a hole, 2 for an outer inside a hole, etc.
	depth int
	// isHole is true when depth is odd.
	isHole bool
}

// gatherResultEvents flattens sorted (post-sweep, left events only) into a
// list containing every in-result event (both endpoints). Membership is
// recomputed here against final kind values — handleOverlap can retag an
// edge's kind AFTER computeFields ran, so the ev.inResult set at insertion
// time may be stale.
func gatherResultEvents(sorted []*sweepEvent, op BoolOp) []*sweepEvent {
	out := make([]*sweepEvent, 0, len(sorted)*2)
	for _, e := range sorted {
		if !e.left {
			// sorted from sweep() contains left events only.
			continue
		}
		e.inResult = inResult(e, op)
		if !e.inResult {
			continue
		}
		out = append(out, e)
		out = append(out, e.otherEvent)
	}
	return out
}

// nextResultPos returns the sorted-list index of the next un-processed
// event that shares the point at position pos, or -1 if no such neighbor
// exists. Scans forward first, then backward.
func nextResultPos(pos int, events []*sweepEvent, processed []bool) int {
	p := events[pos].point
	n := len(events)
	for i := pos + 1; i < n; i++ {
		if !pointsEqual(events[i].point, p) {
			break
		}
		if !processed[i] {
			return i
		}
	}
	for i := pos - 1; i >= 0; i-- {
		if !pointsEqual(events[i].point, p) {
			break
		}
		if !processed[i] {
			return i
		}
	}
	return -1
}

func pointsEqual(a, b Point) bool { return a.X == b.X && a.Y == b.Y }

// isCCW reports whether pts winds counter-clockwise via the shoelace sign.
// A zero-area ring returns false.
func isCCW(pts []Point) bool {
	if len(pts) < 3 {
		return false
	}
	var s float64
	for i := range len(pts) - 1 {
		s += (pts[i+1].X - pts[i].X) * (pts[i+1].Y + pts[i].Y)
	}
	// closing edge
	s += (pts[0].X - pts[len(pts)-1].X) * (pts[0].Y + pts[len(pts)-1].Y)
	// Shoelace with (x2-x1)*(y2+y1) is positive for CW; we want CCW.
	return s < 0
}

func reversePoints(pts []Point) {
	for i, j := 0, len(pts)-1; i < j; i, j = i+1, j-1 {
		pts[i], pts[j] = pts[j], pts[i]
	}
}

// assemble packs the ring results into geometry types with the given CRS.
// Returns Polygon if the result has exactly one exterior with any number of
// holes, MultiPolygon if it has multiple exteriors, or an empty Polygon
// (nil Rings) for the empty set.
func assemble(rings []ringResult, crs CRS) Geometry {
	// Separate exteriors and holes.
	var exteriors []int
	holesByParent := map[int][]int{}
	for i, r := range rings {
		if r.isHole {
			holesByParent[r.parent] = append(holesByParent[r.parent], i)
		} else {
			exteriors = append(exteriors, i)
		}
	}
	if len(exteriors) == 0 {
		return Polygon{CRSValue: crs}
	}
	if len(exteriors) == 1 {
		exID := exteriors[0]
		poly := Polygon{Rings: [][]Point{rings[exID].points}, CRSValue: crs}
		for _, hID := range holesByParent[exID] {
			poly.Rings = append(poly.Rings, rings[hID].points)
		}
		return poly
	}
	// Multi-exterior: build a MultiPolygon.
	polys := make([]Polygon, 0, len(exteriors))
	for _, exID := range exteriors {
		poly := Polygon{Rings: [][]Point{rings[exID].points}, CRSValue: crs}
		for _, hID := range holesByParent[exID] {
			poly.Rings = append(poly.Rings, rings[hID].points)
		}
		polys = append(polys, poly)
	}
	return MultiPolygon{Polygons: polys, CRSValue: crs}
}

package geometry

import "math"

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

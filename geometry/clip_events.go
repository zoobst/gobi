package geometry

import (
	"container/heap"
	"sync"
)

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

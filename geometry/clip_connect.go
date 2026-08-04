package geometry

import "sort"

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

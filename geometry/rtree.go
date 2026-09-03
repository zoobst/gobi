package geometry

import (
	"math"
	"sort"
)

// RTreeNodeSize is the maximum number of children in an R-tree node. Values
// around 16 balance memory density and traversal cost.
const RTreeNodeSize = 16

// RTree is a static, bulk-loaded Sort-Tile-Recursive R-tree over 2D
// bounding boxes. Once built with NewRTree the tree is immutable and safe
// for concurrent readers.
//
// Internal layout: struct-of-arrays. Node bboxes live in four parallel
// []float64s (nodeMinX/Y/MaxX/MaxY); item bboxes get the same treatment.
// The query hot paths (Search, NearestOne) read tightly-packed contiguous
// slices instead of walking through a []struct where each 48-byte
// node dragged in 16 unused padding bytes per cache line.
type RTree struct {
	// Item bboxes, indexed by the caller-facing item ID. Parallel
	// arrays — itemMinX[id], itemMinY[id], itemMaxX[id], itemMaxY[id]
	// together describe item id's bbox.
	itemMinX, itemMinY, itemMaxX, itemMaxY []float64

	// itemIDs is a permutation: itemIDs[i] is the caller ID at leaf slot i.
	itemIDs []int32
	// childRefs stores child node indexes for internal nodes.
	childRefs []int32

	// Node bboxes, indexed by internal node index. Parallel arrays.
	// Leaves and internal nodes share the same node-index space; nodes
	// are laid out with leaves first, then internal levels, root last.
	nodeMinX, nodeMinY, nodeMaxX, nodeMaxY []float64
	// nodeFirst[i] + nodeCount[i] index into itemIDs if nodeIsLeaf[i],
	// otherwise into childRefs.
	nodeFirst  []int32
	nodeCount  []int32
	nodeIsLeaf []bool

	root int32
}

// NewRTree builds an R-tree over the given bounding boxes. Item IDs
// returned by queries are indexes into bounds.
func NewRTree(bounds []Bounds) *RTree {
	n := len(bounds)
	t := &RTree{
		itemMinX: make([]float64, n),
		itemMinY: make([]float64, n),
		itemMaxX: make([]float64, n),
		itemMaxY: make([]float64, n),
	}
	for i, b := range bounds {
		t.itemMinX[i] = b.MinX
		t.itemMinY[i] = b.MinY
		t.itemMaxX[i] = b.MaxX
		t.itemMaxY[i] = b.MaxY
	}
	if n == 0 {
		t.root = -1
		return t
	}
	ids := make([]int32, n)
	for i := range ids {
		ids[i] = int32(i)
	}
	leaves := t.buildLeafLevel(ids)
	for len(leaves) > 1 {
		leaves = t.buildInternalLevel(leaves)
	}
	t.root = leaves[0]
	return t
}

// Len returns the number of items indexed.
func (t *RTree) Len() int { return len(t.itemMinX) }

// Bounds returns the R-tree's overall bounding box.
func (t *RTree) Bounds() Bounds {
	if t.root < 0 {
		return EmptyBounds()
	}
	return Bounds{
		MinX: t.nodeMinX[t.root], MinY: t.nodeMinY[t.root],
		MaxX: t.nodeMaxX[t.root], MaxY: t.nodeMaxY[t.root],
	}
}

// Search returns the IDs of every item whose bounding box intersects q.
// Allocates a fresh result slice per call.
func (t *RTree) Search(q Bounds) []int32 {
	return t.SearchInto(nil, q)
}

// SearchInto appends every item ID whose bounding box intersects q to buf
// (after truncating buf to zero length) and returns the resulting slice.
// This lets callers reuse a scratch buffer across queries to avoid a fresh
// allocation each time.
func (t *RTree) SearchInto(buf []int32, q Bounds) []int32 {
	out := buf[:0]
	if t.root < 0 || !t.nodeIntersectsQ(t.root, q) {
		return out
	}
	stack := []int32{t.root}
	for len(stack) > 0 {
		idx := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !t.nodeIntersectsQ(idx, q) {
			continue
		}
		first, count := t.nodeFirst[idx], t.nodeCount[idx]
		if t.nodeIsLeaf[idx] {
			for i := range count {
				id := t.itemIDs[first+i]
				if t.itemIntersectsQ(id, q) {
					out = append(out, id)
				}
			}
			continue
		}
		for i := range count {
			stack = append(stack, t.childRefs[first+i])
		}
	}
	return out
}

// Nearest returns the k item IDs whose bounding boxes are closest (by
// squared Euclidean point-to-bbox distance) to (x, y), in ascending
// distance order. Fewer than k IDs are returned if the tree is smaller.
//
// For the k=1 case, prefer NearestOne — same semantics but skips the
// priority queue for a zero-allocation depth-first descent.
func (t *RTree) Nearest(x, y float64, k int) []int32 {
	if t.root < 0 || k <= 0 {
		return nil
	}
	var pq rtreePQ
	pq.push(rtreeQueue{node: t.root, dist: t.nodeBboxDist(t.root, x, y)})

	out := make([]int32, 0, k)
	for len(pq) > 0 && len(out) < k {
		top := pq.pop()
		if top.isItem {
			out = append(out, top.item)
			continue
		}
		first, count := t.nodeFirst[top.node], t.nodeCount[top.node]
		if t.nodeIsLeaf[top.node] {
			for i := range count {
				id := t.itemIDs[first+i]
				pq.push(rtreeQueue{isItem: true, item: id, dist: t.itemBboxDist(id, x, y)})
			}
		} else {
			for i := range count {
				child := t.childRefs[first+i]
				pq.push(rtreeQueue{node: child, dist: t.nodeBboxDist(child, x, y)})
			}
		}
	}
	return out
}

// NearestOne returns the ID of the item whose bounding box is
// closest to (x, y) by squared Euclidean point-to-bbox distance.
// ok=false when the tree is empty. Semantically equivalent to
// Nearest(x, y, 1)[0] but with zero allocations — depth-first
// descent with a running best-so-far distance + bbox pruning
// replaces the general k>1 path's priority queue.
//
// Callers doing a single-nearest lookup at high frequency (e.g.
// snap-to-graph, per-point classification) should prefer this over
// Nearest(x, y, 1). At 1M+ calls per request the alloc + boxing
// savings dominate the CPU profile.
func (t *RTree) NearestOne(x, y float64) (id int32, ok bool) {
	if t.root < 0 {
		return 0, false
	}
	best := rtreeNearestState{dist: math.Inf(1), id: -1}
	t.nearestOneDescend(t.root, x, y, &best)
	return best.id, best.id >= 0
}

type rtreeNearestState struct {
	dist float64
	id   int32
}

// nearestOneDescend walks the subtree rooted at nodeIdx, updating
// best in place. Prunes any subtree whose bounding-box distance
// already exceeds the running best. Children are visited in
// ascending bbox-distance order so the tightest bound is found
// early — subsequent siblings that can't improve get pruned by the
// early-exit break at the loop tail.
//
// Recursion depth is O(log_M(N)) — for RTreeNodeSize=16 and even a
// billion items that's about 8. Well within Go's default goroutine
// stack; no risk of blow-up.
func (t *RTree) nearestOneDescend(nodeIdx int32, x, y float64, best *rtreeNearestState) {
	if t.nodeBboxDist(nodeIdx, x, y) >= best.dist {
		return
	}
	first, count := t.nodeFirst[nodeIdx], t.nodeCount[nodeIdx]
	if t.nodeIsLeaf[nodeIdx] {
		for i := range count {
			id := t.itemIDs[first+i]
			d := t.itemBboxDist(id, x, y)
			if d < best.dist {
				best.dist = d
				best.id = id
			}
		}
		return
	}
	// Order children by ascending bbox distance for aggressive
	// pruning. Fixed-size stack buffer sized to the max fan-out —
	// zero heap allocation. Insertion sort on ≤16 elements beats
	// the general sort's setup cost.
	var buf [RTreeNodeSize]struct {
		child int32
		dist  float64
	}
	for i := range count {
		c := t.childRefs[first+i]
		buf[i].child = c
		buf[i].dist = t.nodeBboxDist(c, x, y)
	}
	entries := buf[:count]
	for i := 1; i < len(entries); i++ {
		cur := entries[i]
		j := i
		for j > 0 && entries[j-1].dist > cur.dist {
			entries[j] = entries[j-1]
			j--
		}
		entries[j] = cur
	}
	for _, e := range entries {
		if e.dist >= best.dist {
			break
		}
		t.nearestOneDescend(e.child, x, y, best)
	}
}

// nodeIntersectsQ tests whether node idx's bbox intersects q. Reads
// the four bbox arrays directly rather than materializing a Bounds
// struct — keeps the hot query loop's memory accesses on contiguous
// slices that the compiler can lower to sequential loads.
func (t *RTree) nodeIntersectsQ(idx int32, q Bounds) bool {
	return !(q.MinX > t.nodeMaxX[idx] || t.nodeMinX[idx] > q.MaxX ||
		q.MinY > t.nodeMaxY[idx] || t.nodeMinY[idx] > q.MaxY)
}

// itemIntersectsQ is the leaf-level companion to nodeIntersectsQ,
// reading from the item bbox arrays instead.
func (t *RTree) itemIntersectsQ(id int32, q Bounds) bool {
	return !(q.MinX > t.itemMaxX[id] || t.itemMinX[id] > q.MaxX ||
		q.MinY > t.itemMaxY[id] || t.itemMinY[id] > q.MaxY)
}

// nodeBboxDist returns the squared Euclidean distance from (x, y) to
// node idx's bbox. Zero if the point is inside the bbox.
func (t *RTree) nodeBboxDist(idx int32, x, y float64) float64 {
	var dx, dy float64
	if x < t.nodeMinX[idx] {
		dx = t.nodeMinX[idx] - x
	} else if x > t.nodeMaxX[idx] {
		dx = x - t.nodeMaxX[idx]
	}
	if y < t.nodeMinY[idx] {
		dy = t.nodeMinY[idx] - y
	} else if y > t.nodeMaxY[idx] {
		dy = y - t.nodeMaxY[idx]
	}
	return dx*dx + dy*dy
}

// itemBboxDist is the leaf-level counterpart to nodeBboxDist.
func (t *RTree) itemBboxDist(id int32, x, y float64) float64 {
	var dx, dy float64
	if x < t.itemMinX[id] {
		dx = t.itemMinX[id] - x
	} else if x > t.itemMaxX[id] {
		dx = x - t.itemMaxX[id]
	}
	if y < t.itemMinY[id] {
		dy = t.itemMinY[id] - y
	} else if y > t.itemMaxY[id] {
		dy = y - t.itemMaxY[id]
	}
	return dx*dx + dy*dy
}

// --- build ---

func (t *RTree) buildLeafLevel(ids []int32) []int32 {
	sort.Slice(ids, func(i, j int) bool {
		return t.itemXCenter(ids[i]) < t.itemXCenter(ids[j])
	})
	M := RTreeNodeSize
	P := int(math.Ceil(float64(len(ids)) / float64(M))) // total leaves
	S := max(int(math.Ceil(math.Sqrt(float64(P)))), 1)
	stripeSize := max(int(math.Ceil(float64(len(ids))/float64(S))), M)

	var leaves []int32
	for i := 0; i < len(ids); i += stripeSize {
		end := min(i+stripeSize, len(ids))
		stripe := ids[i:end]
		sort.Slice(stripe, func(i, j int) bool {
			return t.itemYCenter(stripe[i]) < t.itemYCenter(stripe[j])
		})
		for j := 0; j < len(stripe); j += M {
			e := min(j+M, len(stripe))
			group := stripe[j:e]
			b := EmptyBounds()
			for _, id := range group {
				b = b.Union(Bounds{
					MinX: t.itemMinX[id], MinY: t.itemMinY[id],
					MaxX: t.itemMaxX[id], MaxY: t.itemMaxY[id],
				})
			}
			offset := int32(len(t.itemIDs))
			t.itemIDs = append(t.itemIDs, group...)
			leaves = append(leaves, t.appendNode(b, offset, int32(len(group)), true))
		}
	}
	return leaves
}

func (t *RTree) buildInternalLevel(children []int32) []int32 {
	sort.Slice(children, func(i, j int) bool {
		return t.nodeXCenter(children[i]) < t.nodeXCenter(children[j])
	})
	M := RTreeNodeSize
	P := int(math.Ceil(float64(len(children)) / float64(M)))
	S := max(int(math.Ceil(math.Sqrt(float64(P)))), 1)
	stripeSize := max(int(math.Ceil(float64(len(children))/float64(S))), M)

	var next []int32
	for i := 0; i < len(children); i += stripeSize {
		end := min(i+stripeSize, len(children))
		stripe := children[i:end]
		sort.Slice(stripe, func(i, j int) bool {
			return t.nodeYCenter(stripe[i]) < t.nodeYCenter(stripe[j])
		})
		for j := 0; j < len(stripe); j += M {
			e := min(j+M, len(stripe))
			group := stripe[j:e]
			b := EmptyBounds()
			for _, id := range group {
				b = b.Union(Bounds{
					MinX: t.nodeMinX[id], MinY: t.nodeMinY[id],
					MaxX: t.nodeMaxX[id], MaxY: t.nodeMaxY[id],
				})
			}
			offset := int32(len(t.childRefs))
			t.childRefs = append(t.childRefs, group...)
			next = append(next, t.appendNode(b, offset, int32(len(group)), false))
		}
	}
	return next
}

// appendNode writes a new node into the parallel arrays and returns
// its index.
func (t *RTree) appendNode(b Bounds, first, count int32, isLeaf bool) int32 {
	idx := int32(len(t.nodeMinX))
	t.nodeMinX = append(t.nodeMinX, b.MinX)
	t.nodeMinY = append(t.nodeMinY, b.MinY)
	t.nodeMaxX = append(t.nodeMaxX, b.MaxX)
	t.nodeMaxY = append(t.nodeMaxY, b.MaxY)
	t.nodeFirst = append(t.nodeFirst, first)
	t.nodeCount = append(t.nodeCount, count)
	t.nodeIsLeaf = append(t.nodeIsLeaf, isLeaf)
	return idx
}

func (t *RTree) itemXCenter(id int32) float64 { return (t.itemMinX[id] + t.itemMaxX[id]) / 2 }
func (t *RTree) itemYCenter(id int32) float64 { return (t.itemMinY[id] + t.itemMaxY[id]) / 2 }
func (t *RTree) nodeXCenter(id int32) float64 { return (t.nodeMinX[id] + t.nodeMaxX[id]) / 2 }
func (t *RTree) nodeYCenter(id int32) float64 { return (t.nodeMinY[id] + t.nodeMaxY[id]) / 2 }

// --- priority queue for Nearest ---

type rtreeQueue struct {
	node   int32
	item   int32
	isItem bool
	dist   float64
}

// rtreePQ is a min-heap of rtreeQueue entries ordered by dist,
// hand-rolled instead of using container/heap so Push/Pop don't
// box each entry through the `any` interface. Every heap.Push in
// the old shape was a heap allocation for the boxed rtreeQueue
// (24-byte struct doesn't fit in an interface word); the direct-
// typed shape here does the swap-in-place min-heap dance without
// any allocation beyond the slice's own growth.
type rtreePQ []rtreeQueue

func (h *rtreePQ) push(v rtreeQueue) {
	*h = append(*h, v)
	// Sift up from the last position.
	i := len(*h) - 1
	slice := *h
	for i > 0 {
		parent := (i - 1) / 2
		if slice[parent].dist <= slice[i].dist {
			break
		}
		slice[parent], slice[i] = slice[i], slice[parent]
		i = parent
	}
}

func (h *rtreePQ) pop() rtreeQueue {
	slice := *h
	n := len(slice)
	top := slice[0]
	slice[0] = slice[n-1]
	*h = slice[:n-1]
	// Sift down from the root.
	if len(*h) > 1 {
		slice = *h
		i := 0
		for {
			left := 2*i + 1
			right := 2*i + 2
			smallest := i
			if left < len(slice) && slice[left].dist < slice[smallest].dist {
				smallest = left
			}
			if right < len(slice) && slice[right].dist < slice[smallest].dist {
				smallest = right
			}
			if smallest == i {
				break
			}
			slice[i], slice[smallest] = slice[smallest], slice[i]
			i = smallest
		}
	}
	return top
}

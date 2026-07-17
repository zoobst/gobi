package geometry

import (
	"container/heap"
	"math"
	"sort"
)

// RTreeNodeSize is the maximum number of children in an R-tree node. Values
// around 16 balance memory density and traversal cost.
const RTreeNodeSize = 16

// RTree is a static, bulk-loaded Sort-Tile-Recursive R-tree over 2D
// bounding boxes. Once built with NewRTree the tree is immutable and safe
// for concurrent readers.
type RTree struct {
	// itemBounds is the caller's bounds, indexed by caller-facing ID.
	itemBounds []Bounds
	// itemIDs is a permutation: itemIDs[i] is the caller ID at leaf slot i.
	itemIDs []int32
	// childRefs stores child node indexes for internal nodes.
	childRefs []int32
	// nodes are laid out with leaves first, then internal levels; root is last.
	nodes []rtreeNode
	root  int32
}

type rtreeNode struct {
	bounds Bounds
	// first + count point into itemIDs if isLeaf, otherwise into childRefs.
	first  int32
	count  int32
	isLeaf bool
}

// NewRTree builds an R-tree over the given bounding boxes. Item IDs
// returned by queries are indexes into bounds.
func NewRTree(bounds []Bounds) *RTree {
	t := &RTree{itemBounds: append([]Bounds(nil), bounds...)}
	if len(bounds) == 0 {
		t.root = -1
		return t
	}
	ids := make([]int32, len(bounds))
	for i := range ids {
		ids[i] = int32(i)
	}
	// Leaf level: sort/tile items, pack them into leaf nodes.
	leaves := t.buildLeafLevel(ids)
	// Recursively build internal levels until one root.
	for len(leaves) > 1 {
		leaves = t.buildInternalLevel(leaves)
	}
	t.root = leaves[0]
	return t
}

// Len returns the number of items indexed.
func (t *RTree) Len() int { return len(t.itemBounds) }

// Bounds returns the R-tree's overall bounding box.
func (t *RTree) Bounds() Bounds {
	if t.root < 0 {
		return EmptyBounds()
	}
	return t.nodes[t.root].bounds
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
	if t.root < 0 || !q.Intersects(t.Bounds()) {
		return out
	}
	stack := []int32{t.root}
	for len(stack) > 0 {
		idx := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := t.nodes[idx]
		if !n.bounds.Intersects(q) {
			continue
		}
		if n.isLeaf {
			for i := int32(0); i < n.count; i++ {
				id := t.itemIDs[n.first+i]
				if t.itemBounds[id].Intersects(q) {
					out = append(out, id)
				}
			}
			continue
		}
		for i := int32(0); i < n.count; i++ {
			stack = append(stack, t.childRefs[n.first+i])
		}
	}
	return out
}

// Nearest returns the k item IDs whose bounding boxes are closest (by
// squared Euclidean point-to-bbox distance) to (x, y), in ascending
// distance order. Fewer than k IDs are returned if the tree is smaller.
func (t *RTree) Nearest(x, y float64, k int) []int32 {
	if t.root < 0 || k <= 0 {
		return nil
	}
	pq := &rtreeHeap{}
	heap.Init(pq)
	heap.Push(pq, rtreeQueue{node: t.root, dist: bboxDist(t.nodes[t.root].bounds, x, y)})

	out := make([]int32, 0, k)
	for pq.Len() > 0 && len(out) < k {
		top := heap.Pop(pq).(rtreeQueue)
		if top.isItem {
			out = append(out, top.item)
			continue
		}
		n := t.nodes[top.node]
		if n.isLeaf {
			for i := int32(0); i < n.count; i++ {
				id := t.itemIDs[n.first+i]
				heap.Push(pq, rtreeQueue{isItem: true, item: id, dist: bboxDist(t.itemBounds[id], x, y)})
			}
		} else {
			for i := int32(0); i < n.count; i++ {
				child := t.childRefs[n.first+i]
				heap.Push(pq, rtreeQueue{node: child, dist: bboxDist(t.nodes[child].bounds, x, y)})
			}
		}
	}
	return out
}

// --- build ---

func (t *RTree) buildLeafLevel(ids []int32) []int32 {
	sort.Slice(ids, func(i, j int) bool {
		return xCenter(t.itemBounds[ids[i]]) < xCenter(t.itemBounds[ids[j]])
	})
	M := RTreeNodeSize
	P := int(math.Ceil(float64(len(ids)) / float64(M))) // total leaves
	S := max(
		// stripes
		int(math.Ceil(math.Sqrt(float64(P)))), 1)
	stripeSize := max(int(math.Ceil(float64(len(ids))/float64(S))), M)

	var leaves []int32
	for i := 0; i < len(ids); i += stripeSize {
		end := min(i+stripeSize, len(ids))
		stripe := ids[i:end]
		sort.Slice(stripe, func(i, j int) bool {
			return yCenter(t.itemBounds[stripe[i]]) < yCenter(t.itemBounds[stripe[j]])
		})
		for j := 0; j < len(stripe); j += M {
			e := min(j+M, len(stripe))
			group := stripe[j:e]
			b := EmptyBounds()
			for _, id := range group {
				b = b.Union(t.itemBounds[id])
			}
			offset := int32(len(t.itemIDs))
			t.itemIDs = append(t.itemIDs, group...)
			leaves = append(leaves, t.appendNode(rtreeNode{
				bounds: b, first: offset, count: int32(len(group)), isLeaf: true,
			}))
		}
	}
	return leaves
}

func (t *RTree) buildInternalLevel(children []int32) []int32 {
	sort.Slice(children, func(i, j int) bool {
		return xCenter(t.nodes[children[i]].bounds) < xCenter(t.nodes[children[j]].bounds)
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
			return yCenter(t.nodes[stripe[i]].bounds) < yCenter(t.nodes[stripe[j]].bounds)
		})
		for j := 0; j < len(stripe); j += M {
			e := min(j+M, len(stripe))
			group := stripe[j:e]
			b := EmptyBounds()
			for _, id := range group {
				b = b.Union(t.nodes[id].bounds)
			}
			offset := int32(len(t.childRefs))
			t.childRefs = append(t.childRefs, group...)
			next = append(next, t.appendNode(rtreeNode{
				bounds: b, first: offset, count: int32(len(group)), isLeaf: false,
			}))
		}
	}
	return next
}

func (t *RTree) appendNode(n rtreeNode) int32 {
	t.nodes = append(t.nodes, n)
	return int32(len(t.nodes) - 1)
}

func xCenter(b Bounds) float64 { return (b.MinX + b.MaxX) / 2 }
func yCenter(b Bounds) float64 { return (b.MinY + b.MaxY) / 2 }

// bboxDist returns the squared Euclidean distance from (x, y) to the
// closest point on b. Zero if the point lies inside b.
func bboxDist(b Bounds, x, y float64) float64 {
	var dx, dy float64
	if x < b.MinX {
		dx = b.MinX - x
	} else if x > b.MaxX {
		dx = x - b.MaxX
	}
	if y < b.MinY {
		dy = b.MinY - y
	} else if y > b.MaxY {
		dy = y - b.MaxY
	}
	return dx*dx + dy*dy
}

// --- priority queue for Nearest ---

type rtreeQueue struct {
	node   int32
	item   int32
	isItem bool
	dist   float64
}

type rtreeHeap []rtreeQueue

func (h rtreeHeap) Len() int           { return len(h) }
func (h rtreeHeap) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h rtreeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *rtreeHeap) Push(x any)        { *h = append(*h, x.(rtreeQueue)) }
func (h *rtreeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

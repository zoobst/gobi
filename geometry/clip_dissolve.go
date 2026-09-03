package geometry

import (
	"sort"
	"sync"
)

// dissolveParallelThreshold is the smallest subtree size that
// spawns a goroutine in dissolveMerge. Below this the recursive
// merge stays sequential — goroutine setup + channel sync
// dominates the tiny-subtree union cost, and small
// (≤ threshold-polys) subtrees are typically already cheap.
// Above this every split-in-half fans out to two goroutines
// until the recursion bottoms out below the threshold.
//
// Empirically picked to hit ~2×–4× speedup on the polars-st
// bench dissolve shape (1k random polygons, mix of small /
// medium / large) on Apple M3 Pro — smaller thresholds burn
// scheduling overhead; larger ones under-utilize cores.
const dissolveParallelThreshold = 32

// Dissolve merges every polygon in geoms into their union. Inputs may be
// Polygon or MultiPolygon; other geometry types return an error. All inputs
// must agree on CRS (an unset CRS is treated as a wildcard).
//
// The implementation clusters inputs by bbox connectivity using an R-tree,
// then unites each cluster via a spatially-sorted divide-and-conquer merge
// (like shapely's unary_union). On fully-disjoint inputs this is
// near-linear; on heavily-overlapping inputs it's O(n log n) sweeps.
func Dissolve(geoms []Geometry) (Geometry, error) {
	if len(geoms) == 0 {
		return Polygon{}, nil
	}
	crs, err := dissolveValidate(geoms)
	if err != nil {
		return nil, err
	}

	// Bbox R-tree + union-find over bbox intersection.
	bounds := make([]Bounds, len(geoms))
	for i, g := range geoms {
		bounds[i] = g.Bounds()
	}
	uf := newUnionFind(len(geoms))
	tree := NewRTree(bounds)
	var buf []int32
	for i, b := range bounds {
		buf = tree.SearchInto(buf, b)
		for _, jRaw := range buf {
			j := int(jRaw)
			if j <= i {
				continue
			}
			uf.union(i, j)
		}
	}

	// Group indices by union-find root.
	groups := make(map[int][]int, len(geoms))
	for i := range geoms {
		r := uf.find(i)
		groups[r] = append(groups[r], i)
	}

	// Slice 25: cluster-level parallelism. Each union-find cluster
	// is topologically independent (bboxes don't cross between
	// clusters), so their dissolves can run concurrently. The
	// within-cluster dissolveMerge itself also fans out via
	// dissolveMergeParallel — together this cuts wall time
	// meaningfully on multi-cluster workloads (e.g. globally
	// scattered polygon sets with several dense sub-regions).
	type clusterResult struct {
		merged Geometry
		err    error
	}
	groupList := make([][]int, 0, len(groups))
	for _, idxs := range groups {
		groupList = append(groupList, idxs)
	}
	results := make([]clusterResult, len(groupList))
	var wg sync.WaitGroup
	// Trivially-small clusters aren't worth a goroutine — the
	// group-loop's tiny per-iter cost dominates goroutine setup.
	// Use the same threshold as dissolveMerge for consistency.
	const clusterParallelThreshold = dissolveParallelThreshold
	for i, idxs := range groupList {
		if len(idxs) < clusterParallelThreshold {
			results[i].merged, results[i].err = dissolveGroup(geoms, idxs, crs)
			continue
		}
		wg.Add(1)
		go func(idx int, ids []int) {
			defer wg.Done()
			results[idx].merged, results[idx].err = dissolveGroup(geoms, ids, crs)
		}(i, idxs)
	}
	wg.Wait()
	var polys []Polygon
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		polys = appendPolygons(polys, r.merged)
	}

	if len(polys) == 0 {
		return Polygon{CRSValue: crs}, nil
	}
	if len(polys) == 1 {
		p := polys[0]
		p.CRSValue = crs
		return p, nil
	}
	for i := range polys {
		polys[i].CRSValue = crs
	}
	return MultiPolygon{Polygons: polys, CRSValue: crs}, nil
}

func dissolveValidate(geoms []Geometry) (CRS, error) {
	var crs CRS
	for i, g := range geoms {
		if g == nil {
			return CRS{}, wrapDissolveErr(i, "nil geometry")
		}
		if err := requirePolygonal(g); err != nil {
			return CRS{}, err
		}
		c := g.CRS()
		if crs.Zero() {
			crs = c
		} else if !c.Zero() && !c.Equal(crs) {
			return CRS{}, ErrCRSMismatch
		}
	}
	if !crs.Zero() && !crs.Projected {
		return CRS{}, ErrGeographicCRS
	}
	return crs, nil
}

func wrapDissolveErr(i int, msg string) error {
	return &dissolveErr{index: i, msg: msg}
}

type dissolveErr struct {
	index int
	msg   string
}

func (e *dissolveErr) Error() string {
	return "geometry: dissolve: input " + itoa(e.index) + ": " + e.msg
}

// itoa is a tiny int-to-string helper local to this file (avoids pulling
// strconv into the base geometry package's dependency graph).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// dissolveGroup unites every geometry at the given indices via
// divide-and-conquer merge (shapely's `unary_union` strategy). The
// input is spatially pre-sorted so adjacent-in-slice ≈ adjacent-in-
// space, then recursively split in half. Each recursive merge is a
// single `Boolean(a, b, OpUnion)` between two internally-non-
// overlapping shapes — this avoids Martinez-Rueda's even-odd winding
// trap that hits when SAME-role polygons overlap in one big sweep.
//
// Total work: O(n log n) sweeps rather than the old O(n) linear-fold
// sweeps against a growing accumulator. Boundary-vertex numerical
// drift also collapses from O(n) hops to O(log n).
func dissolveGroup(geoms []Geometry, idxs []int, crs CRS) (Geometry, error) {
	if len(idxs) == 0 {
		return Polygon{CRSValue: crs}, nil
	}
	if len(idxs) == 1 {
		return geoms[idxs[0]], nil
	}
	// Spatially sort by bounding-box center X (then Y). This keeps
	// pairs at every recursion level roughly adjacent, which produces
	// simpler intermediate topology and fewer sweep events per merge.
	ordered := append([]int(nil), idxs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		bi, bj := geoms[ordered[i]].Bounds(), geoms[ordered[j]].Bounds()
		cxi, cxj := (bi.MinX+bi.MaxX)/2, (bj.MinX+bj.MaxX)/2
		if cxi != cxj {
			return cxi < cxj
		}
		return (bi.MinY+bi.MaxY)/2 < (bj.MinY+bj.MaxY)/2
	})
	return dissolveMerge(geoms, ordered)
}

// dissolveMerge recursively halves the input slice and unions
// the two halves. Base cases: 0 → empty polygon, 1 → passthrough.
//
// Slice 25 parallelism: above `dissolveParallelThreshold` the
// left and right halves are unioned in parallel goroutines,
// then the two results are unioned sequentially. On a
// divide-and-conquer merge over N polygons this fans out to
// ~min(N/threshold, GOMAXPROCS) concurrent Boolean(OpUnion)
// calls at the top of the tree — the sweep engine already
// releases sync.Pool-tracked events per session, so concurrent
// clipSessions don't cross-contaminate.
//
// The cross-goroutine boundary is one Geometry return value
// per subtree — no shared mutable state, no locks needed.
func dissolveMerge(geoms []Geometry, idxs []int) (Geometry, error) {
	switch len(idxs) {
	case 0:
		return Polygon{}, nil
	case 1:
		return geoms[idxs[0]], nil
	}
	mid := len(idxs) / 2
	if len(idxs) >= dissolveParallelThreshold {
		return dissolveMergeParallel(geoms, idxs, mid)
	}
	left, err := dissolveMerge(geoms, idxs[:mid])
	if err != nil {
		return nil, err
	}
	right, err := dissolveMerge(geoms, idxs[mid:])
	if err != nil {
		return nil, err
	}
	return Boolean(left, right, OpUnion, ClipOptions{})
}

// dissolveMergeParallel runs the two subtree merges on separate
// goroutines and combines their results. Both subtrees recurse
// into dissolveMerge which itself may fan out further — until
// each subtree drops below dissolveParallelThreshold and takes
// the sequential path.
func dissolveMergeParallel(geoms []Geometry, idxs []int, mid int) (Geometry, error) {
	var (
		leftGeom, rightGeom Geometry
		leftErr, rightErr   error
		wg                  sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		leftGeom, leftErr = dissolveMerge(geoms, idxs[:mid])
	}()
	go func() {
		defer wg.Done()
		rightGeom, rightErr = dissolveMerge(geoms, idxs[mid:])
	}()
	wg.Wait()
	if leftErr != nil {
		return nil, leftErr
	}
	if rightErr != nil {
		return nil, rightErr
	}
	return Boolean(leftGeom, rightGeom, OpUnion, ClipOptions{})
}

// unionFind is a compact disjoint-set with path-compression and rank-based
// union. Scoped to Dissolve to keep the geometry package's public surface
// minimal.
type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (uf *unionFind) find(i int) int {
	for uf.parent[i] != i {
		uf.parent[i] = uf.parent[uf.parent[i]]
		i = uf.parent[i]
	}
	return i
}

func (uf *unionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return
	}
	if uf.rank[ra] < uf.rank[rb] {
		uf.parent[ra] = rb
	} else if uf.rank[ra] > uf.rank[rb] {
		uf.parent[rb] = ra
	} else {
		uf.parent[rb] = ra
		uf.rank[ra]++
	}
}

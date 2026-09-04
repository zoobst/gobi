package gobi

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/geometry"
)

// sjoinPointsInPolygonsFastPath is the Slice 16 WKB-native SJoin
// refine for the Points × Polygons shape. Bypasses both
// decodeGeometryColumn passes:
//
//   - Left: extract (x, y) per row via a small WKB-Point scanner.
//     Non-Point rows fall through to a decoded-Geometry cache
//     lazily populated on first miss.
//   - Right: keep raw WKB bytes; extract bounds via BoundsFromWKB.
//     R-tree built from those bounds (unchanged interface).
//     Non-Polygon rows likewise fall through to lazy AoS parse.
//
// Refine kernel is `geometry.PIPInclusiveFromWKB(rightWKB, x, y)`
// for the fast-path shape, which matches AoS
// `pointInPolygon(pt, poly)` (boundary-inclusive) exactly.
//
// Returns (leftIdxs, rightIdxs, ok). ok=false signals the caller
// should take the AoS path — the fast path only fires when both
// columns are homogeneously the expected shape AND the predicate
// is Intersects or Within (both reduce to point-in-polygon on
// this shape).
func sjoinPointsInPolygonsFastPath(
	lGeom, rGeom Series,
	pred SpatialPredicate,
	workers int,
) (leftIdxs, rightIdxs []int, ok bool) {
	// Predicate gate: only Intersects/Within reduce cleanly to
	// PIP-inclusive on Points × Polygons. SPContains(pt, poly) is
	// trivially false (a point can't contain a non-degenerate
	// polygon); let the AoS path handle it uniformly.
	if pred != SPIntersects && pred != SPWithin {
		return nil, nil, false
	}

	// Shape gate: peek both columns to confirm Points × Polygons.
	// A single mismatched non-null row disables the fast path.
	leftPts, leftFast, err := extractLeftPoints(lGeom)
	if err != nil || !leftFast {
		return nil, nil, false
	}
	rightWKBs, rightBounds, rightFast, err := extractRightPolygons(rGeom)
	if err != nil || !rightFast {
		return nil, nil, false
	}

	tree := geometry.NewRTree(rightBounds)
	n := len(leftPts)

	if workers <= 1 || n < SJoinMinParallelRows {
		l, r := sjoinPointsScanRange(leftPts, rightWKBs, tree, 0, n, nil)
		return l, r, true
	}

	workers = min(workers, n)
	chunk := (n + workers - 1) / workers
	type shard struct{ l, r []int }
	shards := make([]shard, workers)

	var wg sync.WaitGroup
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, n)
		if start >= end {
			continue
		}
		idx, s, e := w, start, end
		wg.Go(func() {
			var scratch []int32
			l, r := sjoinPointsScanRange(leftPts, rightWKBs, tree, s, e, scratch)
			shards[idx] = shard{l: l, r: r}
		})
	}
	wg.Wait()

	var total int
	for _, sh := range shards {
		total += len(sh.l)
	}
	leftIdxs = make([]int, 0, total)
	rightIdxs = make([]int, 0, total)
	for _, sh := range shards {
		leftIdxs = append(leftIdxs, sh.l...)
		rightIdxs = append(rightIdxs, sh.r...)
	}
	return leftIdxs, rightIdxs, true
}

// sjoinPoint is the compact left-row representation for the fast
// path: coords plus a null flag. Avoids an interface box per row.
type sjoinPoint struct {
	x, y float64
	null bool
}

// sjoinPointsScanRange evaluates PIPInclusiveFromWKB for each
// left-point × R-tree-candidate pair in [start, end).
func sjoinPointsScanRange(
	leftPts []sjoinPoint,
	rightWKBs [][]byte,
	tree *geometry.RTree,
	start, end int,
	scratch []int32,
) (leftIdxs, rightIdxs []int) {
	nHint := end - start
	leftIdxs = make([]int, 0, nHint)
	rightIdxs = make([]int, 0, nHint)
	for lRow := start; lRow < end; lRow++ {
		p := leftPts[lRow]
		if p.null {
			continue
		}
		q := geometry.Bounds{MinX: p.x, MinY: p.y, MaxX: p.x, MaxY: p.y}
		scratch = tree.SearchInto(scratch, q)
		for _, rIdx := range scratch {
			wkb := rightWKBs[rIdx]
			if wkb == nil {
				continue
			}
			inside, err := geometry.PIPInclusiveFromWKB(wkb, p.x, p.y)
			if err != nil || !inside {
				continue
			}
			leftIdxs = append(leftIdxs, lRow)
			rightIdxs = append(rightIdxs, int(rIdx))
		}
	}
	return leftIdxs, rightIdxs
}

// extractLeftPoints walks the left geometry column and extracts
// (x, y) per row. fastPath=false when any non-null row is not a
// Point (falls back to full AoS path — no lazy hybrid attempted).
func extractLeftPoints(s Series) (points []sjoinPoint, fastPath bool, err error) {
	points = make([]sjoinPoint, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, false, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				points = append(points, sjoinPoint{null: true})
				continue
			}
			wkb := bin.Value(i)
			typ, hasZ, terr := geometry.WKBTypeCode(wkb)
			if terr != nil {
				return nil, false, terr
			}
			// Only pure Points supported — MultiPoints could work but
			// need an inner loop; skip for scope.
			if typ != 1 { // wkbPoint
				return nil, false, nil
			}
			x, y, xerr := readWKBPointCoords(wkb, hasZ)
			if xerr != nil {
				return nil, false, xerr
			}
			points = append(points, sjoinPoint{x: x, y: y})
		}
	}
	return points, true, nil
}

// extractRightPolygons walks the right geometry column, returning
// per-row WKB blobs plus per-row bounds. fastPath=false when any
// non-null row isn't a Polygon or MultiPolygon.
func extractRightPolygons(s Series) (wkbs [][]byte, bounds []geometry.Bounds, fastPath bool, err error) {
	wkbs = make([][]byte, 0, s.Len())
	bounds = make([]geometry.Bounds, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, nil, false, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				wkbs = append(wkbs, nil)
				bounds = append(bounds, geometry.EmptyBounds())
				continue
			}
			wkb := bin.Value(i)
			typ, _, terr := geometry.WKBTypeCode(wkb)
			if terr != nil {
				return nil, nil, false, terr
			}
			// Polygon (3) or MultiPolygon (6) only.
			if typ != 3 && typ != 6 {
				return nil, nil, false, nil
			}
			b, berr := geometry.BoundsFromWKB(wkb)
			if berr != nil {
				return nil, nil, false, berr
			}
			wkbs = append(wkbs, wkb)
			bounds = append(bounds, b)
		}
	}
	return wkbs, bounds, true, nil
}

// rightGeomCache is a race-safe lazy cache of the right-side AoS
// Geometry per row. Each slot's `once` guarantees ParseWKB runs at
// most once, and the geom / err fields are only ever read after
// once.Do has returned — so concurrent workers hitting the same
// slot see the same fully-initialized values without torn reads.
//
// Replaces the pre-review "shared []geometry.Geometry with
// last-writer-wins" pattern, which was incorrect: a
// geometry.Geometry interface value is two words (type + data
// pointer), and unsynchronized writes can be observed torn by a
// concurrent reader — the two words are not guaranteed to update
// atomically, and even if the same underlying WKB were parsed
// twice, ParseWKB returns fresh distinct structs each call, so
// the data pointers differ.
type rightGeomCache struct {
	slots []rightGeomSlot
}

type rightGeomSlot struct {
	once sync.Once
	geom geometry.Geometry
	err  error
}

func newRightGeomCache(n int) *rightGeomCache {
	return &rightGeomCache{slots: make([]rightGeomSlot, n)}
}

// get returns the parsed Geometry for slot i, running ParseWKB
// via once.Do the first time (concurrent callers block until the
// first completes). Subsequent calls return the cached value with
// no synchronization cost beyond the once fast-path.
func (c *rightGeomCache) get(i int, wkb []byte) (geometry.Geometry, error) {
	s := &c.slots[i]
	s.once.Do(func() {
		s.geom, s.err = geometry.ParseWKB(wkb)
	})
	return s.geom, s.err
}

// sjoinLine is the compact left-row representation for the
// LineString × Polygon SJoin fast path (Slice 20c). Holds the
// line's coord slabs plus its bounding box; the WKB blob is
// retained only for the "boundary crossing without vertex
// inside" AoS fallback.
type sjoinLine struct {
	xs, ys []float64
	bounds geometry.Bounds
	wkb    []byte
	null   bool
}

// sjoinLinesInPolygonsFastPath is the Slice 20c fast path for
// SJoin with left = LineString, right = Polygon/MultiPolygon,
// pred = SPIntersects. Skips both decodeGeometryColumn passes:
//
//   - Left: extract line coord slabs via LineStringViewFromWKB
//     (Slice 10) + bounds via BoundsFromWKB (Slice 2).
//   - Right: keep raw WKB + bounds. R-tree built from bounds.
//
// Refine kernel (per left-line × right-polygon candidate):
//
//  1. Any left-line vertex inside the right polygon via
//     PIPInclusiveFromWKB → definite match.
//  2. No vertex inside → AoS fallback (line-segment × polygon-
//     ring crossing test). Handles the case where the line
//     "grazes" the polygon (segments cross boundary but no
//     vertex is inside).
//
// SPWithin (Slice 21e): fast path when the right polygon is
// convex — every left-line vertex inside convex right implies
// every line segment is inside (segments in convex sets stay
// in convex sets). Non-convex right falls back to AoS.
//
// SPContains(line, polygon) is trivially false (a line cannot
// contain a 2D shape) — falls back to AoS which returns false
// uniformly.
func sjoinLinesInPolygonsFastPath(
	lGeom, rGeom Series,
	pred SpatialPredicate,
	workers int,
) (leftIdxs, rightIdxs []int, ok bool) {
	if pred != SPIntersects && pred != SPWithin {
		return nil, nil, false
	}
	leftLines, leftFast, err := extractLeftLines(lGeom)
	if err != nil || !leftFast {
		return nil, nil, false
	}
	rightWKBs, rightBounds, rightConvex, rightFast, err := extractRightPolygonsWithConvexity(rGeom)
	if err != nil || !rightFast {
		return nil, nil, false
	}

	// Right-side geometry cache — lazily populated on the first
	// AoS fallback for a given right row via sync.Once per slot
	// (race-safe under concurrent workers).
	rightGeoms := newRightGeomCache(len(rightWKBs))

	tree := geometry.NewRTree(rightBounds)
	n := len(leftLines)

	scan := func(start, end int, scratch []int32) (l, r []int) {
		return sjoinLinesScanRange(leftLines, rightWKBs, rightConvex, rightGeoms, tree, pred, start, end, scratch)
	}

	if workers <= 1 || n < SJoinMinParallelRows {
		l, r := scan(0, n, nil)
		return l, r, true
	}
	workers = min(workers, n)
	chunk := (n + workers - 1) / workers
	type shard struct{ l, r []int }
	shards := make([]shard, workers)
	var wg sync.WaitGroup
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, n)
		if start >= end {
			continue
		}
		idx, s, e := w, start, end
		wg.Go(func() {
			var scratch []int32
			l, r := scan(s, e, scratch)
			shards[idx] = shard{l: l, r: r}
		})
	}
	wg.Wait()
	var total int
	for _, sh := range shards {
		total += len(sh.l)
	}
	leftIdxs = make([]int, 0, total)
	rightIdxs = make([]int, 0, total)
	for _, sh := range shards {
		leftIdxs = append(leftIdxs, sh.l...)
		rightIdxs = append(rightIdxs, sh.r...)
	}
	return leftIdxs, rightIdxs, true
}

// sjoinLinesScanRange runs the refine loop for [start, end).
// Per-predicate dispatch:
//
//   - SPIntersects: any line vertex inside polygon → match;
//     otherwise AoS fallback for boundary-crossing case.
//   - SPWithin: fast path when the polygon is convex + every
//     line vertex inside → match. Non-convex polygon falls
//     back to AoS.
func sjoinLinesScanRange(
	leftLines []sjoinLine,
	rightWKBs [][]byte,
	rightConvex []bool,
	rightGeoms *rightGeomCache,
	tree *geometry.RTree,
	pred SpatialPredicate,
	start, end int,
	scratch []int32,
) (leftIdxs, rightIdxs []int) {
	nHint := end - start
	leftIdxs = make([]int, 0, nHint)
	rightIdxs = make([]int, 0, nHint)
	geomPred := pred.toGeometry()
	for lRow := start; lRow < end; lRow++ {
		line := leftLines[lRow]
		if line.null {
			continue
		}
		scratch = tree.SearchInto(scratch, line.bounds)
		for _, rIdx := range scratch {
			wkb := rightWKBs[rIdx]
			if wkb == nil {
				continue
			}
			matched, resolved := lineInPolyRefine(line, wkb, rightConvex[rIdx], pred)
			if !resolved {
				// AoS fallback — either line grazes polygon
				// boundary (Intersects) or right polygon is
				// non-convex (Within).
				rg, err := rightGeoms.get(int(rIdx), wkb)
				if err != nil {
					continue
				}
				lg := geometry.LineString{
					Points: pointsFromXY(line.xs, line.ys),
				}
				matched = geometry.Test(geomPred, lg, rg)
			}
			if !matched {
				continue
			}
			leftIdxs = append(leftIdxs, lRow)
			rightIdxs = append(rightIdxs, int(rIdx))
		}
	}
	return leftIdxs, rightIdxs
}

// lineInPolyRefine runs the per-predicate SoA check for one
// LineString × Polygon pair. Returns (matched, resolved).
// resolved=false means the fast path couldn't decide — AoS
// fallback required.
func lineInPolyRefine(line sjoinLine, polyWKB []byte, polyConvex bool, pred SpatialPredicate) (matched, resolved bool) {
	switch pred {
	case SPIntersects:
		for i := range line.xs {
			inside, err := geometry.PIPInclusiveFromWKB(polyWKB, line.xs[i], line.ys[i])
			if err == nil && inside {
				return true, true
			}
		}
		return false, false
	case SPWithin:
		if !polyConvex {
			return false, false
		}
		for i := range line.xs {
			inside, err := geometry.PIPInclusiveFromWKB(polyWKB, line.xs[i], line.ys[i])
			if err != nil || !inside {
				return false, true
			}
		}
		return true, true
	}
	return false, false
}

// pointsFromXY materializes an AoS []Point from parallel slabs
// for the AoS fallback path. Only called when the vertex-inside
// fast path misses; matched rows never allocate here.
func pointsFromXY(xs, ys []float64) []geometry.Point {
	pts := make([]geometry.Point, len(xs))
	for i := range xs {
		pts[i] = geometry.Point{X: xs[i], Y: ys[i]}
	}
	return pts
}

// extractLeftLines walks the left column: per-row peek type via
// WKBTypeCode; if it's a LineString, materialize view slabs +
// bounds. Any non-LineString non-null row disables the fast path.
func extractLeftLines(s Series) (lines []sjoinLine, fastPath bool, err error) {
	lines = make([]sjoinLine, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, false, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				lines = append(lines, sjoinLine{null: true})
				continue
			}
			wkb := bin.Value(i)
			typ, _, terr := geometry.WKBTypeCode(wkb)
			if terr != nil {
				return nil, false, terr
			}
			// LineString only (type 2). MultiLineString could be added
			// in a follow-up but is not covered here.
			if typ != 2 {
				return nil, false, nil
			}
			view, verr := geometry.LineStringViewFromWKB(wkb)
			if verr != nil {
				return nil, false, verr
			}
			b, berr := geometry.BoundsFromWKB(wkb)
			if berr != nil {
				return nil, false, berr
			}
			lines = append(lines, sjoinLine{
				xs: view.Xs, ys: view.Ys, bounds: b, wkb: wkb,
			})
		}
	}
	return lines, true, nil
}

// sjoinPoly is the compact left/right-row representation for the
// Polygon-family × Polygon-family SJoin fast path (Slice 20d /
// 21c-d + Slice 24 MultiPolygon extension). Holds each sub-
// polygon's exterior ring coord slabs plus bounds and a pre-
// computed convexity flag.
//
// # extRings shape
//
// One `extRing` per sub-polygon exterior:
//   - Single-ring Polygon → len(extRings) == 1
//   - MultiPolygon        → len(extRings) == number of sub-polygons
//
// Interior rings (holes) are NOT stored. For SPIntersects the
// vertex-inside check only needs exterior vertices (holes'
// vertices being inside another polygon still means the two
// polygons intersect on the boundary — but for the fast-path
// "any exterior vertex inside" heuristic we skip holes).
//
// # convex
//
// True iff the polygon is a single-ring convex Polygon (no
// holes, one exterior ring, convex winding). MultiPolygon and
// polygons-with-holes always convex=false. Used by the Slice-21
// SPWithin/SPContains gate.
type sjoinPoly struct {
	extRings []extRing
	bounds   geometry.Bounds
	wkb      []byte
	convex   bool
	null     bool
}

// extRing holds one exterior ring's coord slabs.
type extRing struct {
	xs, ys []float64
}

// sjoinPolygonsInPolygonsFastPath is the SJoin fast path for
// Polygon-family × Polygon-family inputs (Slice 20d + Slice 24
// MultiPolygon extension). Handles three predicates via per-
// predicate refine dispatch:
//
//   - SPIntersects (Slice 20d): bilateral vertex-inside check
//     with AoS fallback for edge-crossing-only pairs. Iterates
//     every exterior ring on both sides so MultiPolygon inputs
//     get the same treatment as single Polygons.
//   - SPWithin (Slice 21c): A ⊆ B. Fast path when right is a
//     single-ring convex Polygon + every A exterior vertex
//     inside right → match. MultiPolygon or concave right falls
//     back to AoS.
//   - SPContains (Slice 21d): B ⊆ A. Symmetric — fast path when
//     left is a single-ring convex Polygon.
//
// Slice 24 widening: `extractPolygonFamily` now accepts both
// Polygon (type 3) and MultiPolygon (type 6). Previously any
// MultiPolygon row disabled the fast path for the entire
// column (measured as a 720× slowdown vs polars-st on a corpus
// that mixed ~5% MultiPolygons into random Polygons — the poly×
// poly self-join fell entirely to AoS Test at 5.7 s for 1k×1k).
//
// Polygons with holes are accepted; only the EXTERIOR ring is
// stored per sub-polygon. Hole-vertex-inside checks would double
// the SPIntersects work for little correctness benefit (a hole
// vertex inside the other polygon just means the two touch on
// the boundary, which vertex-inside on the exterior would also
// catch if the polygons truly intersect).
func sjoinPolygonsInPolygonsFastPath(
	lGeom, rGeom Series,
	pred SpatialPredicate,
	workers int,
) (leftIdxs, rightIdxs []int, ok bool) {
	if pred != SPIntersects && pred != SPWithin && pred != SPContains {
		return nil, nil, false
	}
	leftPolys, leftFast, err := extractPolygonFamily(lGeom)
	if err != nil || !leftFast {
		return nil, nil, false
	}
	rightPolys, rightFast, err := extractPolygonFamily(rGeom)
	if err != nil || !rightFast {
		return nil, nil, false
	}

	rightBounds := make([]geometry.Bounds, len(rightPolys))
	for i, p := range rightPolys {
		if p.null {
			// Bounds{} is NOT Empty() — Empty() requires MinX > MaxX
			// while the zero value has MinX == MaxX == 0. Without
			// an explicit empty sentinel, the R-tree treats null
			// rows as a real bbox at the origin and returns them
			// as candidates for any query touching (0,0), forcing
			// the refine loop to reject them via `rp.null`. Wasted
			// CPU. Set the sentinel so the R-tree skips them.
			rightBounds[i] = geometry.EmptyBounds()
			continue
		}
		rightBounds[i] = p.bounds
	}
	tree := geometry.NewRTree(rightBounds)

	// Lazy AoS cache — populated on the first fallback for a given
	// right row. Reused across workers (see note in
	// sjoinLinesInPolygonsFastPath about ParseWKB idempotence).
	rightGeoms := newRightGeomCache(len(rightPolys))
	n := len(leftPolys)

	scan := func(start, end int, scratch []int32) (l, r []int) {
		return sjoinPolysScanRange(leftPolys, rightPolys, rightGeoms, tree, pred, start, end, scratch)
	}

	if workers <= 1 || n < SJoinMinParallelRows {
		l, r := scan(0, n, nil)
		return l, r, true
	}
	workers = min(workers, n)
	chunk := (n + workers - 1) / workers
	type shard struct{ l, r []int }
	shards := make([]shard, workers)
	var wg sync.WaitGroup
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, n)
		if start >= end {
			continue
		}
		idx, s, e := w, start, end
		wg.Go(func() {
			var scratch []int32
			l, r := scan(s, e, scratch)
			shards[idx] = shard{l: l, r: r}
		})
	}
	wg.Wait()
	var total int
	for _, sh := range shards {
		total += len(sh.l)
	}
	leftIdxs = make([]int, 0, total)
	rightIdxs = make([]int, 0, total)
	for _, sh := range shards {
		leftIdxs = append(leftIdxs, sh.l...)
		rightIdxs = append(rightIdxs, sh.r...)
	}
	return leftIdxs, rightIdxs, true
}

// sjoinPolysScanRange runs the refine loop for [start, end).
// Dispatches per-predicate to the appropriate check + AoS
// fallback shape.
func sjoinPolysScanRange(
	leftPolys, rightPolys []sjoinPoly,
	rightGeoms *rightGeomCache,
	tree *geometry.RTree,
	pred SpatialPredicate,
	start, end int,
	scratch []int32,
) (leftIdxs, rightIdxs []int) {
	nHint := end - start
	leftIdxs = make([]int, 0, nHint)
	rightIdxs = make([]int, 0, nHint)
	geomPred := pred.toGeometry()
	for lRow := start; lRow < end; lRow++ {
		lp := leftPolys[lRow]
		if lp.null {
			continue
		}
		scratch = tree.SearchInto(scratch, lp.bounds)
		for _, rIdx := range scratch {
			rp := rightPolys[rIdx]
			if rp.null {
				continue
			}
			matched, resolved := polyPolyRefine(lp, rp, pred)
			if !resolved {
				// AoS fallback: parse both sides and run the AoS
				// predicate. The vertex-inside fast paths above
				// couldn't resolve this pair.
				rg, err := rightGeoms.get(int(rIdx), rp.wkb)
				if err != nil {
					continue
				}
				lg, err := geometry.ParseWKB(lp.wkb)
				if err != nil {
					continue
				}
				matched = geometry.Test(geomPred, lg, rg)
			}
			if !matched {
				continue
			}
			leftIdxs = append(leftIdxs, lRow)
			rightIdxs = append(rightIdxs, int(rIdx))
		}
	}
	return leftIdxs, rightIdxs
}

// polyPolyRefine runs the per-predicate SoA check. Returns
// (matched, resolved). resolved=false means the fast path
// couldn't decide (concave container for SPWithin/SPContains,
// or Intersects vertex-inside miss); the caller must run AoS.
//
// Iterates over every exterior ring of both sides — Slice 24
// extends the check from single-ring Polygon to MultiPolygon
// by walking `sjoinPoly.extRings` rather than a single
// (extXs, extYs) pair. The vertex-inside check via
// `PIPInclusiveFromWKB` on the OTHER side's WKB already
// handles both Polygon and MultiPolygon on the queried side,
// so the two dimensions of MultiPolygon-ness (subject and
// clipper) compose cleanly.
func polyPolyRefine(lp, rp sjoinPoly, pred SpatialPredicate) (matched, resolved bool) {
	switch pred {
	case SPIntersects:
		// Left exterior vertices inside right?
		for _, ring := range lp.extRings {
			for i := range ring.xs {
				inside, err := geometry.PIPInclusiveFromWKB(rp.wkb, ring.xs[i], ring.ys[i])
				if err == nil && inside {
					return true, true
				}
			}
		}
		// Right exterior vertices inside left?
		for _, ring := range rp.extRings {
			for i := range ring.xs {
				inside, err := geometry.PIPInclusiveFromWKB(lp.wkb, ring.xs[i], ring.ys[i])
				if err == nil && inside {
					return true, true
				}
			}
		}
		// Miss — must AoS for edge-crossing case.
		return false, false
	case SPWithin:
		// Left ⊆ Right requires convex right (single-ring convex
		// Polygon only) + every left exterior vertex inside.
		// MultiPolygon or holes-having sides never pass convex.
		if !rp.convex {
			return false, false
		}
		for _, ring := range lp.extRings {
			for i := range ring.xs {
				inside, err := geometry.PIPInclusiveFromWKB(rp.wkb, ring.xs[i], ring.ys[i])
				if err != nil || !inside {
					return false, true
				}
			}
		}
		return true, true
	case SPContains:
		// Symmetric: Right ⊆ Left requires convex left.
		if !lp.convex {
			return false, false
		}
		for _, ring := range rp.extRings {
			for i := range ring.xs {
				inside, err := geometry.PIPInclusiveFromWKB(lp.wkb, ring.xs[i], ring.ys[i])
				if err != nil || !inside {
					return false, true
				}
			}
		}
		return true, true
	}
	return false, false
}

// extractPolygonFamily walks a geometry column and materializes
// each row's exterior ring coord slabs plus bounds. Accepts both
// single-Polygon and MultiPolygon rows (Slice 24 widening of the
// original single-ring-only extract). Any non-polygon-family row
// (Point / LineString / etc.) disables the fast path for the
// entire column; the caller falls back to AoS uniformly.
//
// Convexity is set true only for single-ring Polygons with a
// convex exterior — MultiPolygon and holes-having polygons stay
// convex=false so the Slice-21 SPWithin/SPContains gate rejects
// them correctly.
//
// Polygons with holes are accepted; only the EXTERIOR ring is
// stored (SPIntersects vertex-inside check ignores holes since a
// hole vertex being inside the other polygon still makes the two
// intersect on the boundary).
func extractPolygonFamily(s Series) (polys []sjoinPoly, fastPath bool, err error) {
	polys = make([]sjoinPoly, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, false, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				polys = append(polys, sjoinPoly{null: true})
				continue
			}
			wkb := bin.Value(i)
			typ, _, terr := geometry.WKBTypeCode(wkb)
			if terr != nil {
				return nil, false, terr
			}
			b, berr := geometry.BoundsFromWKB(wkb)
			if berr != nil {
				return nil, false, berr
			}
			switch typ {
			case 3: // Polygon
				rings, verr := geometry.PolygonRingViewsFromWKB(wkb)
				if verr != nil {
					return nil, false, verr
				}
				if len(rings) == 0 {
					polys = append(polys, sjoinPoly{null: true})
					continue
				}
				extRings := []extRing{{xs: rings[0].Xs, ys: rings[0].Ys}}
				convex := false
				if len(rings) == 1 {
					convex = ringConvexFromXY(rings[0].Xs, rings[0].Ys)
				}
				polys = append(polys, sjoinPoly{
					extRings: extRings,
					bounds:   b,
					wkb:      wkb,
					convex:   convex,
				})
			case 6: // MultiPolygon
				subPolys, verr := geometry.MultiPolygonRingViewsFromWKB(wkb)
				if verr != nil {
					return nil, false, verr
				}
				extRings := make([]extRing, 0, len(subPolys))
				for _, sub := range subPolys {
					if len(sub) == 0 {
						continue
					}
					extRings = append(extRings, extRing{xs: sub[0].Xs, ys: sub[0].Ys})
				}
				if len(extRings) == 0 {
					polys = append(polys, sjoinPoly{null: true})
					continue
				}
				polys = append(polys, sjoinPoly{
					extRings: extRings,
					bounds:   b,
					wkb:      wkb,
					// MultiPolygon → convex gate defaults false;
					// SPWithin/SPContains fall to AoS.
				})
			default:
				// Not a polygon family — disable fast path.
				return nil, false, nil
			}
		}
	}
	return polys, true, nil
}

// ringConvexFromXY reports whether the ring stored in parallel
// Xs / Ys slabs winds consistently (all left- or right-turns —
// i.e., is a convex ring). Mirrors geometry.ringIsConvex which
// takes []Point; kept local to sjoin_fast to avoid inflating
// the geometry package's exported surface.
func ringConvexFromXY(xs, ys []float64) bool {
	n := min(len(xs), len(ys))
	if n < 3 {
		return false
	}
	// Strip trailing closing vertex.
	if xs[0] == xs[n-1] && ys[0] == ys[n-1] {
		n--
	}
	if n < 3 {
		return false
	}
	sign := 0
	for i := 0; i < n; i++ {
		ax, ay := xs[i], ys[i]
		bx, by := xs[(i+1)%n], ys[(i+1)%n]
		cx, cy := xs[(i+2)%n], ys[(i+2)%n]
		cross := (bx-ax)*(cy-by) - (by-ay)*(cx-bx)
		if cross > 0 {
			if sign < 0 {
				return false
			}
			sign = 1
		} else if cross < 0 {
			if sign > 0 {
				return false
			}
			sign = -1
		}
	}
	return sign != 0
}

// extractRightPolygonsWithConvexity is the Slice-21e variant of
// extractRightPolygons that also computes each polygon's convexity
// flag (needed for the LineString × Polygon SPWithin fast path).
// MultiPolygon rows are marked non-convex (convex-Multi containers
// are rare enough that supporting them isn't worth the extra
// scanner code).
func extractRightPolygonsWithConvexity(s Series) (wkbs [][]byte, bounds []geometry.Bounds, convex []bool, fastPath bool, err error) {
	wkbs = make([][]byte, 0, s.Len())
	bounds = make([]geometry.Bounds, 0, s.Len())
	convex = make([]bool, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, nil, nil, false, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				wkbs = append(wkbs, nil)
				bounds = append(bounds, geometry.EmptyBounds())
				convex = append(convex, false)
				continue
			}
			wkb := bin.Value(i)
			typ, _, terr := geometry.WKBTypeCode(wkb)
			if terr != nil {
				return nil, nil, nil, false, terr
			}
			if typ != 3 && typ != 6 {
				return nil, nil, nil, false, nil
			}
			b, berr := geometry.BoundsFromWKB(wkb)
			if berr != nil {
				return nil, nil, nil, false, berr
			}
			isConvex := false
			if typ == 3 { // single Polygon; convexity check on exterior
				rings, verr := geometry.PolygonRingViewsFromWKB(wkb)
				if verr == nil && len(rings) == 1 {
					isConvex = ringConvexFromXY(rings[0].Xs, rings[0].Ys)
				}
			}
			wkbs = append(wkbs, wkb)
			bounds = append(bounds, b)
			convex = append(convex, isConvex)
		}
	}
	return wkbs, bounds, convex, true, nil
}

// readWKBPointCoords extracts (x, y) from a Point / PointZ WKB
// blob. Caller has already confirmed the type code via WKBTypeCode.
func readWKBPointCoords(data []byte, hasZ bool) (float64, float64, error) {
	// Header 5 bytes (byte order + type code) + coord bytes.
	need := 5 + 16
	if hasZ {
		need = 5 + 24
	}
	if len(data) < need {
		return 0, 0, fmt.Errorf("gobi: WKB Point too short: %d bytes", len(data))
	}
	var bo binary.ByteOrder
	switch data[0] {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return 0, 0, fmt.Errorf("gobi: invalid WKB byte order %d", data[0])
	}
	x := math.Float64frombits(bo.Uint64(data[5:13]))
	y := math.Float64frombits(bo.Uint64(data[13:21]))
	return x, y, nil
}

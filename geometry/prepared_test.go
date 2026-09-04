package geometry

import (
	"math/rand"
	"sync"
	"testing"
)

// TestTestPrepared_MatchesTest_PointVsPolygon — the fast paths
// must produce the same answer as Test() for every predicate on
// the Point×Polygon shape (both orderings).
func TestTestPrepared_MatchesTest_PointVsPolygon(t *testing.T) {
	poly := Polygon{Rings: [][]Point{
		// Exterior L-shape.
		{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
			{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
		// Hole in the lower-left corner.
		{{X: 1, Y: 1}, {X: 2, Y: 1}, {X: 2, Y: 2}, {X: 1, Y: 2}, {X: 1, Y: 1}},
	}}
	rng := rand.New(rand.NewSource(1))
	pPoly := Prepare(poly)
	preds := []Predicate{PredIntersects, PredContains, PredWithin, PredDisjoint}
	for range 200 {
		pt := Point{X: rng.Float64()*14 - 2, Y: rng.Float64()*14 - 2}
		pPt := Prepare(pt)
		for _, pred := range preds {
			// (Point, Polygon)
			want := Test(pred, pt, poly)
			got := TestPrepared(pred, pPt, pPoly)
			if got != want {
				t.Errorf("(pt, poly) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
			// (Polygon, Point) — swapped
			want = Test(pred, poly, pt)
			got = TestPrepared(pred, pPoly, pPt)
			if got != want {
				t.Errorf("(poly, pt) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestTestPrepared_MatchesTest_PointVsMultiPolygon — same as
// above but on the MultiPolygon shape. Uses two disjoint squares
// to exercise the "iterate polys until one matches" loop.
func TestTestPrepared_MatchesTest_PointVsMultiPolygon(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{{
			{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10},
		}}},
	}}
	rng := rand.New(rand.NewSource(2))
	pM := Prepare(m)
	preds := []Predicate{PredIntersects, PredContains, PredWithin, PredDisjoint}
	for range 200 {
		pt := Point{X: rng.Float64() * 20, Y: rng.Float64() * 20}
		pPt := Prepare(pt)
		for _, pred := range preds {
			want := Test(pred, pt, m)
			got := TestPrepared(pred, pPt, pM)
			if got != want {
				t.Errorf("(pt, multi) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
			want = Test(pred, m, pt)
			got = TestPrepared(pred, pM, pPt)
			if got != want {
				t.Errorf("(multi, pt) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestTestPrepared_FallsThroughForUnsupported — pair shapes we
// don't have fast paths for (Point×Point, LineString×Polygon,
// Polygon×Polygon) must match Test() output. This proves the
// fall-through path is functional, not silently returning false.
func TestTestPrepared_FallsThroughForUnsupported(t *testing.T) {
	// Two identical points — should intersect.
	pa := Point{X: 3, Y: 4}
	pb := Point{X: 3, Y: 4}
	if got := TestPrepared(PredIntersects, Prepare(pa), Prepare(pb)); got != Test(PredIntersects, pa, pb) {
		t.Errorf("Point×Point Intersects: got %v, want %v", got, Test(PredIntersects, pa, pb))
	}

	// LineString × Polygon — crosses.
	ls := LineString{Points: []Point{{X: -5, Y: 5}, {X: 15, Y: 5}}}
	poly := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}}}
	if got := TestPrepared(PredIntersects, Prepare(ls), Prepare(poly)); got != Test(PredIntersects, ls, poly) {
		t.Errorf("LineString×Polygon Intersects: got %v, want %v", got, Test(PredIntersects, ls, poly))
	}

	// Polygon × Polygon — overlapping.
	polyA := Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
	}}}
	polyB := Polygon{Rings: [][]Point{{
		{X: 3, Y: 3}, {X: 8, Y: 3}, {X: 8, Y: 8}, {X: 3, Y: 8}, {X: 3, Y: 3},
	}}}
	if got := TestPrepared(PredIntersects, Prepare(polyA), Prepare(polyB)); got != Test(PredIntersects, polyA, polyB) {
		t.Errorf("Polygon×Polygon Intersects: got %v, want %v", got, Test(PredIntersects, polyA, polyB))
	}
}

// TestTestPrepared_NilGeometries — nil on either side returns
// false for every predicate. Matches Test's guard.
func TestTestPrepared_NilGeometries(t *testing.T) {
	pt := Prepare(Point{X: 1, Y: 1})
	nilP := PreparedGeometry{} // G is nil
	preds := []Predicate{PredIntersects, PredContains, PredWithin, PredTouches,
		PredCrosses, PredOverlaps, PredDisjoint}
	for _, pred := range preds {
		if got := TestPrepared(pred, nilP, pt); got {
			t.Errorf("(nil, pt) %s: got true, want false", pred)
		}
		if got := TestPrepared(pred, pt, nilP); got {
			t.Errorf("(pt, nil) %s: got true, want false", pred)
		}
	}
}

// TestPrepare_NoRingsForNonPolygon — non-polygon geometries
// should not populate the ring caches. Prevents a future refactor
// from accidentally materializing views for shapes that don't
// need them (which would negate the Slice-1 amortization argument
// for one-shot callers). Note: Prepare inherently pays one
// interface-boxing alloc when the caller passes a concrete
// Geometry — that's outside the scope of this check.
func TestPrepare_NoRingsForNonPolygon(t *testing.T) {
	cases := []Geometry{
		Point{X: 5, Y: 5},
		LineString{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		MultiPoint{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
	}
	for _, g := range cases {
		p := Prepare(g)
		if p.polyRings != nil {
			t.Errorf("%T: polyRings should be nil", g)
		}
		if p.mpSubBounds != nil {
			t.Errorf("%T: mpSubBounds should be nil", g)
		}
		if p.mpSubRings != nil {
			t.Errorf("%T: mpSubRings should be nil", g)
		}
		if p.mpTree != nil {
			t.Errorf("%T: mpTree should be nil", g)
		}
	}
}

// squarePolygonAt returns an axis-aligned square of side `side`
// centered at (cx, cy). Shared helper for the many-small-polys
// MultiPolygon fixtures below.
func squarePolygonAt(cx, cy, side float64) Polygon {
	h := side / 2
	return Polygon{Rings: [][]Point{{
		{X: cx - h, Y: cy - h},
		{X: cx + h, Y: cy - h},
		{X: cx + h, Y: cy + h},
		{X: cx - h, Y: cy + h},
		{X: cx - h, Y: cy - h},
	}}}
}

// makeIslandMP builds a MultiPolygon of `n` disjoint unit squares
// laid out on a stride so no two overlap. Approximates the
// "hundreds of Mediterranean islands" workload that revealed the
// pre-review eager-materialization regression.
func makeIslandMP(n int) MultiPolygon {
	polys := make([]Polygon, n)
	for i := range n {
		polys[i] = squarePolygonAt(float64(i)*10, 0, 1)
	}
	return MultiPolygon{Polygons: polys}
}

// TestPrepare_MultiPolygon_TreeThreshold — Prepare(MP) with N <
// mpTreeMinSubPolys builds no tree; N >= threshold builds one.
// Locks in the threshold contract so a future refactor doesn't
// silently flip small-MP callers to the tree (which is a
// regression per the linear-scan-wins-below-threshold measurement).
func TestPrepare_MultiPolygon_TreeThreshold(t *testing.T) {
	// Below threshold: linear scan path.
	mSmall := makeIslandMP(mpTreeMinSubPolys - 1)
	pSmall := Prepare(mSmall)
	if pSmall.mpTree != nil {
		t.Errorf("n=%d: expected no tree (below threshold), got one", mpTreeMinSubPolys-1)
	}
	if len(pSmall.mpSubBounds) != mpTreeMinSubPolys-1 {
		t.Errorf("mpSubBounds len: got %d, want %d", len(pSmall.mpSubBounds), mpTreeMinSubPolys-1)
	}

	// At threshold: tree path.
	mBig := makeIslandMP(mpTreeMinSubPolys)
	pBig := Prepare(mBig)
	if pBig.mpTree == nil {
		t.Errorf("n=%d: expected tree (at threshold), got nil", mpTreeMinSubPolys)
	}
	if pBig.mpTree.Len() != mpTreeMinSubPolys {
		t.Errorf("mpTree len: got %d, want %d", pBig.mpTree.Len(), mpTreeMinSubPolys)
	}
}

// TestPrepare_MultiPolygon_LazyRingViews — mpSubRings slots are
// nil until a query actually touches the sub-polygon. Populated
// atomically on first hit. Locks in the "many-small-polys shape
// pays no upfront ring-view cost" contract.
func TestPrepare_MultiPolygon_LazyRingViews(t *testing.T) {
	m := makeIslandMP(50) // well above the tree threshold
	p := Prepare(m)

	// Fresh Prepare: every slot should be nil.
	for i := range p.mpSubRings {
		if p.mpSubRings[i].Load() != nil {
			t.Fatalf("slot %d: ring views populated at Prepare time, expected lazy", i)
		}
	}

	// Query a point that hits sub-polygon 7 (centered at x=70).
	pt := Point{X: 70, Y: 0}
	if !TestPrepared(PredIntersects, Prepare(pt), p) {
		t.Fatalf("expected intersects @ (70, 0)")
	}

	// Slot 7 must now be populated; the rest should still be nil.
	// (island squares don't overlap; only slot 7's bbox contains the query.)
	populated := 0
	for i := range p.mpSubRings {
		if p.mpSubRings[i].Load() != nil {
			populated++
			if i != 7 {
				t.Errorf("slot %d populated but only slot 7 should be hit", i)
			}
		}
	}
	if populated != 1 {
		t.Errorf("populated slots after 1 hit: got %d, want 1", populated)
	}
}

// islandBoundarySeeds returns edge-exact query points for a
// makeIslandMP fixture — island i is a unit square centered at
// (i*10, 0) with bbox [i*10-0.5, -0.5, i*10+0.5, 0.5]. The seeds
// hit corners, edge midpoints, exact centers, and between-island
// gaps to force linear-vs-tree divergence if Bounds.Contains and
// RTree.SearchInto ever drifted apart on edge inclusivity (they
// agree today — both use inclusive `>=/<=` and `!(>)` reject
// respectively — but that contract deserves explicit fuzz
// coverage since random floats will never land on these exact
// coordinates).
func islandBoundarySeeds(nIslands int) []Point {
	seeds := []Point{
		// Corners of island 0 (all four).
		{X: -0.5, Y: -0.5},
		{X: 0.5, Y: -0.5},
		{X: 0.5, Y: 0.5},
		{X: -0.5, Y: 0.5},
		// Edge midpoints of island 3.
		{X: 30.5, Y: 0},
		{X: 29.5, Y: 0},
		{X: 30, Y: 0.5},
		{X: 30, Y: -0.5},
		// Exact centers.
		{X: 0, Y: 0},
		{X: 70, Y: 0},
		// Between-island gaps (all four should miss every island).
		{X: 5, Y: 0},
		{X: 15, Y: 0},
		{X: 25, Y: 0},
	}
	if nIslands >= 10 {
		// Adjacent-corner-adjacent shape: island 0 top-right corner
		// (0.5, 0.5) and island 1 top-left corner (9.5, 0.5) don't
		// share coordinates (there's a 9-unit gap), so no shared-
		// corner test is meaningful on this fixture. Still, exercise
		// the last valid island's corner.
		last := float64(nIslands-1) * 10
		seeds = append(seeds, Point{X: last + 0.5, Y: 0.5})
	}
	return seeds
}

// TestTestPrepared_MultiPolygon_ManyIslands_MatchesTest — parity
// on the landMP shape: 50 disjoint island squares, 500 random
// points + edge-exact seeds, three predicates × both orderings.
// Fails if the tree path or the lazy loader diverges from AoS
// Test.
func TestTestPrepared_MultiPolygon_ManyIslands_MatchesTest(t *testing.T) {
	m := makeIslandMP(50)
	pM := Prepare(m)
	preds := []Predicate{PredIntersects, PredContains, PredWithin}
	// Boundary-exact seeds first — deterministic, catches inclusivity
	// bugs that random floats would miss.
	for _, pt := range islandBoundarySeeds(50) {
		pPt := Prepare(pt)
		for _, pred := range preds {
			want := Test(pred, pt, m)
			got := TestPrepared(pred, pPt, pM)
			if got != want {
				t.Errorf("boundary seed (pt, MP) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
			want = Test(pred, m, pt)
			got = TestPrepared(pred, pM, pPt)
			if got != want {
				t.Errorf("boundary seed (MP, pt) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
	rng := rand.New(rand.NewSource(42))
	// Query range spans (-5, 505) so points fall inside, on-boundary,
	// and outside every island.
	for range 500 {
		pt := Point{X: rng.Float64()*510 - 5, Y: rng.Float64()*4 - 2}
		pPt := Prepare(pt)
		for _, pred := range preds {
			want := Test(pred, pt, m)
			got := TestPrepared(pred, pPt, pM)
			if got != want {
				t.Errorf("(pt, MP) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
			want = Test(pred, m, pt)
			got = TestPrepared(pred, pM, pPt)
			if got != want {
				t.Errorf("(MP, pt) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestTestPrepared_MultiPolygon_LinearPath_MatchesTest — same
// parity check but on the sub-threshold N so the linear
// bbox-reject scan is exercised (not the tree). Includes the
// same boundary-exact seeds so any inclusivity divergence
// between the linear and tree paths surfaces here.
func TestTestPrepared_MultiPolygon_LinearPath_MatchesTest(t *testing.T) {
	nIslands := mpTreeMinSubPolys - 1
	m := makeIslandMP(nIslands)
	pM := Prepare(m)
	if pM.mpTree != nil {
		t.Fatal("expected linear path (no tree) for sub-threshold N")
	}
	preds := []Predicate{PredIntersects, PredContains, PredWithin}
	// Boundary-exact seeds first.
	for _, pt := range islandBoundarySeeds(nIslands) {
		pPt := Prepare(pt)
		for _, pred := range preds {
			want := Test(pred, pt, m)
			got := TestPrepared(pred, pPt, pM)
			if got != want {
				t.Errorf("boundary seed (pt, MP-linear) %s @ (%v, %v): got %v, want %v",
					pred, pt.X, pt.Y, got, want)
			}
		}
	}
	rng := rand.New(rand.NewSource(7))
	for range 300 {
		extent := float64(nIslands) * 10
		pt := Point{X: rng.Float64()*(extent+10) - 5, Y: rng.Float64()*4 - 2}
		pPt := Prepare(pt)
		for _, pred := range preds {
			want := Test(pred, pt, m)
			got := TestPrepared(pred, pPt, pM)
			if got != want {
				t.Errorf("(pt, MP-linear) %s @ (%v, %v): got %v, want %v", pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestTestPointsPrepared_ParityVsSinglePoint — the batch API
// must return the same bool per index as calling TestPrepared
// on each (Point, prep) pair individually. Covers Polygon,
// MultiPolygon (tree path), MultiPolygon (linear path), and the
// nil / unsupported-shape fallbacks.
func TestTestPointsPrepared_ParityVsSinglePoint(t *testing.T) {
	shapes := []struct {
		name string
		g    Geometry
	}{
		{"Polygon", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}}},
		{"Polygon_with_hole", Polygon{Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 3, Y: 3}, {X: 6, Y: 3}, {X: 6, Y: 6}, {X: 3, Y: 6}, {X: 3, Y: 3}},
		}}},
		{"MP_linear", makeIslandMP(mpTreeMinSubPolys - 1)},
		{"MP_tree", makeIslandMP(50)},
		{"LineString_fallback", LineString{Points: []Point{{X: 0, Y: 0}, {X: 5, Y: 5}}}},
	}
	preds := []Predicate{PredIntersects, PredWithin, PredContains, PredDisjoint}
	rng := rand.New(rand.NewSource(9))
	// Generate a mixed point cloud that hits interiors, boundaries,
	// and the outside of every fixture.
	const n = 200
	xs := make([]float64, n)
	ys := make([]float64, n)
	for i := range xs {
		xs[i] = rng.Float64()*550 - 5
		ys[i] = rng.Float64()*15 - 3
	}
	got := make([]bool, n)
	for _, sh := range shapes {
		prep := Prepare(sh.g)
		for _, pred := range preds {
			TestPointsPrepared(pred, xs, ys, prep, got)
			for i := range n {
				want := Test(pred, Point{X: xs[i], Y: ys[i]}, sh.g)
				if got[i] != want {
					t.Errorf("%s %s [%d] @ (%v, %v): batch=%v want=%v",
						sh.name, pred, i, xs[i], ys[i], got[i], want)
				}
			}
		}
	}
}

// TestTestPointsPrepared_LenPanics — mismatched lengths panic
// (matches the compute-package convention). Callers passing an
// undersized out or misaligned xs/ys deserve a loud failure at
// the boundary rather than silent truncation.
func TestTestPointsPrepared_LenPanics(t *testing.T) {
	prep := Prepare(Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}}})
	xs := []float64{0.5, 0.5}
	ys := []float64{0.5}
	out := make([]bool, 2)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on len(xs) != len(ys)")
		}
	}()
	TestPointsPrepared(PredIntersects, xs, ys, prep, out)
}

func TestTestPointsPrepared_OutTooShortPanics(t *testing.T) {
	prep := Prepare(Polygon{Rings: [][]Point{{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}}})
	xs := []float64{0.5, 0.5}
	ys := []float64{0.5, 0.5}
	out := make([]bool, 1) // too short
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on len(out) < len(xs)")
		}
	}()
	TestPointsPrepared(PredIntersects, xs, ys, prep, out)
}

// TestTestPointsPrepared_NilPrepReturnsFalse — nil-G prep matches
// Test(nil, _) behavior: every result is false. Verifies the
// early guard.
func TestTestPointsPrepared_NilPrepReturnsFalse(t *testing.T) {
	xs := []float64{0, 1, 2}
	ys := []float64{0, 1, 2}
	out := make([]bool, 3)
	for i := range out {
		out[i] = true // seed with true so we can detect any missed writes
	}
	TestPointsPrepared(PredIntersects, xs, ys, PreparedGeometry{}, out)
	for i, v := range out {
		if v {
			t.Errorf("out[%d] = true, want false (nil prep)", i)
		}
	}
}

// BenchmarkTestPointsPrepared_MP_LandShape mirrors
// BenchmarkPrepare_MP_LandShape but uses the batch API. Should
// show fewer per-query allocs (no pool Get/Put per point, no
// interface-boxing per Prepare(pt)) and lower wall time on
// tree-eligible MPs.
func BenchmarkTestPointsPrepared_MP_LandShape(b *testing.B) {
	m := makeIslandMP(200)
	rng := rand.New(rand.NewSource(1))
	const npoints = 1000
	xs := make([]float64, npoints)
	ys := make([]float64, npoints)
	for i := range xs {
		if i%2 == 0 {
			idx := rng.Intn(200)
			xs[i], ys[i] = float64(idx)*10, 0
		} else {
			xs[i], ys[i] = rng.Float64()*2000, 5+rng.Float64()
		}
	}
	out := make([]bool, npoints)

	b.ReportAllocs()
	for b.Loop() {
		pM := Prepare(m)
		TestPointsPrepared(PredIntersects, xs, ys, pM, out)
	}
}

// BenchmarkPrepare_MP_LandShape measures Prepare + N-point-query
// cost on the many-small-polys shape (200 disjoint islands). The
// pre-review implementation eagerly materialized every ring at
// Prepare time — this bench should show constant-time Prepare
// dominated by bbox+tree setup, with per-query cost proportional
// to hits (typically 1) rather than N.
func BenchmarkPrepare_MP_LandShape(b *testing.B) {
	m := makeIslandMP(200)
	rng := rand.New(rand.NewSource(1))
	points := make([]Point, 1000)
	for i := range points {
		// Half hit an island, half miss (query outside the island band).
		if i%2 == 0 {
			// Hit: pick an island center.
			idx := rng.Intn(200)
			points[i] = Point{X: float64(idx) * 10, Y: 0}
		} else {
			// Miss: y outside the ±0.5 band around y=0.
			points[i] = Point{X: rng.Float64() * 2000, Y: 5 + rng.Float64()}
		}
	}
	pPts := make([]PreparedGeometry, len(points))
	for i, pt := range points {
		pPts[i] = Prepare(pt)
	}

	b.ReportAllocs()
	for b.Loop() {
		pM := Prepare(m)
		for _, pp := range pPts {
			_ = TestPrepared(PredIntersects, pp, pM)
		}
	}
}

// TestTestPrepared_MP_ConcurrentPublish_NoRace — locks in the
// "atomic publish is safe under concurrent readers" contract on
// the lazy mpSubRings slots. Four goroutines query a shared
// prepared MP at four distinct points, each targeting a distinct
// sub-polygon; after they join, every hit slot must be populated
// exactly once (no torn slice header, no nil-load-and-drop, no
// duplicate publish surviving as the winning slot).
//
// The race detector is the primary assertion — a data race on
// the atomic.Pointer would flag here. The post-join population
// check is a secondary contract test that catches slot-mixup
// bugs (e.g., a slot getting a slice header from a DIFFERENT
// sub-polygon due to a swap-store bug) which race won't catch.
func TestTestPrepared_MP_ConcurrentPublish_NoRace(t *testing.T) {
	m := makeIslandMP(50)
	pM := Prepare(m)
	// Pre-flight: every slot starts unpopulated. Guards against a
	// future refactor that accidentally eager-materializes.
	for i := range pM.mpSubRings {
		if pM.mpSubRings[i].Load() != nil {
			t.Fatalf("slot %d already populated pre-query", i)
		}
	}

	// Four target island indexes, each in a distinct sub-poly so
	// no two goroutines contend on the same atomic slot. Confirms
	// the "concurrent readers on DIFFERENT slots" fast path.
	targets := []int{3, 17, 29, 42}
	var wg sync.WaitGroup
	for _, idx := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pt := Point{X: float64(i) * 10, Y: 0}
			if !TestPrepared(PredIntersects, Prepare(pt), pM) {
				t.Errorf("island %d: expected hit @ (%v, 0)", i, float64(i)*10)
			}
		}(idx)
	}
	wg.Wait()

	// Every target slot must be populated; no other slot should be
	// (points targeted distinct islands, all disjoint).
	for i := range pM.mpSubRings {
		want := false
		for _, tgt := range targets {
			if tgt == i {
				want = true
				break
			}
		}
		got := pM.mpSubRings[i].Load() != nil
		if got != want {
			t.Errorf("slot %d: populated=%v, want=%v", i, got, want)
		}
	}
}

// TestTestPrepared_MP_ConcurrentSameSlot_NoRace — the harder
// race case: many goroutines target the SAME sub-polygon at once
// (each queries a distinct point INSIDE the same island). Every
// goroutine's first load hits the nil slot, races to materialize,
// and Stores; the atomic publish contract says exactly one Store
// wins observably and every reader sees a valid slice header.
// If the atomic were replaced with a plain write, the race
// detector would flag here.
func TestTestPrepared_MP_ConcurrentSameSlot_NoRace(t *testing.T) {
	m := makeIslandMP(50)
	pM := Prepare(m)
	const nGoroutines = 8
	const targetIdx = 12
	var wg sync.WaitGroup
	// Random-ish offsets inside island 12's ±0.5 unit square.
	offsets := []float64{-0.4, -0.3, -0.2, -0.1, 0.1, 0.2, 0.3, 0.4}
	for i := range nGoroutines {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			pt := Point{X: float64(targetIdx)*10 + offsets[k], Y: 0}
			if !TestPrepared(PredIntersects, Prepare(pt), pM) {
				t.Errorf("goroutine %d: expected hit @ (%v, 0)", k, pt.X)
			}
		}(i)
	}
	wg.Wait()

	// Slot must be populated exactly once (whichever goroutine's
	// Store won). The other slots should be untouched.
	populated := 0
	for i := range pM.mpSubRings {
		if pM.mpSubRings[i].Load() != nil {
			populated++
			if i != targetIdx {
				t.Errorf("unexpected populated slot %d (target was %d)", i, targetIdx)
			}
		}
	}
	if populated != 1 {
		t.Errorf("populated slots: got %d, want 1", populated)
	}
}

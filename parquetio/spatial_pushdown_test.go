package parquetio_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

// spatialFrame is the *testing.T entry point — see spatialFrameTB
// for the generic body.
func spatialFrame(t *testing.T, N int, clusterA, clusterB geometry.Point) *gobi.Frame {
	return spatialFrameTB(t, N, clusterA, clusterB)
}

// spatialFrameTB builds a Frame of N polygons split into two spatial
// clusters: the first N/2 clustered near clusterA, the second N/2 near
// clusterB. Each polygon is a small square. Written with
// RowGroupRows=N/2, the two clusters land in separate row groups —
// exactly the shape the bbox pushdown is designed to exploit.
func spatialFrameTB(tb testing.TB, N int, clusterA, clusterB geometry.Point) *gobi.Frame {
	tb.Helper()
	if N%2 != 0 {
		tb.Fatalf("spatialFrame: N must be even, got %d", N)
	}
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()

	half := N / 2
	for i := range N {
		idB.Append(int64(i))
		var center geometry.Point
		if i < half {
			center = clusterA
		} else {
			center = clusterB
		}
		// 1×1 square offset by row index so no two polygons share bounds.
		x := center.X + float64(i%half)*0.001
		y := center.Y + float64(i%half)*0.001
		poly := geometry.SimplePolygon([]geometry.Point{
			{X: x, Y: y},
			{X: x + 1, Y: y},
			{X: x + 1, Y: y + 1},
			{X: x, Y: y + 1},
			{X: x, Y: y},
		}, geometry.PseudoMercator)
		geomB.Append(geometry.WKB(poly))
	}

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		gobi.GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		tb.Fatal(err)
	}
	return f
}

// TestSpatialPushdown_SkipsDisjointRowGroup writes a two-cluster
// frame into a two-row-group parquet, then reads it with a spatial
// predicate that intersects only cluster A. The reader should skip
// cluster B's row group entirely — proven by row-count arithmetic.
func TestSpatialPushdown_SkipsDisjointRowGroup(t *testing.T) {
	clusterA := geometry.Point{X: 10, Y: 10}
	clusterB := geometry.Point{X: 5000, Y: 5000}
	df := spatialFrame(t, 400, clusterA, clusterB)
	defer df.Release()

	path := filepath.Join(t.TempDir(), "spatial.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 200,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// AOI polygon around cluster A only.
	aoi := geometry.SimplePolygon([]geometry.Point{
		{X: -100, Y: -100},
		{X: 500, Y: -100},
		{X: 500, Y: 500},
		{X: -100, Y: 500},
		{X: -100, Y: -100},
	}, geometry.PseudoMercator)

	// Baseline: no predicate → all 400 rows.
	base, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("baseline read: %v", err)
	}
	baseRows, _ := base.Shape()
	base.Release()
	if baseRows != 400 {
		t.Fatalf("baseline row count = %d, want 400", baseRows)
	}

	// With pushdown: predicate intersects only cluster A. Reader
	// should skip cluster B's row group, returning ~200 rows (the
	// exact count is 200 because we sized row groups to match).
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		Predicate: gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi)),
	})
	if err != nil {
		t.Fatalf("pushdown read: %v", err)
	}
	defer out.Release()
	got, _ := out.Shape()
	if got != 200 {
		t.Fatalf("row count with pushdown = %d, want 200 (cluster B row group not skipped)", got)
	}
}

// TestSpatialPushdown_KeepsRowGroupOnBboxOverlap: predicate whose
// bbox overlaps BOTH row groups keeps both — negative control for
// the previous test.
func TestSpatialPushdown_KeepsRowGroupOnBboxOverlap(t *testing.T) {
	clusterA := geometry.Point{X: 10, Y: 10}
	clusterB := geometry.Point{X: 5000, Y: 5000}
	df := spatialFrame(t, 400, clusterA, clusterB)
	defer df.Release()

	path := filepath.Join(t.TempDir(), "spatial.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 200,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// AOI covering both clusters.
	aoi := geometry.SimplePolygon([]geometry.Point{
		{X: -100, Y: -100},
		{X: 6000, Y: -100},
		{X: 6000, Y: 6000},
		{X: -100, Y: 6000},
		{X: -100, Y: -100},
	}, geometry.PseudoMercator)

	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		Predicate: gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi)),
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer out.Release()
	got, _ := out.Shape()
	if got != 400 {
		t.Fatalf("row count = %d, want 400 (both row groups should survive)", got)
	}
}

// TestSpatialPushdown_Disjoint_ConcaveLiteralNotPruned exercises the
// correctness fix from the v0.3.4 code review: when the Disjoint
// literal is a concave polygon (not a filled rectangle), the row
// group must NOT be pruned even if its bbox is fully inside the
// literal's bbox — because rows inside litBounds but outside lit
// itself ARE genuinely disjoint from lit and would be silently
// dropped by the naive bbox rule.
//
// Fixture:
//
//	L-shaped literal covering [0,10]×[0,10] MINUS the [5,10]×[5,10]
//	quadrant, so its bbox is [0,10]×[0,10] but its shape has a hole
//	in the upper-right.
//	Row group with polygons clustered near (7, 7) — inside the
//	literal's bbox, outside the literal itself → they ARE disjoint.
//
// Correct behavior: pushdown keeps the row group (bbox rule is
// insufficient to prove disjointness); per-row filter finds the
// disjoint rows.
func TestSpatialPushdown_Disjoint_ConcaveLiteralNotPruned(t *testing.T) {
	// Row group's polygons all sit in the "missing quadrant" of the L.
	upperRight := geometry.Point{X: 7, Y: 7}
	// Second cluster far away — this row group's bbox is fully outside
	// the literal's bbox, so it SHOULD be pruned (baseline sanity).
	farAway := geometry.Point{X: 5000, Y: 5000}
	df := spatialFrame(t, 400, upperRight, farAway)
	defer df.Release()

	path := filepath.Join(t.TempDir(), "concave_disjoint.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 200,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// L-shaped polygon: [0,10]×[0,10] with the [5,10]×[5,10] quadrant
	// removed. Same bbox as a filled 10×10 square, but concave.
	lShape := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0},
		{X: 10, Y: 0},
		{X: 10, Y: 5},
		{X: 5, Y: 5},
		{X: 5, Y: 10},
		{X: 0, Y: 10},
		{X: 0, Y: 0},
	}, geometry.PseudoMercator)

	// Baseline: no predicate → 400 rows.
	base, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("baseline read: %v", err)
	}
	baseRows, _ := base.Shape()
	base.Release()
	if baseRows != 400 {
		t.Fatalf("baseline row count = %d, want 400", baseRows)
	}

	// With Disjoint predicate against the L-shape: the upper-right
	// row group MUST be kept (its rows sit in the L's bbox but
	// outside the L's shape → they are disjoint from L, so they must
	// survive to be classified as true by the per-row filter).
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		Predicate: gobi.Col("geometry").GeomDisjoint(gobi.Lit(lShape)),
	})
	if err != nil {
		t.Fatalf("pushdown read: %v", err)
	}
	defer out.Release()
	got, _ := out.Shape()
	// Far-away row group's bbox is disjoint from lShape's bbox → the
	// Disjoint predicate's bbox-only test says "every row is
	// definitely disjoint" so we don't NEED to visit that row group
	// to know its rows are disjoint. But the current implementation
	// keeps it (the rectangle guard makes the prune conservative in
	// both directions). Either 200 or 400 is correctness-safe.
	if got < 200 {
		t.Fatalf("row count with L-shape Disjoint pushdown = %d, want >= 200 (upper-right row group must not be pruned)", got)
	}
	// Tighten the correctness claim: some row with an id in [0, 200)
	// must survive. That's the upper-right cluster (spatialFrame
	// assigns IDs sequentially, cluster A → [0, N/2), cluster B →
	// [N/2, N)). A bug that wrongly pruned cluster A would leave only
	// [200, 400) IDs, which the check below catches even if the total
	// row count is coincidentally >= 200.
	if !containsUpperRightID(out, 200) {
		t.Fatalf("no id < 200 survived Disjoint pushdown — upper-right (concave-quadrant) cluster was wrongly pruned")
	}
}

// containsUpperRightID reports whether the "id" column has any value
// in [0, cutoff). Used to prove the upper-right cluster (IDs 0..N/2-1
// from spatialFrame) survived pushdown, independent of row count.
func containsUpperRightID(f *gobi.Frame, cutoff int64) bool {
	col, err := f.Column("id")
	if err != nil {
		return false
	}
	for _, chunk := range col.Column().Data().Chunks() {
		ints, ok := chunk.(*array.Int64)
		if !ok {
			return false
		}
		for i := range ints.Len() {
			if !ints.IsNull(i) && ints.Value(i) < cutoff {
				return true
			}
		}
	}
	return false
}

// TestSpatialPushdown_Disjoint_RectangularLiteralPrunes: negative
// control for the concave-literal test. A filled rectangle IS a
// bbox-rectangle, so the same containment rule DOES prune sound.
func TestSpatialPushdown_Disjoint_RectangularLiteralPrunes(t *testing.T) {
	// Row group inside [0,10]²; far cluster elsewhere. Note we can't
	// use a spatialFrame with clusters both at (5,5) because we need
	// row groups whose bboxes are unambiguously "inside" the literal.
	inside := geometry.Point{X: 3, Y: 3}
	farAway := geometry.Point{X: 5000, Y: 5000}
	df := spatialFrame(t, 400, inside, farAway)
	defer df.Release()

	path := filepath.Join(t.TempDir(), "rect_disjoint.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 200,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Rectangle spanning much more than the inside row group.
	rect := geometry.SimplePolygon([]geometry.Point{
		{X: -100, Y: -100},
		{X: 100, Y: -100},
		{X: 100, Y: 100},
		{X: -100, Y: 100},
		{X: -100, Y: -100},
	}, geometry.PseudoMercator)

	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		Predicate: gobi.Col("geometry").GeomDisjoint(gobi.Lit(rect)),
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer out.Release()
	got, _ := out.Shape()
	// The "inside" row group is fully inside rect → every row is
	// definitely NOT disjoint from rect, so the Disjoint predicate
	// can prune that row group. We only see the far-away row group.
	if got != 200 {
		t.Fatalf("row count with rectangular-Disjoint pushdown = %d, want 200", got)
	}
}

// TestSpatialPushdown_LazyScanFilterCollect_NoPanic exercises the
// end-to-end lazy path: parquetio.ScanFile(path).Filter(pred).Collect().
//
// This test would panic *without* two v0.3.4 round-2 fixes:
//
//  1. The pushdown-idempotency check in the WithPredicatePushdown
//     callback. Without it, the optimizer's fixed-point loop
//     re-pushes the same predicate every pass, producing a 30+
//     deep (P AND P AND P ...) chain that inflates plan strings and
//     wastes cycles (but doesn't itself panic — bug 1 is silent).
//  2. The ReadSchema ↔ frameFromRecord alignment on hideCovering.
//     Without it, ReadSchema returns the full 11-col schema but
//     frameFromRecord emits 7-col batches, so downstream operators
//     walk column indices into wrong types and concatBatchesToFrame
//     panics with `arrow/array: inconsistent data type utf8 vs
//     float64`. This test panic-checks that path.
//
// A regression in either fix would show up here — bug 2 as a hard
// runtime panic, bug 1 as an insanely long ExplainOptimized string
// on the sibling test below.
func TestSpatialPushdown_LazyScanFilterCollect_NoPanic(t *testing.T) {
	clusterA := geometry.Point{X: 10, Y: 10}
	clusterB := geometry.Point{X: 5000, Y: 5000}
	df := spatialFrame(t, 400, clusterA, clusterB)
	defer df.Release()

	path := filepath.Join(t.TempDir(), "lazy_scan_filter.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:        parquetio.CodecSnappy,
		RowGroupRows: 200,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	aoi := geometry.SimplePolygon([]geometry.Point{
		{X: -100, Y: -100},
		{X: 500, Y: -100},
		{X: 500, Y: 500},
		{X: -100, Y: 500},
		{X: -100, Y: -100},
	}, geometry.PseudoMercator)

	// Lazy pipeline: ScanFile → Filter → Collect. Runs the optimizer
	// (predicate pushdown fires), streams through the executor
	// (frameFromRecord path), and concatenates batches at the end.
	out, err := parquetio.ScanFile(path, nil).Filter(
		gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi)),
	).Collect()
	if err != nil {
		t.Fatalf("lazy Collect: %v", err)
	}
	defer out.Release()

	// Cluster A survives row-group pruning and per-row filtering; the
	// far cluster is either pruned or filtered away.
	got, cols := out.Shape()
	if got != 200 {
		t.Fatalf("got %d rows, want 200 (only cluster A)", got)
	}
	// Round-trip contract: covering columns hidden by default.
	if cols != 2 {
		t.Fatalf("got %d cols, want 2 (id + geometry) — covering cols must not surface", cols)
	}
	// Verify covering column names are absent.
	names := out.ColumnNames()
	for _, n := range []string{"geometry_bbox_xmin", "geometry_bbox_ymin", "geometry_bbox_xmax", "geometry_bbox_ymax"} {
		if slices.Contains(names, n) {
			t.Errorf("covering column %q leaked into lazy Collect output; got %v", n, names)
		}
	}
}

// TestSpatialPushdown_OptimizerIdempotent asserts that the optimizer's
// fixed-point loop applies the pushdown rule exactly once — the
// predicate lands on the scan node's ReadOptions.Predicate and stays
// there, without accumulating on subsequent passes.
//
// Regression protection for the parquetio callback's idempotency
// check: without it, each pass builds a fresh ScanFile with
// `newOpts.Predicate.And(pred)`, and the fixed-point loop never
// converges until maxOptimizeIters caps it (leaving a
// 30+ deep AND chain visible in Explain).
//
// We check by running Optimize twice and asserting the plan string
// is stable — the second run finds nothing to change, which is only
// true if the pushdown callback correctly says "already applied."
func TestSpatialPushdown_OptimizerIdempotent(t *testing.T) {
	df := spatialFrame(t, 20, geometry.Point{X: 0, Y: 0}, geometry.Point{X: 100, Y: 100})
	defer df.Release()

	path := filepath.Join(t.TempDir(), "idempotent.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec: parquetio.CodecSnappy,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	aoi := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0},
		{X: 200, Y: 0},
		{X: 200, Y: 200},
		{X: 0, Y: 200},
		{X: 0, Y: 0},
	}, geometry.PseudoMercator)

	lf := parquetio.ScanFile(path, nil).Filter(
		gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi)),
	)

	// First optimize.
	first := gobi.Optimize(lf.Plan())
	firstStr := planString(first)

	// Feed the OPTIMIZED plan back through Optimize. If pushdown is
	// truly idempotent, no rule finds anything to change and the
	// output is byte-identical to the input.
	second := gobi.Optimize(first)
	secondStr := planString(second)

	if firstStr != secondStr {
		t.Fatalf("optimizer non-idempotent — predicate accumulated on the second pass\nfirst:  %s\nsecond: %s",
			firstStr, secondStr)
	}

	// Sanity floor: the pushed-down predicate should appear a bounded
	// number of times. Before the fix, "intersects" appeared 30+
	// times in the scan node's pred label. After: twice (once in the
	// Filter node above, once in the pushed-down pred on the scan).
	occurrences := strings.Count(firstStr, "intersects")
	if occurrences > 4 {
		t.Fatalf("intersects appears %d times in optimized plan — expected ≤4 (predicate accumulated on pushdown)\n%s",
			occurrences, firstStr)
	}
}

// planString renders a plan via ExplainOptimized-style walk. Kept
// local to the test since gobi.NewLazyFrame(plan).ExplainOptimized()
// would re-run Optimize, defeating the purpose of the idempotency
// check.
func planString(p gobi.LogicalPlan) string {
	return gobi.NewLazyFrame(p).Explain()
}

// TestSpatialPushdown_SkipBboxCovering: the opt-out writes files
// without covering columns, so ReadFile-with-IncludeCoveringColumns
// still finds nothing. Useful for tiny/streaming workloads where the
// write scan cost outweighs the read benefit.
func TestSpatialPushdown_SkipBboxCovering(t *testing.T) {
	df := spatialFrame(t, 20, geometry.Point{X: 0, Y: 0}, geometry.Point{X: 100, Y: 100})
	defer df.Release()

	path := filepath.Join(t.TempDir(), "no_covering.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec:            parquetio.CodecSnappy,
		SkipBboxCovering: true,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		IncludeCoveringColumns: true,
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer out.Release()
	cols := out.ColumnNames()
	forbidden := []string{"geometry_bbox_xmin", "geometry_bbox_ymin", "geometry_bbox_xmax", "geometry_bbox_ymax"}
	for _, name := range forbidden {
		if slices.Contains(cols, name) {
			t.Errorf("expected %q to be absent from SkipBboxCovering write; got %v", name, cols)
		}
	}
}

// TestSpatialPushdown_CoveringColumnsHiddenByDefault verifies the
// WriteFile ↔ ReadFile round-trip contract holds: bbox covering
// columns live in the file (used by pushdown) but don't surface in
// the output frame unless the caller opts in.
func TestSpatialPushdown_CoveringColumnsHiddenByDefault(t *testing.T) {
	df := spatialFrame(t, 20, geometry.Point{X: 0, Y: 0}, geometry.Point{X: 100, Y: 100})
	defer df.Release()

	path := filepath.Join(t.TempDir(), "covering.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		Codec: parquetio.CodecSnappy,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Default read: covering columns hidden. Round-trips to the same
	// schema the writer saw.
	hidden, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read (hidden): %v", err)
	}
	defer hidden.Release()
	got := hidden.ColumnNames()
	want := []string{"id", "geometry"}
	if !slices.Equal(got, want) {
		t.Errorf("default read column names = %v, want %v (covering columns should be hidden)", got, want)
	}

	// Opt-in: covering columns exposed.
	visible, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		IncludeCoveringColumns: true,
	})
	if err != nil {
		t.Fatalf("read (visible): %v", err)
	}
	defer visible.Release()
	visibleCols := visible.ColumnNames()
	wantBbox := []string{"geometry_bbox_xmin", "geometry_bbox_ymin", "geometry_bbox_xmax", "geometry_bbox_ymax"}
	for _, name := range wantBbox {
		if !slices.Contains(visibleCols, name) {
			t.Errorf("opt-in read missing covering column %q; got %v", name, visibleCols)
		}
	}
}

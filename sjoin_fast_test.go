package gobi

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// buildIndexedPolygonGrid returns a gridSize x gridSize grid of unit
// polygons with a unique per-row name so parity tests can pair
// left+right rows by (name, name_right).
func buildIndexedPolygonGrid(t testing.TB, gridSize int, prefix string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for y := range gridSize {
		for x := range gridSize {
			nameB.Append(fmt.Sprintf("%s-%d-%d", prefix, x, y))
			poly := geometry.SimplePolygon([]geometry.Point{
				{X: float64(x), Y: float64(y)},
				{X: float64(x + 1), Y: float64(y)},
				{X: float64(x + 1), Y: float64(y + 1)},
				{X: float64(x), Y: float64(y + 1)},
				{X: float64(x), Y: float64(y)},
			}, geometry.WGS84)
			geomB.Append(geometry.WKB(poly))
		}
	}
	return newIndexedFrame(t, nameB, geomB)
}

// buildIndexedSmallPolygonCloud — n small triangles with unique names.
func buildIndexedSmallPolygonCloud(t testing.TB, n int, extent float64, prefix string, seed1, seed2 uint64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(seed1, seed2))
	for i := range n {
		nameB.Append(fmt.Sprintf("%s-%d", prefix, i))
		cx := rng.Float64() * extent
		cy := rng.Float64() * extent
		pts := make([]geometry.Point, 4)
		for j := range 3 {
			theta := 2 * math.Pi * float64(j) / 3
			pts[j] = geometry.Point{
				X: cx + 0.3*math.Cos(theta),
				Y: cy + 0.3*math.Sin(theta),
			}
		}
		pts[3] = pts[0]
		poly := geometry.SimplePolygon(pts, geometry.WGS84)
		geomB.Append(geometry.WKB(poly))
	}
	return newIndexedFrame(t, nameB, geomB)
}

// buildIndexedLineCloud — n 2-vertex LineStrings with unique names.
func buildIndexedLineCloud(t testing.TB, n int, extent float64, prefix string, seed1, seed2 uint64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(seed1, seed2))
	for i := range n {
		nameB.Append(fmt.Sprintf("%s-%d", prefix, i))
		x := rng.Float64() * (extent - 0.5)
		y := rng.Float64() * (extent - 0.5)
		theta := rng.Float64() * 2 * math.Pi
		ls := geometry.LineString{
			Points: []geometry.Point{
				{X: x, Y: y},
				{X: x + 0.5*math.Cos(theta), Y: y + 0.5*math.Sin(theta)},
			},
			CRSValue: geometry.WGS84,
		}
		geomB.Append(geometry.WKB(ls))
	}
	return newIndexedFrame(t, nameB, geomB)
}

func newIndexedFrame(t testing.TB, nameB *array.StringBuilder, geomB *array.BinaryBuilder) *Frame {
	t.Helper()
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestSJoin_PolysInPolygons_ParallelPath_NoRace — exercises the
// concurrent worker path with the race detector. Uses >
// SJoinMinParallelRows so `sjoinScan` actually spawns workers;
// the pre-review "shared []Geometry cache" pattern would have
// been caught here (torn 2-word interface writes → race
// detector flags → test fails). The current sync.Once-per-slot
// cache passes clean.
func TestSJoin_PolysInPolygons_ParallelPath_NoRace(t *testing.T) {
	n := SJoinMinParallelRows + 500
	right := buildBenchPolygonGrid(t, 5) // 25 grid cells
	left := buildBenchSmallPolygonCloud(t, n, 5)
	got, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatalf("SJoin: %v", err)
	}
	// Sanity: matches produced. Real assertion is that -race
	// finds no data race across the concurrent workers hitting
	// the rightGeoms cache.
	if got.NumRows() == 0 {
		t.Fatal("expected some matches on grid × cloud shape")
	}
}

// TestSJoin_LinesInPolygons_ParityWithAoS — Slice 20c LineString×
// Polygon fast path must produce the same (leftIdx, rightIdx)
// pairs as the AoS refine, including for the "edge-crossing
// without vertex-inside" fallback case.
func TestSJoin_LinesInPolygons_ParityWithAoS(t *testing.T) {
	right := buildIndexedPolygonGrid(t, 5, "R")
	left := buildIndexedLineCloud(t, 200, 5, "L", 0xF0F0, 0x0F0F)
	compareSJoinAgainstAoS(t, left, right, "Lines")
}

// TestSJoin_PolysInPolygons_ParityWithAoS — Slice 20d Polygon×
// Polygon fast path parity.
func TestSJoin_PolysInPolygons_ParityWithAoS(t *testing.T) {
	right := buildIndexedPolygonGrid(t, 5, "R")
	left := buildIndexedSmallPolygonCloud(t, 200, 5, "L", 0xC0DE, 0xBEEF)
	compareSJoinAgainstAoS(t, left, right, "Polys")
}

// TestSJoin_PolysInPolygons_SPWithin_ParityWithAoS — Slice 21c
// SPWithin fast path parity. Convex right polygons are the fast-
// path case; the AoS oracle handles them the same.
func TestSJoin_PolysInPolygons_SPWithin_ParityWithAoS(t *testing.T) {
	right := buildIndexedPolygonGrid(t, 5, "R")
	left := buildIndexedSmallPolygonCloud(t, 200, 5, "L", 0xC0DE, 0xBEEF)
	compareSJoinAgainstAoSPred(t, left, right, SPWithin, "PolysWithin")
}

// TestSJoin_PolysInPolygons_SPContains_ParityWithAoS — Slice 21d.
func TestSJoin_PolysInPolygons_SPContains_ParityWithAoS(t *testing.T) {
	right := buildIndexedSmallPolygonCloud(t, 200, 5, "R", 0xC0DE, 0xBEEF)
	left := buildIndexedPolygonGrid(t, 5, "L")
	compareSJoinAgainstAoSPred(t, left, right, SPContains, "PolysContains")
}

// TestSJoin_LinesInPolygons_SPWithin_ParityWithAoS — Slice 21e
// LineString × convex Polygon SPWithin parity.
func TestSJoin_LinesInPolygons_SPWithin_ParityWithAoS(t *testing.T) {
	right := buildIndexedPolygonGrid(t, 5, "R")
	left := buildIndexedLineCloud(t, 200, 5, "L", 0xF0F0, 0x0F0F)
	compareSJoinAgainstAoSPred(t, left, right, SPWithin, "LinesWithin")
}

// compareSJoinAgainstAoSPred is the SPIntersects-agnostic version
// of compareSJoinAgainstAoS. Takes the predicate explicitly so
// SPWithin/SPContains parity tests can reuse the harness.
func compareSJoinAgainstAoSPred(t *testing.T, left, right *Frame, pred SpatialPredicate, label string) {
	t.Helper()
	got, err := left.SJoin(right, "geometry", "geometry", pred)
	if err != nil {
		t.Fatalf("%s SoA SJoin: %v", label, err)
	}
	want, err := legacySJoinAoS(left, right, "geometry", "geometry", pred)
	if err != nil {
		t.Fatalf("%s AoS SJoin: %v", label, err)
	}
	comparePairMultisets(t, got, want, label)
}

func compareSJoinAgainstAoS(t *testing.T, left, right *Frame, label string) {
	t.Helper()
	got, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatalf("%s SoA SJoin: %v", label, err)
	}
	want, err := legacySJoinAoS(left, right, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatalf("%s AoS SJoin: %v", label, err)
	}
	comparePairMultisets(t, got, want, label)
}

// comparePairMultisets compares SJoin output frames by (name,
// name_right) tuples — the identity fingerprint for each matched
// pair. Both input frames must carry a per-row unique `name` column;
// SJoin renames the right frame's column to `name_right` in the
// output. Comparison is order-insensitive (multisets), catching
// missing pairs, extra pairs, and mispairings that the previous
// row-count-only check missed.
func comparePairMultisets(t *testing.T, got, want *Frame, label string) {
	t.Helper()
	gotPairs := extractSJoinPairs(t, got, label+"/SoA")
	wantPairs := extractSJoinPairs(t, want, label+"/AoS")
	sort.Slice(gotPairs, func(i, j int) bool { return pairStrLess(gotPairs[i], gotPairs[j]) })
	sort.Slice(wantPairs, func(i, j int) bool { return pairStrLess(wantPairs[i], wantPairs[j]) })
	if len(gotPairs) != len(wantPairs) {
		t.Fatalf("%s pair count mismatch: SoA=%d AoS=%d\ngot=%v\nwant=%v",
			label, len(gotPairs), len(wantPairs), gotPairs, wantPairs)
	}
	for i := range gotPairs {
		if gotPairs[i] != wantPairs[i] {
			t.Errorf("%s pair %d: SoA=%v AoS=%v", label, i, gotPairs[i], wantPairs[i])
		}
	}
}

// extractSJoinPairs pulls (name, name_right) string tuples per
// output row. Real pair identity — each grid cell / triangle /
// line has a distinct `name`, so two frames producing the same
// multiset of tuples produced the same pairing regardless of
// row order.
func extractSJoinPairs(t *testing.T, f *Frame, label string) [][2]string {
	t.Helper()
	rows := f.NumRows()
	nameCol, err := f.Column("name")
	if err != nil {
		t.Fatalf("%s: missing name column: %v", label, err)
	}
	rightNameCol, err := f.Column("name_right")
	if err != nil {
		t.Fatalf("%s: missing name_right column: %v", label, err)
	}
	leftNames := stringColValues(t, nameCol, label+"/name")
	rightNames := stringColValues(t, rightNameCol, label+"/name_right")
	if len(leftNames) != rows || len(rightNames) != rows {
		t.Fatalf("%s: column length %d/%d != NumRows %d",
			label, len(leftNames), len(rightNames), rows)
	}
	pairs := make([][2]string, rows)
	for i := range rows {
		pairs[i] = [2]string{leftNames[i], rightNames[i]}
	}
	return pairs
}

func stringColValues(t *testing.T, s Series, label string) []string {
	t.Helper()
	var out []string
	for _, chunk := range s.Column().Data().Chunks() {
		arr, ok := chunk.(*array.String)
		if !ok {
			t.Fatalf("%s: chunk not *array.String (%T)", label, chunk)
		}
		for i := range arr.Len() {
			out = append(out, arr.Value(i))
		}
	}
	return out
}

func pairStrLess(a, b [2]string) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

var _ = geometry.WKB // keep import stable

// buildIndexedPointCloud — n uniformly random points in [0, extent)²
// with unique names. Shared helper for the K=1 SJoin fast path
// where the left side is a large point cloud and the right side
// is a single (multi)polygon.
func buildIndexedPointCloud(t testing.TB, n int, extent float64, prefix string, seed1, seed2 uint64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(seed1, seed2))
	for i := range n {
		nameB.Append(fmt.Sprintf("%s-%d", prefix, i))
		pt := geometry.Point{
			X:        rng.Float64() * extent,
			Y:        rng.Float64() * extent,
			CRSValue: geometry.WGS84,
		}
		geomB.Append(geometry.WKB(pt))
	}
	return newIndexedFrame(t, nameB, geomB)
}

// buildSinglePolygonFrame — right side with EXACTLY ONE Polygon
// row (not MultiPolygon). Exercises the testPointsVsPolygonBatch
// path inside the K=1 SJoin fast path, which the MultiPolygon
// fixtures don't hit (they route through
// testPointsVsMultiPolygonBatch → mpQueryPointCore).
func buildSinglePolygonFrame(t testing.TB, extent float64, prefix string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	// One big square centered at (extent/2, extent/2) covering the
	// middle 60% of the extent. Guarantees a mix of hits, boundary
	// contacts, and misses when queried against a uniform point
	// cloud over [0, extent).
	c := extent / 2
	h := extent * 0.3
	poly := geometry.Polygon{Rings: [][]geometry.Point{{
		{X: c - h, Y: c - h},
		{X: c + h, Y: c - h},
		{X: c + h, Y: c + h},
		{X: c - h, Y: c + h},
		{X: c - h, Y: c - h},
	}}, CRSValue: geometry.WGS84}
	nameB.Append(prefix + "-poly")
	geomB.Append(geometry.WKB(poly))
	return newIndexedFrame(t, nameB, geomB)
}

// buildSingleMultiPolygonFrame — right side with EXACTLY ONE
// MultiPolygon row, laid out as `nSubPolys` unit squares striding
// across the extent. Approximates the landMP shape (Mediterranean
// islands as a single feature).
func buildSingleMultiPolygonFrame(t testing.TB, nSubPolys int, extent float64, prefix string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	polys := make([]geometry.Polygon, nSubPolys)
	stride := extent / float64(nSubPolys)
	for i := range nSubPolys {
		cx := (float64(i) + 0.5) * stride
		cy := extent / 2
		h := stride * 0.3
		polys[i] = geometry.Polygon{Rings: [][]geometry.Point{{
			{X: cx - h, Y: cy - h},
			{X: cx + h, Y: cy - h},
			{X: cx + h, Y: cy + h},
			{X: cx - h, Y: cy + h},
			{X: cx - h, Y: cy - h},
		}}}
	}
	mp := geometry.MultiPolygon{Polygons: polys, CRSValue: geometry.WGS84}
	nameB.Append(prefix + "-mp")
	geomB.Append(geometry.WKB(mp))
	return newIndexedFrame(t, nameB, geomB)
}

// TestSJoin_PointsInSingleMP_ParityWithAoS — K=1 right-driven
// fast path parity. Constructs a many-small-polys MultiPolygon
// (≥ mpTreeMinSubPolys, so mpQueryPointCore takes the tree
// branch) as a single right row and joins against a point cloud.
// The sjoinPointsSinglePrepBatch path fires when the right frame
// has exactly one non-null polygon; results must match the AoS
// oracle exactly (order-insensitive multiset compare).
func TestSJoin_PointsInSingleMP_ParityWithAoS(t *testing.T) {
	right := buildSingleMultiPolygonFrame(t, 32, 10, "R")
	left := buildIndexedPointCloud(t, 500, 10, "L", 0xAAAA, 0xBBBB)
	compareSJoinAgainstAoS(t, left, right, "PointsInSingleMP")
}

// TestSJoin_PointsInSingleMP_SPWithin_ParityWithAoS — same shape
// with SPWithin. Both SPIntersects and SPWithin reduce to
// qContainsOrBoundary inside TestPointPrepared for the
// Point×MultiPolygon shape, but they take different branches in
// the SJoin dispatcher and AoS oracle — parity has to hold for
// both.
func TestSJoin_PointsInSingleMP_SPWithin_ParityWithAoS(t *testing.T) {
	right := buildSingleMultiPolygonFrame(t, 32, 10, "R")
	left := buildIndexedPointCloud(t, 500, 10, "L", 0xC0C0, 0xB0B0)
	compareSJoinAgainstAoSPred(t, left, right, SPWithin, "PointsInSingleMP_Within")
}

// TestSJoin_PointsInSingleMP_SubThreshold_ParityWithAoS — K=1 with
// a sub-threshold MultiPolygon (nSubPolys < mpTreeMinSubPolys=16),
// so mpQueryPointCore's linear bbox-reject scan runs instead of
// the R-tree path. Previously exercised only by the geometry-
// package prepared tests; this test locks it in at the SJoin
// level so a future refactor that flips the threshold or drops
// the linear branch has to break this parity check.
func TestSJoin_PointsInSingleMP_SubThreshold_ParityWithAoS(t *testing.T) {
	right := buildSingleMultiPolygonFrame(t, 8, 10, "R") // < 16
	left := buildIndexedPointCloud(t, 500, 10, "L", 0xDEAD, 0xBEEF)
	compareSJoinAgainstAoS(t, left, right, "PointsInSingleMP_Sub")
}

// TestSJoin_PointsInSinglePolygon_ParityWithAoS — K=1 fast path
// over a plain Polygon (not MultiPolygon) right row. Exercises
// testPointsVsPolygonBatch, which the MP fixtures never reach.
// Parity has to hold against the AoS oracle for both interior
// and boundary-inclusive hits.
func TestSJoin_PointsInSinglePolygon_ParityWithAoS(t *testing.T) {
	right := buildSinglePolygonFrame(t, 10, "R")
	left := buildIndexedPointCloud(t, 500, 10, "L", 0xFEED, 0xFACE)
	compareSJoinAgainstAoS(t, left, right, "PointsInSinglePolygon")
}

// TestSJoin_PointsInSingleMP_ParallelPath_NoRace — exercises the
// K=1 parallel shard path with the race detector. Uses >
// SJoinMinParallelRows so TestPointsPrepared gets dispatched
// across worker goroutines writing to disjoint slices of a
// shared out buffer.
func TestSJoin_PointsInSingleMP_ParallelPath_NoRace(t *testing.T) {
	right := buildSingleMultiPolygonFrame(t, 20, 10, "R")
	n := SJoinMinParallelRows + 500
	left := buildIndexedPointCloud(t, n, 10, "L", 0x1234, 0x5678)
	got, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
	if err != nil {
		t.Fatalf("SJoin: %v", err)
	}
	if got.NumRows() == 0 {
		t.Fatal("expected some matches on point-cloud × landMP shape")
	}
}

// BenchmarkSJoin_10kPointsInSingleLandMP measures the K=1
// right-driven fast path on the landMP-shaped workload that
// motivated the batch API: many points × one MultiPolygon
// carrying hundreds of small sub-polygons. The pre-K=1-gate
// path built a 1-item right R-tree, queried it 10k times
// returning the same candidate each call, then refined via
// PIPInclusiveFromWKB — pure overhead vs a direct batch call.
func BenchmarkSJoin_10kPointsInSingleLandMP(b *testing.B) {
	right := buildSingleMultiPolygonFrame(b, 100, 10, "R")
	left := buildIndexedPointCloud(b, 10_000, 10, "L", 0xC0DE, 0xBEEF)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		got, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		_ = got
	}
}

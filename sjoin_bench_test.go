package gobi

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

var sinkFrame *Frame

// buildBenchPolygonGrid returns a frame of gridSize x gridSize non-overlapping
// unit-square polygons tiling a plane.
func buildBenchPolygonGrid(b testing.TB, gridSize int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for y := range gridSize {
		for x := range gridSize {
			nameB.Append("")
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
		b.Fatal(err)
	}
	return f
}

// buildBenchPointCloud returns a frame of n random points uniformly scattered
// across the given [0, extent) x [0, extent) area.
func buildBenchPointCloud(b testing.TB, n int, extent float64) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(0xC0FFEE, 0xF00D))
	for range n {
		nameB.Append("")
		p := geometry.Point{X: rng.Float64() * extent, Y: rng.Float64() * extent}
		geomB.Append(geometry.WKB(p))
	}
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
		b.Fatal(err)
	}
	return f
}

// BenchmarkSJoin_10kPointsIn100Polygons exercises the R-tree pre-filter +
// exact predicate check on a realistic workload.
func BenchmarkSJoin_10kPointsIn100Polygons(b *testing.B) {
	polygons := buildBenchPolygonGrid(b, 10) // 100 polygons over [0,10)x[0,10)
	points := buildBenchPointCloud(b, 10_000, 10)

	b.ReportAllocs()
	for b.Loop() {
		out, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// BenchmarkSJoin_10kPointsIn100LargePolygons — the shape Slice 5's
// PreparedGeometry fast path was built for. 64-vertex polygons
// beat the AoS `pointInRing` inner-loop cost by enough that the
// TestPrepared dispatch overhead + Prepare's up-front
// materialization tax both amortize cleanly. The existing 5-vertex-
// square SJoin benches don't exercise this workload — their inner
// PIP loop is 4 segment tests, small enough that the TestPrepared
// call overhead cancels the SoA kernel win. This bench keeps the
// per-polygon candidate count similar (~100 candidates per
// polygon at this uniform-fill shape) while pushing the polygon
// complexity into the range where the held-view PIP kernel
// dominates.
func BenchmarkSJoin_10kPointsIn100LargePolygons(b *testing.B) {
	polygons := buildBenchLargePolygonRow(b, 100, 64) // 100 polys, 64 verts each
	points := buildBenchPointCloud(b, 10_000, 100)

	b.ReportAllocs()
	for b.Loop() {
		out, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// buildBenchLargePolygonRow returns a frame of numPolys polygons
// each with vertsPerPoly vertices arranged as a closed jagged
// circle. Polygons are laid out side-by-side along the X axis in
// a single row so they don't overlap; each polygon's bbox has
// side ~1 in the [0, numPolys) x [-1, 1] area.
func buildBenchLargePolygonRow(b testing.TB, numPolys, vertsPerPoly int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()

	rng := rand.New(rand.NewPCG(0xB16FACE5, 0x12345678))
	for i := range numPolys {
		nameB.Append("")
		cx := float64(i) + 0.5
		cy := 0.0
		pts := make([]geometry.Point, vertsPerPoly+1)
		for j := range vertsPerPoly {
			theta := 2.0 * math.Pi * float64(j) / float64(vertsPerPoly)
			r := 0.3 + rng.Float64()*0.15
			pts[j] = geometry.Point{
				X: cx + r*math.Cos(theta),
				Y: cy + r*math.Sin(theta),
			}
		}
		pts[vertsPerPoly] = pts[0]
		poly := geometry.SimplePolygon(pts, geometry.WGS84)
		geomB.Append(geometry.WKB(poly))
	}

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
		b.Fatal(err)
	}
	return f
}

// BenchmarkSJoin_1kLinesIn100Polygons — Slice 20c LineString ×
// Polygon SJoin fast path. Random line segments inside a
// 10x10 polygon grid; ~1 candidate polygon per line.
func BenchmarkSJoin_1kLinesIn100Polygons(b *testing.B) {
	polygons := buildBenchPolygonGrid(b, 10)
	lines := buildBenchLineCloud(b, 1_000, 10)
	b.ReportAllocs()
	for b.Loop() {
		out, err := lines.SJoin(polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// BenchmarkSJoin_1kLinesIn100Polygons_AoSOnly — same shapes but
// forces the AoS refine by calling legacySJoinAoS. Delta between
// this and the SoA bench above is the Slice-20c win.
func BenchmarkSJoin_1kLinesIn100Polygons_AoSOnly(b *testing.B) {
	polygons := buildBenchPolygonGrid(b, 10)
	lines := buildBenchLineCloud(b, 1_000, 10)
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacySJoinAoS(lines, polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// BenchmarkSJoin_1kPolysIn100Polygons — Slice 20d Polygon ×
// Polygon SJoin fast path. Small random polygons intersected
// against the 10x10 grid; ~1 candidate per left polygon.
func BenchmarkSJoin_1kPolysIn100Polygons(b *testing.B) {
	right := buildBenchPolygonGrid(b, 10)
	left := buildBenchSmallPolygonCloud(b, 1_000, 10)
	b.ReportAllocs()
	for b.Loop() {
		out, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// BenchmarkSJoin_1kPolysIn100Polygons_AoSOnly — same shapes but
// forces the AoS refine. Delta = Slice-20d win.
func BenchmarkSJoin_1kPolysIn100Polygons_AoSOnly(b *testing.B) {
	right := buildBenchPolygonGrid(b, 10)
	left := buildBenchSmallPolygonCloud(b, 1_000, 10)
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacySJoinAoS(left, right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// BenchmarkSJoin_1kLinesIn100LargePolygons — LineString × Polygon
// SJoin on 64-vertex right polygons.
func BenchmarkSJoin_1kLinesIn100LargePolygons(b *testing.B) {
	right := buildBenchLargePolygonRow(b, 100, 64)
	left := buildBenchLineCloud(b, 1_000, 100)
	b.ReportAllocs()
	for b.Loop() {
		out, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

func BenchmarkSJoin_1kLinesIn100LargePolygons_AoSOnly(b *testing.B) {
	right := buildBenchLargePolygonRow(b, 100, 64)
	left := buildBenchLineCloud(b, 1_000, 100)
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacySJoinAoS(left, right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// BenchmarkSJoin_1kPolysIn100LargePolygons — Polygon × Polygon
// SJoin on 64-vertex right polygons where the per-row decode is
// the dominant AoS cost. Small triangle left polygons.
func BenchmarkSJoin_1kPolysIn100LargePolygons(b *testing.B) {
	right := buildBenchLargePolygonRow(b, 100, 64)
	left := buildBenchSmallPolygonCloud(b, 1_000, 100)
	b.ReportAllocs()
	for b.Loop() {
		out, err := left.SJoin(right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

func BenchmarkSJoin_1kPolysIn100LargePolygons_AoSOnly(b *testing.B) {
	right := buildBenchLargePolygonRow(b, 100, 64)
	left := buildBenchSmallPolygonCloud(b, 1_000, 100)
	b.ReportAllocs()
	for b.Loop() {
		out, err := legacySJoinAoS(left, right, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// legacySJoinAoS runs SJoin without the Slice 16 / 20 fast paths,
// reproducing the pre-slice AoS refine (decode both sides, R-tree
// candidate lookup, per-pair AoS Test). Used for bench deltas.
func legacySJoinAoS(f, right *Frame, leftGeomCol, rightGeomCol string, pred SpatialPredicate) (*Frame, error) {
	lGeom, err := f.Column(leftGeomCol)
	if err != nil {
		return nil, err
	}
	rGeom, err := right.Column(rightGeomCol)
	if err != nil {
		return nil, err
	}
	rightGeoms, err := decodeGeometryColumn(rGeom)
	if err != nil {
		return nil, err
	}
	rightBounds := make([]geometry.Bounds, len(rightGeoms))
	for i, g := range rightGeoms {
		if g != nil {
			rightBounds[i] = g.Bounds()
		}
	}
	tree := geometry.NewRTree(rightBounds)
	leftGeoms, err := decodeGeometryColumn(lGeom)
	if err != nil {
		return nil, err
	}
	geomPred := pred.toGeometry()
	leftIdxs, rightIdxs := sjoinScan(leftGeoms, rightGeoms, tree, geomPred, 1)
	return assembleJoinedFrame(f, right, leftIdxs, rightIdxs, rightGeomCol)
}

// buildBenchLineCloud returns n random 2-vertex LineStrings each
// fitting in a ~0.5-unit segment within [0, extent) × [0, extent).
func buildBenchLineCloud(b testing.TB, n int, extent float64) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(0xF115, 0xACE5))
	for range n {
		nameB.Append("")
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
		b.Fatal(err)
	}
	return f
}

// buildBenchSmallPolygonCloud returns n random small triangle
// polygons scattered within [0, extent) × [0, extent). Each
// triangle side ~0.5.
func buildBenchSmallPolygonCloud(b testing.TB, n int, extent float64) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	rng := rand.New(rand.NewPCG(0xC0DE, 0xBEEF))
	for range n {
		nameB.Append("")
		cx := rng.Float64() * extent
		cy := rng.Float64() * extent
		pts := make([]geometry.Point, 4)
		for i := range 3 {
			theta := 2 * math.Pi * float64(i) / 3
			pts[i] = geometry.Point{
				X: cx + 0.3*math.Cos(theta),
				Y: cy + 0.3*math.Sin(theta),
			}
		}
		pts[3] = pts[0]
		poly := geometry.SimplePolygon(pts, geometry.WGS84)
		geomB.Append(geometry.WKB(poly))
	}
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
		b.Fatal(err)
	}
	return f
}

func BenchmarkSJoin_100kPointsIn10kPolygons(b *testing.B) {
	polygons := buildBenchPolygonGrid(b, 100) // 10,000 polygons over [0,100)x[0,100)
	points := buildBenchPointCloud(b, 100_000, 100)

	b.ReportAllocs()
	for b.Loop() {
		out, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/zoobst/gobi/geometry"
)

// TestSeries_GeomDistance3D_Projected — Cartesian dispatch when
// the column carries a projected EPSG.
func TestSeries_GeomDistance3D_Projected(t *testing.T) {
	origin := geometry.Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: geometry.PseudoMercator}
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 3, Y: 4, Z: 0, HasZ: true, CRSValue: geometry.PseudoMercator},  // dist 5
		geometry.Point{X: 0, Y: 0, Z: 12, HasZ: true, CRSValue: geometry.PseudoMercator}, // dist 12
		nil, // null → null
	})
	out, err := s.GeomDistance3D(origin, geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	if out.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("output dtype = %s, want Float64", out.DataType())
	}
	vals, arr, ok := out.singleF64()
	if !ok {
		t.Fatal("expected single-chunk Float64 output")
	}
	// vals[0] = 5, vals[1] = 12, vals[2] = null
	if math.Abs(vals[0]-5) > 1e-9 {
		t.Errorf("[0] = %v, want 5", vals[0])
	}
	if math.Abs(vals[1]-12) > 1e-9 {
		t.Errorf("[1] = %v, want 12", vals[1])
	}
	if !arr.IsNull(2) {
		t.Errorf("[2] should be null, got %v", vals[2])
	}
}

// TestSeries_GeomDistance3D_Geographic — ECEF-dispatch on WGS84
// column. 100m altitude delta at same lat/lon → ~100m distance.
func TestSeries_GeomDistance3D_Geographic(t *testing.T) {
	origin := geometry.Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: geometry.WGS84}
	s := geomSeries(t, "geometry", int32(geometry.WGS84.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, Z: 100, HasZ: true, CRSValue: geometry.WGS84},
		geometry.Point{X: 0, Y: 0, Z: 1000, HasZ: true, CRSValue: geometry.WGS84},
	})
	out, err := s.GeomDistance3D(origin, geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	vals, _, ok := out.singleF64()
	if !ok {
		t.Fatal("expected single-chunk output")
	}
	if math.Abs(vals[0]-100) > 0.001 {
		t.Errorf("[0] = %v, want ~100", vals[0])
	}
	if math.Abs(vals[1]-1000) > 0.001 {
		t.Errorf("[1] = %v, want ~1000", vals[1])
	}
}

// TestSeries_GeomZ — extract altitude column.
func TestSeries_GeomZ(t *testing.T) {
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 1, Y: 2, Z: 42, HasZ: true, CRSValue: geometry.PseudoMercator},
		geometry.Point{X: 3, Y: 4, CRSValue: geometry.PseudoMercator}, // 2D → null
		nil, // null → null
	})
	out, err := s.GeomZ()
	if err != nil {
		t.Fatal(err)
	}
	vals, arr, ok := out.singleF64()
	if !ok {
		t.Fatal("expected single-chunk output")
	}
	if vals[0] != 42 {
		t.Errorf("[0] = %v, want 42", vals[0])
	}
	if !arr.IsNull(1) {
		t.Error("[1] should be null (2D point has no Z)")
	}
	if !arr.IsNull(2) {
		t.Error("[2] should be null (null input)")
	}
}

// TestSeries_GeomForce2D_ForceZ — coord promote/demote round trip.
func TestSeries_GeomForce2D_ForceZ(t *testing.T) {
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 1, Y: 2, Z: 3, HasZ: true, CRSValue: geometry.PseudoMercator},
	})
	dropped, err := s.GeomForce2D()
	if err != nil {
		t.Fatal(err)
	}
	// Read back — the row's Z should be 0 and HasZ false.
	g0, err := dropped.Geometry(0)
	if err != nil {
		t.Fatal(err)
	}
	p := g0.(geometry.Point)
	if p.HasZ || p.Z != 0 {
		t.Errorf("Force2D: got HasZ=%v Z=%v, want false 0", p.HasZ, p.Z)
	}
	// Promote to 3D at alt=99.
	promoted, err := dropped.GeomForceZ(99)
	if err != nil {
		t.Fatal(err)
	}
	g1, err := promoted.Geometry(0)
	if err != nil {
		t.Fatal(err)
	}
	q := g1.(geometry.Point)
	if !q.HasZ || q.Z != 99 {
		t.Errorf("ForceZ: got HasZ=%v Z=%v, want true 99", q.HasZ, q.Z)
	}
}

// TestSeries_GeomIntersects3D_Prism — Point-in-prism batch.
func TestSeries_GeomIntersects3D_Prism(t *testing.T) {
	square := geometry.Polygon{
		Rings: [][]geometry.Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}},
		CRSValue: geometry.PseudoMercator,
	}
	prism, err := geometry.NewExtrudedPolygon(square, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 5, Y: 5, Z: 150, HasZ: true, CRSValue: geometry.PseudoMercator},   // in
		geometry.Point{X: 5, Y: 5, Z: 50, HasZ: true, CRSValue: geometry.PseudoMercator},    // below
		geometry.Point{X: 15, Y: 15, Z: 150, HasZ: true, CRSValue: geometry.PseudoMercator}, // outside
		nil, // null
	})
	out, err := s.GeomIntersects3D(prism)
	if err != nil {
		t.Fatal(err)
	}
	if out.DataType().ID() != arrow.BOOL {
		t.Fatalf("dtype = %s, want Bool", out.DataType())
	}
	// Iterate: expected [true, false, false, null]
	want := []struct {
		null bool
		val  bool
	}{
		{false, true}, {false, false}, {false, false}, {true, false},
	}
	for _, chunk := range out.col.Data().Chunks() {
		barr, ok := chunk.(interface {
			Value(int) bool
			IsNull(int) bool
			Len() int
		})
		if !ok {
			t.Fatalf("chunk not Boolean-shaped: %T", chunk)
		}
		if barr.Len() != len(want) {
			t.Fatalf("len = %d, want %d", barr.Len(), len(want))
		}
		for i, w := range want {
			if w.null {
				if !barr.IsNull(i) {
					t.Errorf("[%d]: expected null", i)
				}
				continue
			}
			if barr.Value(i) != w.val {
				t.Errorf("[%d]: got %v, want %v", i, barr.Value(i), w.val)
			}
		}
	}
}

// TestExpr_GeomDistance3D_RoundTrip — Expr wiring produces same
// values as the Series-level call.
func TestExpr_GeomDistance3D_RoundTrip(t *testing.T) {
	origin := geometry.Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: geometry.PseudoMercator}
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 3, Y: 4, Z: 0, HasZ: true, CRSValue: geometry.PseudoMercator},
	})
	f, err := NewFrame(
		arrow.NewSchema([]arrow.Field{s.field}, nil),
		[]arrow.Column{*s.col},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := Col("geometry").GeomDistance3D(origin, geometry.UnitMeters)
	got, err := e.node.Eval(f)
	if err != nil {
		t.Fatal(err)
	}
	vals, _, ok := got.singleF64()
	if !ok {
		t.Fatal("expected single-chunk output")
	}
	if math.Abs(vals[0]-5) > 1e-9 {
		t.Errorf("Expr GeomDistance3D: got %v, want 5", vals[0])
	}
	if got.name != "geometry_distance_3d" {
		t.Errorf("output name = %q, want geometry_distance_3d", got.name)
	}
}

// TestExpr_GeomIntersects3D_RoundTrip — Expr wiring produces same
// bool as Series-level call.
func TestExpr_GeomIntersects3D_RoundTrip(t *testing.T) {
	square := geometry.Polygon{
		Rings: [][]geometry.Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}},
		CRSValue: geometry.PseudoMercator,
	}
	prism, err := geometry.NewExtrudedPolygon(square, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 5, Y: 5, Z: 150, HasZ: true, CRSValue: geometry.PseudoMercator},
	})
	f, err := NewFrame(
		arrow.NewSchema([]arrow.Field{s.field}, nil),
		[]arrow.Column{*s.col},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := Col("geometry").GeomIntersects3D(prism)
	got, err := e.node.Eval(f)
	if err != nil {
		t.Fatal(err)
	}
	// One-row bool result: read it back.
	chunk := got.col.Data().Chunks()[0]
	barr := chunk.(interface {
		Value(int) bool
		IsNull(int) bool
	})
	if barr.IsNull(0) || !barr.Value(0) {
		t.Errorf("expected true, got null=%v val=%v", barr.IsNull(0), barr.Value(0))
	}
}

// TestSeries_GeomLength3D_Projected — Cartesian sum-of-segment
// distances on a LineString-Z column. Zig-zag fixture with a
// known Z stride so the expected value falls out of a small manual
// calc; also covers null passthrough on a non-line row.
func TestSeries_GeomLength3D_Projected(t *testing.T) {
	// Line 1: (0,0,0) → (3,4,0) → (3,4,12). Segments: 5, 12. Total: 17.
	line1 := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, Z: 0, HasZ: true},
			{X: 3, Y: 4, Z: 0, HasZ: true},
			{X: 3, Y: 4, Z: 12, HasZ: true},
		},
		HasZ: true, CRSValue: geometry.PseudoMercator,
	}
	// Row 2: single-vertex line → length 0.
	line2 := geometry.LineString{
		Points: []geometry.Point{{X: 5, Y: 5, Z: 5, HasZ: true}},
		HasZ:   true, CRSValue: geometry.PseudoMercator,
	}
	// Row 3: not a line → null passthrough.
	pt := geometry.Point{X: 1, Y: 2, Z: 3, HasZ: true, CRSValue: geometry.PseudoMercator}
	s := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		line1, line2, pt, nil,
	})
	out, err := s.GeomLength3D(geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	vals, arr, ok := out.singleF64()
	if !ok {
		t.Fatal("expected single-chunk output")
	}
	if math.Abs(vals[0]-17) > 1e-9 {
		t.Errorf("[0] = %v, want 17", vals[0])
	}
	if vals[1] != 0 {
		t.Errorf("[1] = %v, want 0 (single-vertex line)", vals[1])
	}
	if !arr.IsNull(2) {
		t.Errorf("[2] should be null (Point row, non-line)")
	}
	if !arr.IsNull(3) {
		t.Errorf("[3] should be null (null input)")
	}
}

// TestSeries_GeomLength3D_Geographic — geodesic dispatch on WGS84.
// (0°N, 0°E, 0m) → (0°N, 0°E, 100m) → (0°N, 0°E, 300m) → total
// altitude climb of 300m = ECEF distance of ~300m ± rounding.
func TestSeries_GeomLength3D_Geographic(t *testing.T) {
	line := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, Z: 0, HasZ: true},
			{X: 0, Y: 0, Z: 100, HasZ: true},
			{X: 0, Y: 0, Z: 300, HasZ: true},
		},
		HasZ: true, CRSValue: geometry.WGS84,
	}
	s := geomSeries(t, "geometry", int32(geometry.WGS84.EPSG), []geometry.Geometry{line})
	out, err := s.GeomLength3D(geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	vals, _, ok := out.singleF64()
	if !ok {
		t.Fatal("expected single-chunk output")
	}
	if math.Abs(vals[0]-300) > 0.001 {
		t.Errorf("length = %v, want ~300 m", vals[0])
	}
}

// TestSeries_GeomDistance3D_MultiChunk — Series-level 3D ops that
// walk the WKB column via `s.col.Data().Chunks()` must handle
// multi-chunk columns (e.g. from Concat). Verifies the column
// pass concatenates chunks correctly and produces the expected
// distance-per-row output.
func TestSeries_GeomDistance3D_MultiChunk(t *testing.T) {
	origin := geometry.Point{X: 0, Y: 0, Z: 0, HasZ: true, CRSValue: geometry.PseudoMercator}
	// Two single-chunk fixtures Concat'd → multi-chunk column.
	sA := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 3, Y: 4, Z: 0, HasZ: true, CRSValue: geometry.PseudoMercator}, // 5
	})
	sB := geomSeries(t, "geometry", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, Z: 12, HasZ: true, CRSValue: geometry.PseudoMercator}, // 12
		nil, // null pass-through across the chunk boundary
	})
	// Build a two-chunk column by hand — Series.Concat isn't in
	// scope, and using Frame.Concat would drag in cross-Frame
	// setup. Direct arrow-level append keeps this test tight.
	chunks := []arrow.Array{
		sA.col.Data().Chunks()[0],
		sB.col.Data().Chunks()[0],
	}
	for _, c := range chunks {
		c.Retain()
	}
	chunked := arrow.NewChunked(chunks[0].DataType(), chunks)
	col := arrow.NewColumn(sA.field, chunked)
	chunked.Release()
	for _, c := range chunks {
		c.Release()
	}
	multi := Series{name: sA.field.Name, field: sA.field, col: col}

	if multi.Len() != 3 {
		t.Fatalf("multi-chunk fixture Len=%d, want 3", multi.Len())
	}
	// Distance3D on the multi-chunk column.
	out, err := multi.GeomDistance3D(origin, geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != 3 {
		t.Fatalf("output Len=%d, want 3", out.Len())
	}
	// Compact into a single-chunk Float64 for readback via
	// singleF64 (which expects one chunk).
	vals := make([]float64, 0, 3)
	nulls := make([]bool, 0, 3)
	for _, chunk := range out.col.Data().Chunks() {
		f64 := chunk.(interface {
			Value(int) float64
			IsNull(int) bool
			Len() int
		})
		for i := 0; i < f64.Len(); i++ {
			nulls = append(nulls, f64.IsNull(i))
			vals = append(vals, f64.Value(i))
		}
	}
	if math.Abs(vals[0]-5) > 1e-9 {
		t.Errorf("[0] = %v, want 5", vals[0])
	}
	if math.Abs(vals[1]-12) > 1e-9 {
		t.Errorf("[1] = %v, want 12", vals[1])
	}
	if !nulls[2] {
		t.Errorf("[2] should be null (multi-chunk null passthrough)")
	}
}

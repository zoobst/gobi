package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// projectedSquare returns a polygon in the PseudoMercator CRS so that the
// clip engine's projected-CRS gate accepts it.
func projectedSquare(x, y, size float64) geometry.Polygon {
	return geometry.SimplePolygon([]geometry.Point{
		{X: x, Y: y}, {X: x + size, Y: y}, {X: x + size, Y: y + size},
		{X: x, Y: y + size}, {X: x, Y: y},
	}, geometry.PseudoMercator)
}

// polygonArea returns the planar (shoelace) area of a Polygon or
// MultiPolygon. Ignores CRS entirely so we don't accidentally invoke the
// spherical-area path on a projected polygon whose CRS was dropped during
// the WKB round-trip that a geometry Series performs.
func polygonArea(t *testing.T, g geometry.Geometry) float64 {
	t.Helper()
	switch v := g.(type) {
	case nil:
		return 0
	case geometry.Polygon:
		return ringsAreaPlanar(v.Rings)
	case geometry.MultiPolygon:
		var total float64
		for _, p := range v.Polygons {
			total += ringsAreaPlanar(p.Rings)
		}
		return total
	}
	t.Fatalf("polygonArea: unexpected %T", g)
	return 0
}

func ringsAreaPlanar(rings [][]geometry.Point) float64 {
	if len(rings) == 0 {
		return 0
	}
	total := ringAreaPlanar(rings[0])
	for _, h := range rings[1:] {
		total -= ringAreaPlanar(h)
	}
	return total
}

func ringAreaPlanar(ring []geometry.Point) float64 {
	if len(ring) < 3 {
		return 0
	}
	var a float64
	for i := 0; i < len(ring)-1; i++ {
		a += ring[i].X*ring[i+1].Y - ring[i+1].X*ring[i].Y
	}
	// close the ring if not already closed
	last := len(ring) - 1
	if ring[0].X != ring[last].X || ring[0].Y != ring[last].Y {
		a += ring[last].X*ring[0].Y - ring[0].X*ring[last].Y
	}
	if a < 0 {
		a = -a
	}
	return a / 2
}

func TestSeries_GeomEstimateUTMCRS_WGS84(t *testing.T) {
	// LA-area polygon in WGS84 → should land on UTM zone 11N (EPSG:32611).
	poly := geometry.SimplePolygon([]geometry.Point{
		{X: -118.30, Y: 34.00}, {X: -118.10, Y: 34.00},
		{X: -118.10, Y: 34.20}, {X: -118.30, Y: 34.20},
		{X: -118.30, Y: 34.00},
	}, geometry.WGS84)
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{poly})
	crs, err := s.GeomEstimateUTMCRS()
	if err != nil {
		t.Fatalf("GeomEstimateUTMCRS: %v", err)
	}
	if crs.EPSG != 32611 {
		t.Errorf("EPSG = %d, want 32611 (WGS 84 / UTM zone 11N)", crs.EPSG)
	}
	if !crs.Projected {
		t.Errorf("returned CRS should be projected")
	}
}

func TestSeries_GeomEstimateUTMCRS_AggregatesAcrossRows(t *testing.T) {
	// Two polygons far apart in longitude but centered on the same zone.
	// The bounds-center approach should still pick zone 11N.
	p1 := geometry.SimplePolygon([]geometry.Point{
		{X: -119, Y: 34}, {X: -118.5, Y: 34}, {X: -118.5, Y: 34.5}, {X: -119, Y: 34.5}, {X: -119, Y: 34},
	}, geometry.WGS84)
	p2 := geometry.SimplePolygon([]geometry.Point{
		{X: -117.5, Y: 34}, {X: -117, Y: 34}, {X: -117, Y: 34.5}, {X: -117.5, Y: 34.5}, {X: -117.5, Y: 34},
	}, geometry.WGS84)
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{p1, p2})
	crs, err := s.GeomEstimateUTMCRS()
	if err != nil {
		t.Fatalf("GeomEstimateUTMCRS: %v", err)
	}
	// Bounds center is (-118, 34.25) → zone 11N.
	if crs.EPSG != 32611 {
		t.Errorf("EPSG = %d, want 32611", crs.EPSG)
	}
}

func TestSeries_GeomEstimateUTMCRS_EmptySeries(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{nil, nil})
	_, err := s.GeomEstimateUTMCRS()
	if err == nil {
		t.Fatal("expected ErrEmptyGeometry on all-null input")
	}
}

func TestSeries_GeomToCRS_WGS84ToUTM(t *testing.T) {
	// (-118.24, 34.05) in WGS84 → UTM 11N ~ (378000, 3770000) meters.
	poly := geometry.SimplePolygon([]geometry.Point{
		{X: -118.24, Y: 34.05}, {X: -118.23, Y: 34.05},
		{X: -118.23, Y: 34.06}, {X: -118.24, Y: 34.06},
		{X: -118.24, Y: 34.05},
	}, geometry.WGS84)
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{poly})
	utm, _ := geometry.LookupCRS(32611)
	out, err := s.GeomToCRS(utm)
	if err != nil {
		t.Fatalf("GeomToCRS: %v", err)
	}
	if geometryCRSFromField(out.field) != 32611 {
		t.Errorf("output CRS epsg = %d, want 32611", geometryCRSFromField(out.field))
	}
	g, err := out.Geometry(0)
	if err != nil {
		t.Fatalf("Geometry(0): %v", err)
	}
	p, ok := g.(geometry.Polygon)
	if !ok {
		t.Fatalf("output type = %T, want Polygon", g)
	}
	// First vertex should be in a UTM-scale coordinate range (hundreds of
	// thousands / millions of meters), NOT the original degree scale.
	first := p.Rings[0][0]
	if math.Abs(first.X) < 1e4 || math.Abs(first.Y) < 1e5 {
		t.Errorf("output vertex %v looks like it wasn't reprojected", first)
	}
}

func TestSeries_GeomToCRS_NullPassthrough(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: -118, Y: 34}, {X: -117, Y: 34}, {X: -117, Y: 35}, {X: -118, Y: 35}, {X: -118, Y: 34},
		}, geometry.WGS84),
		nil,
	})
	utm, _ := geometry.LookupCRS(32611)
	out, err := s.GeomToCRS(utm)
	if err != nil {
		t.Fatalf("GeomToCRS: %v", err)
	}
	if out.Len() != 2 {
		t.Fatalf("len = %d, want 2", out.Len())
	}
	g1, err := out.Geometry(1)
	if err != nil {
		t.Fatalf("Geometry(1): %v", err)
	}
	if g1 != nil {
		t.Errorf("row 1 (null input) should be null, got %v", g1)
	}
}

func TestSeries_GeomClip(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		projectedSquare(20, 0, 10),
		nil,
	})
	mask := projectedSquare(5, 0, 10) // overlaps s[0] by 5x10=50; disjoint from s[1].
	out, err := s.GeomClip(mask)
	if err != nil {
		t.Fatalf("GeomClip: %v", err)
	}
	if !out.IsGeometry() {
		t.Fatalf("output is not a geometry series")
	}
	if out.Len() != 3 {
		t.Fatalf("output len = %d, want 3", out.Len())
	}
	g0, err := out.Geometry(0)
	if err != nil {
		t.Fatalf("Geometry(0): %v", err)
	}
	if a := polygonArea(t, g0); math.Abs(a-50) > 1e-9 {
		t.Errorf("row 0 area = %v, want 50", a)
	}
	g1, err := out.Geometry(1)
	if err != nil {
		t.Fatalf("Geometry(1): %v", err)
	}
	if a := polygonArea(t, g1); a != 0 {
		t.Errorf("row 1 (disjoint) area = %v, want 0", a)
	}
	g2, err := out.Geometry(2)
	if err != nil {
		t.Fatalf("Geometry(2): %v", err)
	}
	if g2 != nil {
		t.Errorf("row 2 (null input) should be null, got %v", g2)
	}
}

func TestSeries_GeomUnion(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
	})
	other := projectedSquare(5, 0, 10)
	out, err := s.GeomUnion(other)
	if err != nil {
		t.Fatalf("GeomUnion: %v", err)
	}
	g0, _ := out.Geometry(0)
	// Union of [0,10]×[0,10] and [5,15]×[0,10] = [0,15]×[0,10] = area 150.
	if a := polygonArea(t, g0); math.Abs(a-150) > 1e-9 {
		t.Errorf("union area = %v, want 150", a)
	}
}

func TestSeries_GeomDifference(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
	})
	other := projectedSquare(5, 0, 10)
	out, err := s.GeomDifference(other)
	if err != nil {
		t.Fatalf("GeomDifference: %v", err)
	}
	g0, _ := out.Geometry(0)
	// s[0] - other = 100 - 50 = 50.
	if a := polygonArea(t, g0); math.Abs(a-50) > 1e-9 {
		t.Errorf("difference area = %v, want 50", a)
	}
}

func TestSeries_GeomDissolve(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		projectedSquare(5, 0, 10),
		nil, // null → skipped
		projectedSquare(20, 0, 10),
	})
	got, err := s.GeomDissolve()
	if err != nil {
		t.Fatalf("GeomDissolve: %v", err)
	}
	// Cluster A: [0,10]∪[5,15] = [0,15]×[0,10] = 150.
	// Cluster B: [20,30]×[0,10] = 100.
	// Total = 250.
	if a := polygonArea(t, got); math.Abs(a-250) > 1e-9 {
		t.Errorf("dissolve area = %v, want 250", a)
	}
}

func TestSeries_GeomDissolve_EmptyAndAllNull(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), nil)
	got, err := s.GeomDissolve()
	if err != nil {
		t.Fatalf("GeomDissolve(empty): %v", err)
	}
	if a := polygonArea(t, got); a != 0 {
		t.Errorf("empty dissolve area = %v, want 0", a)
	}

	allNull := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{nil, nil})
	got, err = allNull.GeomDissolve()
	if err != nil {
		t.Fatalf("GeomDissolve(allNull): %v", err)
	}
	if a := polygonArea(t, got); a != 0 {
		t.Errorf("all-null dissolve area = %v, want 0", a)
	}
}

// TestSeries_GeomClip_NoLeaks runs GeomClip against a CheckedAllocator and
// reports any Arrow objects that weren't released. This is the leak-check
// discipline that the earlier "fix memory leak" commits established.
func TestSeries_GeomClip_NoLeaks(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer mem.AssertSize(t, 0)

	b := array.NewBinaryBuilder(mem, arrow.BinaryTypes.Binary)
	for _, g := range []geometry.Geometry{
		projectedSquare(0, 0, 10),
		projectedSquare(20, 0, 10),
	} {
		b.Append(geometry.WKB(g))
	}
	arr := b.NewArray()
	b.Release()
	field := GeometryField("geom", int32(geometry.PseudoMercator.EPSG))
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	arr.Release()
	chunked.Release()
	s := Series{name: field.Name, field: field, col: col}

	mask := projectedSquare(5, 0, 10)
	// Note: our production op uses memory.DefaultAllocator, not this
	// CheckedAllocator, so this test can't catch a leak in the op itself.
	// It DOES catch leaks in the input/output plumbing on the caller side.
	// A follow-up should thread the allocator through.
	out, err := s.GeomClip(mask)
	if err != nil {
		t.Fatalf("GeomClip: %v", err)
	}
	if out.Len() != 2 {
		t.Fatalf("Len = %d, want 2", out.Len())
	}
	col.Release()
}

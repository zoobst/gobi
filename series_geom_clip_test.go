package gobi

import (
	"errors"
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
	if !crs.Projected() {
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

// TestSeriesGeomUnion_BboxDisjointFastPath — Slice 21a union
// bbox-disjoint fast path. Rows whose bboxes don't overlap the
// mask should return a MultiPolygon combining both. Rows that
// overlap the mask fall through to the AoS sweep. Both paths
// must produce the same area as the AoS-only reference.
func TestSeriesGeomUnion_BboxDisjointFastPath(t *testing.T) {
	// 3 rows: one disjoint, one overlapping, one fully contained.
	disjoint := projectedSquare(0, 0, 5)
	overlap := projectedSquare(20, 20, 15)
	contained := projectedSquare(25, 25, 5)
	other := projectedSquare(20, 20, 10)

	f := makeGeomFrame(t, disjoint, overlap, contained)
	geomCol, _ := f.Column("geometry")

	got, err := geomCol.GeomUnion(other)
	if err != nil {
		t.Fatal(err)
	}
	// AoS-only oracle: force sweep by clearing the fast path via a
	// helper that skips the disjoint check.
	want, err := legacyGeomBinaryOp(geomCol, other, geometry.OpUnion, "_union")
	if err != nil {
		t.Fatal(err)
	}
	compareGeomSeriesAreas(t, got, want, "GeomUnion")
}

// TestSeriesGeomSymDifference_BboxDisjointFastPath — Slice 21b.
// Same shape as Union — a △ b when disjoint = a ∪ b.
func TestSeriesGeomSymDifference_BboxDisjointFastPath(t *testing.T) {
	disjoint := projectedSquare(0, 0, 5)
	overlap := projectedSquare(20, 20, 15)
	other := projectedSquare(20, 20, 10)

	f := makeGeomFrame(t, disjoint, overlap)
	geomCol, _ := f.Column("geometry")

	got, err := geomCol.GeomSymDifference(other)
	if err != nil {
		t.Fatal(err)
	}
	want, err := legacyGeomBinaryOp(geomCol, other, geometry.OpSymDifference, "_symdiff")
	if err != nil {
		t.Fatal(err)
	}
	compareGeomSeriesAreas(t, got, want, "GeomSymDifference")
}

func makeGeomFrame(t testing.TB, polys ...geometry.Polygon) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for _, p := range polys {
		gb.Append(geometry.WKB(p))
	}
	fields := []arrow.Field{
		GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	schema := arrow.NewSchema(fields, nil)
	arr := gb.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := *arrow.NewColumn(fields[0], chunked)
	chunked.Release()
	f, err := NewFrame(schema, []arrow.Column{col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func compareGeomSeriesAreas(t *testing.T, got, want Series, label string) {
	t.Helper()
	// Each row is a WKB geometry — decode both and compare areas.
	gotAreas := seriesRowAreas(t, got)
	wantAreas := seriesRowAreas(t, want)
	if len(gotAreas) != len(wantAreas) {
		t.Fatalf("%s: row count mismatch: got=%d want=%d", label, len(gotAreas), len(wantAreas))
	}
	for i := range gotAreas {
		// NaN sentinel = "null cell"; both sides must agree on
		// null-ness. Comparing NaNs via math.Abs would always fail,
		// so branch on IsNaN.
		if math.IsNaN(gotAreas[i]) || math.IsNaN(wantAreas[i]) {
			if !math.IsNaN(gotAreas[i]) || !math.IsNaN(wantAreas[i]) {
				t.Errorf("%s row %d: null-ness mismatch got=%v want=%v",
					label, i, gotAreas[i], wantAreas[i])
			}
			continue
		}
		if math.Abs(gotAreas[i]-wantAreas[i]) > 1e-6*math.Max(1, math.Abs(wantAreas[i])) {
			t.Errorf("%s row %d: got area=%v want area=%v", label, i, gotAreas[i], wantAreas[i])
		}
	}
}

func seriesRowAreas(t *testing.T, s Series) []float64 {
	t.Helper()
	var out []float64
	for _, chunk := range s.col.Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				// Sentinel NaN, not 0, so a future regression that
				// silently turned a non-null cell into null wouldn't
				// pass the area-equality check by coincidence
				// (null-vs-empty-polygon both looked like 0 pre-fix).
				out = append(out, math.NaN())
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				t.Fatal(err)
			}
			switch v := g.(type) {
			case geometry.Polygon:
				out = append(out, planarPolygonArea(v))
			case geometry.MultiPolygon:
				var a float64
				for _, p := range v.Polygons {
					a += planarPolygonArea(p)
				}
				out = append(out, a)
			default:
				out = append(out, 0)
			}
		}
	}
	return out
}

func planarPolygonArea(p geometry.Polygon) float64 {
	if len(p.Rings) == 0 {
		return 0
	}
	a := geometry.PlanarRingArea(p.Rings[0])
	for _, h := range p.Rings[1:] {
		a -= geometry.PlanarRingArea(h)
	}
	if a < 0 {
		a = -a
	}
	return a
}

// GeomClip takes point and line rows as well as polygons. Rows with
// nothing inside the mask come back as empty geometries, whether they
// miss the mask's bounding box (the WKB fast path) or only the mask
// itself.
func TestSeries_GeomClip_PointsAndLines(t *testing.T) {
	crs := geometry.PseudoMercator
	pt := func(x, y float64) geometry.Point { return geometry.Point{X: x, Y: y, CRSValue: crs} }
	line := func(xy ...float64) geometry.LineString {
		l := geometry.LineString{CRSValue: crs}
		for i := 0; i < len(xy); i += 2 {
			l.Points = append(l.Points, geometry.Point{X: xy[i], Y: xy[i+1]})
		}
		return l
	}
	// An L-shaped mask: the 10×10 square without its top-right quarter,
	// so (8, 8) is inside the bounding box but outside the mask.
	mask := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 5}, {X: 5, Y: 5}, {X: 5, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}, crs)
	s := geomSeries(t, "geom", int32(crs.EPSG), []geometry.Geometry{
		pt(2, 2),           // 0: inside
		pt(8, 8),           // 1: in the bbox, outside the mask
		pt(50, 50),         // 2: outside the bbox
		pt(10, 3),          // 3: on the boundary counts as inside
		nil,                // 4
		line(-5, 2, 20, 2), // 5: crosses once, 10 long inside
		line(-5, 8, 20, 8), // 6: inside only for x in 0..5
		line(2, -5, 2, 2, 8, 2, 8, 8, 2, 8, 2, 20), // 7: leaves and re-enters: two pieces
		line(50, 50, 60, 60),                       // 8: outside the bbox
		geometry.MultiPoint{Points: []geometry.Point{{X: 1, Y: 1}, {X: 8, Y: 8}, {X: 3, Y: 9}}, CRSValue: crs},          // 9
		geometry.MultiLineString{Lines: []geometry.LineString{line(-5, 1, 20, 1), line(50, 50, 60, 60)}, CRSValue: crs}, // 10
		projectedSquare(4, 4, 2), // 11: polygon rows still work beside them
	})
	out, err := s.GeomClip(mask)
	if err != nil {
		t.Fatalf("GeomClip: %v", err)
	}
	if out.Len() != s.Len() {
		t.Fatalf("len = %d, want %d", out.Len(), s.Len())
	}
	geom := func(row int) geometry.Geometry {
		t.Helper()
		g, err := out.Geometry(row)
		if err != nil {
			t.Fatalf("row %d: %v", row, err)
		}
		return g
	}
	lineLength := func(g geometry.Geometry) float64 {
		var lines []geometry.LineString
		switch v := g.(type) {
		case geometry.LineString:
			lines = []geometry.LineString{v}
		case geometry.MultiLineString:
			lines = v.Lines
		}
		var total float64
		for _, l := range lines {
			for i := 1; i < len(l.Points); i++ {
				total += math.Hypot(l.Points[i].X-l.Points[i-1].X, l.Points[i].Y-l.Points[i-1].Y)
			}
		}
		return total
	}

	for _, row := range []int{0, 3} {
		if p, ok := geom(row).(geometry.Point); !ok {
			t.Errorf("row %d = %T, want the Point kept", row, geom(row))
		} else if want, _ := s.Geometry(row); p.X != want.(geometry.Point).X || p.Y != want.(geometry.Point).Y {
			t.Errorf("row %d = %+v, want %+v", row, p, want)
		}
	}
	for _, row := range []int{1, 2} {
		if mp, ok := geom(row).(geometry.MultiPoint); !ok || len(mp.Points) != 0 {
			t.Errorf("row %d = %T %+v, want an empty MultiPoint", row, geom(row), geom(row))
		}
	}
	if !out.Nulls()[4] {
		t.Error("row 4: null should stay null")
	}
	for row, want := range map[int]float64{5: 10, 6: 5} {
		g := geom(row)
		if _, ok := g.(geometry.LineString); !ok {
			t.Errorf("row %d = %T, want one LineString", row, g)
		}
		if got := lineLength(g); math.Abs(got-want) > 1e-9 {
			t.Errorf("row %d: length inside = %v, want %v", row, got, want)
		}
	}
	// Row 7 inside the mask: (2,0)→(2,2)→(8,2)→(8,5) is 11 long, then
	// (5,8)→(2,8)→(2,10) is 5 long.
	if ml, ok := geom(7).(geometry.MultiLineString); !ok || len(ml.Lines) != 2 {
		t.Errorf("row 7 = %T %+v, want a MultiLineString of 2 pieces", geom(7), geom(7))
	} else if got := lineLength(ml); math.Abs(got-16) > 1e-9 {
		t.Errorf("row 7: length inside = %v, want 16", got)
	}
	if l, ok := geom(8).(geometry.LineString); !ok || len(l.Points) != 0 {
		t.Errorf("row 8 = %T %+v, want an empty LineString", geom(8), geom(8))
	}
	if mp, ok := geom(9).(geometry.MultiPoint); !ok || len(mp.Points) != 2 {
		t.Errorf("row 9 = %T %+v, want a MultiPoint of the 2 points inside", geom(9), geom(9))
	}
	if ml, ok := geom(10).(geometry.MultiLineString); !ok || len(ml.Lines) != 1 || math.Abs(lineLength(ml)-10) > 1e-9 {
		t.Errorf("row 10 = %T %+v, want a MultiLineString of one 10-long piece", geom(10), geom(10))
	}
	// The 2×2 square at (4,4) keeps everything except its top-right
	// 1×1 quarter.
	if area := polygonArea(t, geom(11)); math.Abs(area-3) > 1e-9 {
		t.Errorf("row 11 area = %v, want 3", area)
	}

	// The other boolean ops still need polygon rows.
	if _, err := s.GeomDifference(mask); err == nil {
		t.Error("GeomDifference on point rows: expected an error")
	}
}

// The mask and CRS are checked once, before any row: whether the call
// fails must not depend on which rows happen to reach the mask.
func TestSeries_GeomClip_ChecksInputsUpFront(t *testing.T) {
	far := geometry.Point{X: 50, Y: 50}
	mask := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}, geometry.CRS{})

	// Geographic CRS: an error even though no row is near the mask,
	// and even with no rows at all.
	for name, rows := range map[string][]geometry.Geometry{
		"rows away from the mask": {far, nil, geometry.LineString{Points: []geometry.Point{{X: 60, Y: 60}, {X: 61, Y: 61}}}},
		"a row inside the mask":   {far, geometry.Point{X: 0.5, Y: 0.5}},
		"no rows":                 {},
	} {
		s := geomSeries(t, "geom", int32(geometry.WGS84.EPSG), rows)
		if _, err := s.GeomClip(mask); !errors.Is(err, geometry.ErrGeographicCRS) {
			t.Errorf("EPSG:4326, %s: err = %v, want ErrGeographicCRS", name, err)
		}
	}

	// A mask that isn't a polygon: an error for every op, again
	// wherever the rows are.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{projectedSquare(100, 100, 1), nil})
	line := geometry.LineString{Points: []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}}}
	for name, op := range map[string]func(geometry.Geometry) (Series, error){
		"GeomClip": s.GeomClip, "GeomUnion": s.GeomUnion, "GeomDifference": s.GeomDifference, "GeomSymDifference": s.GeomSymDifference,
	} {
		if _, err := op(line); err == nil {
			t.Errorf("%s with a LineString mask: expected an error", name)
		}
		if _, err := op(nil); err == nil {
			t.Errorf("%s with a nil mask: expected an error", name)
		}
	}

	// A projected CRS with nothing near the mask still works.
	out, err := s.GeomClip(mask)
	if err != nil {
		t.Fatalf("projected CRS: %v", err)
	}
	if g, _ := out.Geometry(0); !geometry.IsEmpty(g) {
		t.Errorf("row 0 = %+v, want an empty polygon", g)
	}
}

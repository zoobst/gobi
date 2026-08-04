package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/geometry"
)

func stringSeriesValues(t *testing.T, s Series) []any {
	t.Helper()
	out := make([]any, 0, s.Len())
	for _, chunk := range s.Column().Data().Chunks() {
		b := chunk.(*array.String)
		for i := range b.Len() {
			if b.IsNull(i) {
				out = append(out, nil)
			} else {
				out = append(out, b.Value(i))
			}
		}
	}
	return out
}

func TestSeries_GeomBuffer_RoundAndSquare(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
	})
	// Round buffer with default 32 segments — area ≈ π*r² for r=5.
	got, err := s.GeomBuffer(5, geometry.BufferOptions{})
	if err != nil {
		t.Fatalf("GeomBuffer round: %v", err)
	}
	g0, _ := got.Geometry(0)
	roundArea := polygonArea(t, g0)
	if roundArea < 76 || roundArea > 79 {
		// 32-gon inscribed in radius-5 circle: (1/2)*32*25*sin(2π/32) ≈ 78.02
		t.Errorf("round buffer area = %v, want ~78 (32-gon in r=5 circle)", roundArea)
	}
	// Square buffer of same distance — a 10×10 square, area exactly 100.
	got, err = s.GeomBuffer(5, geometry.BufferOptions{Style: geometry.BufferSquare})
	if err != nil {
		t.Fatalf("GeomBuffer square: %v", err)
	}
	g0, _ = got.Geometry(0)
	if area := polygonArea(t, g0); math.Abs(area-100) > 1e-9 {
		t.Errorf("square buffer area = %v, want 100", area)
	}
}

func TestSeries_GeomSimplify(t *testing.T) {
	// Line with a nearly-straight kink: (0,0)-(5,0.001)-(10,0). Simplify
	// at tolerance 0.01 should drop the middle vertex.
	line := geometry.LineString{
		Points:   []geometry.Point{{X: 0, Y: 0}, {X: 5, Y: 0.001}, {X: 10, Y: 0}},
		CRSValue: geometry.PseudoMercator,
	}
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{line})
	got, err := s.GeomSimplify(0.01)
	if err != nil {
		t.Fatalf("GeomSimplify: %v", err)
	}
	g, _ := got.Geometry(0)
	l, ok := g.(geometry.LineString)
	if !ok {
		t.Fatalf("got %T, want LineString", g)
	}
	if len(l.Points) != 2 {
		t.Errorf("simplified vertex count = %d, want 2", len(l.Points))
	}
}

func TestSeries_GeomConvexHull(t *testing.T) {
	// Concave L-shape polygon: convex hull is its bounding rectangle.
	l := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 10}, {X: 10, Y: 10},
		{X: 10, Y: 20}, {X: 0, Y: 20}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{l})
	got, err := s.GeomConvexHull()
	if err != nil {
		t.Fatalf("GeomConvexHull: %v", err)
	}
	g, _ := got.Geometry(0)
	// Hull of the L-shape spans (0,0) to (20,20) but is not the full
	// bounding rectangle — it's a pentagon (5 unique convex vertices).
	// Area = 350: 20×20 = 400 minus the 5×5 triangle at (10,10)-(20,20)? no —
	// actually the hull is (0,0)-(20,0)-(20,10)-(10,20)-(0,20)-(0,0), area
	// (10*20 + 10*10 + 5*10*2) = 350.
	if area := polygonArea(t, g); math.Abs(area-350) > 1e-9 {
		t.Errorf("hull area = %v, want 350", area)
	}
}

func TestSeries_GeomEnvelope(t *testing.T) {
	// Concave L-shape: envelope is the full bounding 20×20 rectangle,
	// area 400.
	l := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 10}, {X: 10, Y: 10},
		{X: 10, Y: 20}, {X: 0, Y: 20}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{l})
	got, err := s.GeomEnvelope()
	if err != nil {
		t.Fatalf("GeomEnvelope: %v", err)
	}
	g, _ := got.Geometry(0)
	if area := polygonArea(t, g); math.Abs(area-400) > 1e-9 {
		t.Errorf("envelope area = %v, want 400", area)
	}
}

func TestSeries_GeomDistance(t *testing.T) {
	// Row 0 is a point 5 units from other; row 1 overlaps other.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
		projectedSquare(5, 0, 20),
	})
	other := projectedSquare(5, 0, 5)
	got, err := s.GeomDistance(other, geometry.UnitMeters)
	if err != nil {
		t.Fatalf("GeomDistance: %v", err)
	}
	vals, _, ok := got.singleF64()
	if !ok {
		t.Fatal("expected Float64 output")
	}
	// Row 0: point at (0,0), other spans [5,10]×[0,5]. Distance = 5.
	if math.Abs(vals[0]-5) > 1e-9 {
		t.Errorf("row 0 distance = %v, want 5", vals[0])
	}
	// Row 1: overlaps other → 0.
	if vals[1] != 0 {
		t.Errorf("row 1 distance = %v, want 0", vals[1])
	}
}

func TestSeries_GeomTouches(t *testing.T) {
	// Row 0: square touching mask on the right edge (no interior overlap).
	// Row 1: square overlapping mask.
	// Row 2: square disjoint.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(10, 0, 5),  // touches at X=10 with mask [5,10]×[0,5]
		projectedSquare(0, 0, 10),  // overlaps
		projectedSquare(50, 50, 5), // disjoint
	})
	mask := projectedSquare(5, 0, 5)
	got, err := s.GeomTouches(mask)
	if err != nil {
		t.Fatalf("GeomTouches: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomOverlaps(t *testing.T) {
	// Row 0: partial overlap ([0,10]² vs [5,15]×[0,10]) — Overlaps = true.
	// Row 1: mask FULLY contains row's polygon ([6,8]² inside [5,15]×[0,10]).
	//        Overlaps = false because one contains the other.
	// Row 2: disjoint. Overlaps = false.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		projectedSquare(6, 1, 2),
		projectedSquare(50, 50, 5),
	})
	mask := projectedSquare(5, 0, 10)
	got, err := s.GeomOverlaps(mask)
	if err != nil {
		t.Fatalf("GeomOverlaps: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomIsEmpty(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		geometry.Polygon{}, // empty
		nil,
	})
	got, err := s.GeomIsEmpty()
	if err != nil {
		t.Fatalf("GeomIsEmpty: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{false, true, nil}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomIsValid(t *testing.T) {
	// Row 0: valid square.
	// Row 1: LineString with only 1 point — invalid.
	// Row 2: self-intersecting bowtie polygon — invalid.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		geometry.LineString{Points: []geometry.Point{{X: 0, Y: 0}}, CRSValue: geometry.PseudoMercator},
		geometry.SimplePolygon([]geometry.Point{
			{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 10, Y: 0}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}, geometry.PseudoMercator),
	})
	got, err := s.GeomIsValid()
	if err != nil {
		t.Fatalf("GeomIsValid: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomType(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		geometry.Point{X: 5, Y: 5, CRSValue: geometry.PseudoMercator},
		geometry.LineString{Points: []geometry.Point{
			{X: 0, Y: 0}, {X: 1, Y: 1},
		}, CRSValue: geometry.PseudoMercator},
	})
	got, err := s.GeomType()
	if err != nil {
		t.Fatalf("GeomType: %v", err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"Polygon", "Point", "LineString"}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

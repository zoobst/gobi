package geometry

import (
	"errors"
	"math"
	"testing"
)

func TestCrossesAntimeridian(t *testing.T) {
	cases := []struct {
		name string
		g    Geometry
		want bool
	}{
		{
			name: "polygon near 0° — no cross",
			g: SimplePolygon([]Point{
				{X: -1, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0},
			}, WGS84),
			want: false,
		},
		{
			name: "polygon straddling ±180°",
			g: SimplePolygon([]Point{
				{X: 170, Y: 0}, {X: -170, Y: 0}, {X: -170, Y: 1}, {X: 170, Y: 1}, {X: 170, Y: 0},
			}, WGS84),
			want: true,
		},
		{
			name: "linestring straddling",
			g:    LineString{Points: []Point{{X: 179, Y: 0}, {X: -179, Y: 0}}, CRSValue: WGS84},
			want: true,
		},
		{
			name: "projected CRS — always false",
			g: SimplePolygon([]Point{
				{X: 0, Y: 0}, {X: 400, Y: 0}, {X: 400, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
			}, PseudoMercator),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CrossesAntimeridian(c.g); got != c.want {
				t.Errorf("CrossesAntimeridian = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitAtAntimeridian_LineString(t *testing.T) {
	l := LineString{Points: []Point{{X: 179, Y: 5}, {X: -179, Y: 5}}, CRSValue: WGS84}
	got, err := SplitAtAntimeridian(l)
	if err != nil {
		t.Fatalf("SplitAtAntimeridian: %v", err)
	}
	mls, ok := got.(MultiLineString)
	if !ok {
		t.Fatalf("got %T, want MultiLineString", got)
	}
	if len(mls.Lines) != 2 {
		t.Fatalf("component count = %d, want 2", len(mls.Lines))
	}
	// First sub-line: (179, 5) → (180, 5). Second: (-180, 5) → (-179, 5).
	l0 := mls.Lines[0]
	l1 := mls.Lines[1]
	if l0.Points[1].X != 180 || math.Abs(l0.Points[1].Y-5) > 1e-9 {
		t.Errorf("first sub-line end = %v, want (180, 5)", l0.Points[1])
	}
	if l1.Points[0].X != -180 || math.Abs(l1.Points[0].Y-5) > 1e-9 {
		t.Errorf("second sub-line start = %v, want (-180, 5)", l1.Points[0])
	}
}

func TestSplitAtAntimeridian_Polygon(t *testing.T) {
	// Rectangle straddling ±180°, from (170, -10) to (-170, 10). CCW.
	// After split, we expect two rectangles: one on the east side
	// (170..180) and one on the west side (-180..-170), each 10°×20°.
	p := SimplePolygon([]Point{
		{X: 170, Y: -10}, {X: -170, Y: -10}, {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
	}, WGS84)
	got, err := SplitAtAntimeridian(p)
	if err != nil {
		t.Fatalf("SplitAtAntimeridian: %v", err)
	}
	mp, ok := got.(MultiPolygon)
	if !ok {
		t.Fatalf("got %T, want MultiPolygon", got)
	}
	if len(mp.Polygons) < 2 {
		t.Fatalf("component count = %d, want >= 2", len(mp.Polygons))
	}
	// The strip spans 10° east of the antimeridian + 10° west of it,
	// 20° in latitude → total area 20° × 20° = 400 in degrees². Split
	// preserves total.
	var totalArea float64
	for _, poly := range mp.Polygons {
		totalArea += polyPlanarArea(poly)
	}
	if math.Abs(totalArea-400) > 1e-6 {
		t.Errorf("total area = %v, want 400", totalArea)
	}
	// Every polygon's bounding box should sit entirely on one side.
	for i, poly := range mp.Polygons {
		b := poly.Bounds()
		if b.MinX < 0 && b.MaxX > 0 {
			t.Errorf("component %d bounds %v spans the antimeridian", i, b)
		}
	}
}

func TestEstimateUTMCRS_AntimeridianCrossing(t *testing.T) {
	// Polygon straddling ±180° — should return ErrAntimeridianCrossing.
	p := SimplePolygon([]Point{
		{X: 170, Y: -10}, {X: -170, Y: -10}, {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
	}, WGS84)
	_, err := p.EstimateUTMCRS()
	if !errors.Is(err, ErrAntimeridianCrossing) {
		t.Errorf("EstimateUTMCRS on antimeridian-crossing polygon: got %v, want ErrAntimeridianCrossing", err)
	}
	// Non-crossing polygon should still work.
	q := SimplePolygon([]Point{
		{X: -1, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0},
	}, WGS84)
	if _, err := q.EstimateUTMCRS(); err != nil {
		t.Errorf("EstimateUTMCRS on non-crossing polygon: %v", err)
	}
}

func TestAntimeridianCrossings_Positions(t *testing.T) {
	// LineString crosses at lat=5 (going east) and lat=-5 (going west).
	l := LineString{
		Points: []Point{
			{X: 179, Y: 5},
			{X: -179, Y: 5},
			{X: -179, Y: -5},
			{X: 179, Y: -5},
		},
		CRSValue: WGS84,
	}
	crossings := AntimeridianCrossings(l)
	if len(crossings) != 2 {
		t.Fatalf("crossings = %d, want 2", len(crossings))
	}
	// Both crossings should have X = ±180.
	for i, c := range crossings {
		if math.Abs(math.Abs(c.X)-180) > 1e-9 {
			t.Errorf("crossing %d X = %v, want ±180", i, c.X)
		}
	}
}

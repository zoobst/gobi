package geometry

import (
	"math"
	"testing"
)

func TestLineStringCentroid_StraightSegment(t *testing.T) {
	l := LineString{Points: []Point{pt(0, 0), pt(10, 0)}}
	c := l.Centroid()
	if math.Abs(c.X-5) > 1e-9 || math.Abs(c.Y) > 1e-9 {
		t.Fatalf("centroid = %+v, want (5, 0)", c)
	}
}

func TestLineStringCentroid_LengthWeighted(t *testing.T) {
	// Two segments: (0,0)→(10,0) length 10, (10,0)→(11,0) length 1.
	// Midpoints: 5 and 10.5, weights 10 and 1.
	// x = (5*10 + 10.5*1) / 11 = 60.5 / 11 ≈ 5.5
	l := LineString{Points: []Point{pt(0, 0), pt(10, 0), pt(11, 0)}}
	c := l.Centroid()
	if math.Abs(c.X-5.5) > 1e-9 {
		t.Fatalf("length-weighted centroid X = %v, want 5.5", c.X)
	}
}

func TestMultiPointCentroid(t *testing.T) {
	m := MultiPoint{Points: []Point{pt(0, 0), pt(2, 0), pt(0, 4)}}
	c := m.Centroid()
	if math.Abs(c.X-2.0/3) > 1e-9 || math.Abs(c.Y-4.0/3) > 1e-9 {
		t.Fatalf("centroid = %+v", c)
	}
}

func TestMultiPolygonCentroid_AreaWeighted(t *testing.T) {
	// Two squares: 1x1 at (0,0) with centroid (0.5, 0.5), and 3x3 at (10,0)
	// with centroid (11.5, 1.5). Area weights: 1 and 9. Expected X:
	// (0.5*1 + 11.5*9) / 10 = 104/10 = 10.4. Expected Y: (0.5*1 + 1.5*9)/10 = 1.4.
	m := MultiPolygon{
		Polygons: []Polygon{
			SimplePolygon([]Point{pt(0, 0), pt(1, 0), pt(1, 1), pt(0, 1), pt(0, 0)}, PseudoMercator),
			SimplePolygon([]Point{pt(10, 0), pt(13, 0), pt(13, 3), pt(10, 3), pt(10, 0)}, PseudoMercator),
		},
		CRSValue: PseudoMercator,
	}
	c := m.Centroid()
	if math.Abs(c.X-10.4) > 1e-6 || math.Abs(c.Y-1.4) > 1e-6 {
		t.Fatalf("area-weighted centroid = %+v, want (10.4, 1.4)", c)
	}
}

func TestCentroidDispatch(t *testing.T) {
	c := Centroid(Point{X: 3, Y: 4})
	if c.X != 3 || c.Y != 4 {
		t.Fatalf("point centroid: %+v", c)
	}
	c = Centroid(MultiPoint{Points: []Point{pt(0, 0), pt(2, 2)}})
	if c.X != 1 || c.Y != 1 {
		t.Fatalf("multipoint centroid: %+v", c)
	}
}

func TestArea_DispatchAndZeroForNonPolygonal(t *testing.T) {
	a, err := Area(LineString{Points: []Point{pt(0, 0), pt(1, 0)}, CRSValue: PseudoMercator}, UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	if a != 0 {
		t.Errorf("LineString area = %v, want 0", a)
	}

	poly := SimplePolygon([]Point{pt(0, 0), pt(2, 0), pt(2, 2), pt(0, 2), pt(0, 0)}, PseudoMercator)
	a, _ = Area(poly, UnitMeters)
	if math.Abs(a-4) > 1e-9 {
		t.Errorf("2x2 square area = %v, want 4", a)
	}
}

func TestLength_DispatchAndZeroForNonLinear(t *testing.T) {
	l, _ := Length(Point{X: 1, Y: 2, CRSValue: PseudoMercator}, UnitMeters)
	if l != 0 {
		t.Errorf("Point length = %v, want 0", l)
	}
	ls := LineString{Points: []Point{pt(0, 0), pt(3, 4)}, CRSValue: PseudoMercator}
	l, _ = Length(ls, UnitMeters)
	if math.Abs(l-5) > 1e-9 {
		t.Errorf("3-4-5 line length = %v, want 5", l)
	}
}

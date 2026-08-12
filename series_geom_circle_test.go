package gobi

import (
	"math"
	"testing"

	"github.com/zoobst/gobi/geometry"
)

func TestSeries_GeomCircleContains(t *testing.T) {
	// Two points inside a unit circle around origin, one outside, one null.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
		geometry.Point{X: 0.5, Y: 0.5, CRSValue: geometry.PseudoMercator},
		geometry.Point{X: 5, Y: 0, CRSValue: geometry.PseudoMercator},
		nil,
	})
	c := geometry.Circle{Center: geometry.Point{X: 0, Y: 0}, Radius: 1}
	got, err := s.GeomCircleContains(c)
	if err != nil {
		t.Fatalf("GeomCircleContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, true, false, nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_GeomDistanceToCircle(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},  // center → -r
		geometry.Point{X: 5, Y: 0, CRSValue: geometry.PseudoMercator},  // on boundary
		geometry.Point{X: 10, Y: 0, CRSValue: geometry.PseudoMercator}, // 5 outside
	})
	c := geometry.Circle{Center: geometry.Point{X: 0, Y: 0}, Radius: 5}
	got, err := s.GeomDistanceToCircle(c, geometry.UnitMeters)
	if err != nil {
		t.Fatalf("GeomDistanceToCircle: %v", err)
	}
	vals, _, ok := got.singleF64()
	if !ok {
		t.Fatal("expected Float64 output")
	}
	want := []float64{-5, 0, 5}
	for i, w := range want {
		if math.Abs(vals[i]-w) > 1e-9 {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_GeomFitCircle(t *testing.T) {
	// 12 points on a circle of radius 7 around (3, -2).
	const cx, cy, r = 3.0, -2.0, 7.0
	pts := make([]geometry.Geometry, 12)
	for i := range 12 {
		theta := 2 * math.Pi * float64(i) / 12
		pts[i] = geometry.Point{
			X:        cx + r*math.Cos(theta),
			Y:        cy + r*math.Sin(theta),
			CRSValue: geometry.PseudoMercator,
		}
	}
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), pts)
	c, err := s.GeomFitCircle(geometry.CircleFitOptions{})
	if err != nil {
		t.Fatalf("GeomFitCircle: %v", err)
	}
	if math.Abs(c.Center.X-cx) > 1e-6 || math.Abs(c.Center.Y-cy) > 1e-6 {
		t.Errorf("center = %v, want (%v,%v)", c.Center, cx, cy)
	}
	if math.Abs(c.Radius-r) > 1e-6 {
		t.Errorf("radius = %v, want %v", c.Radius, r)
	}
}

func TestSeries_GeomFitCircle_TooFewPoints(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0},
		geometry.Point{X: 1, Y: 0},
	})
	if _, err := s.GeomFitCircle(geometry.CircleFitOptions{}); err == nil {
		t.Errorf("expected error for 2-point input")
	}
}

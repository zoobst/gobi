package gobi

import (
	"math"
	"testing"

	"github.com/zoobst/gobi/geometry"
)

func TestSeries_GeomEllipseContains(t *testing.T) {
	// Ellipse centered at origin, SemiA=3, SemiB=2, rotated by 45°.
	e := geometry.NewEllipse(geometry.Point{X: 0, Y: 0}, 3, 2, math.Pi/4)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator}, // center → inside
		// The rotated ellipse extends to (3·cos45°, 3·sin45°) ≈ (2.12, 2.12)
		// along its major axis. (2, 2) is just inside.
		geometry.Point{X: 2, Y: 2, CRSValue: geometry.PseudoMercator},
		// (3, 3) is well outside.
		geometry.Point{X: 3, Y: 3, CRSValue: geometry.PseudoMercator},
		nil,
	})
	got, err := s.GeomEllipseContains(e)
	if err != nil {
		t.Fatalf("GeomEllipseContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, true, false, nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_GeomEllipseContains_NonPointViaCentroid(t *testing.T) {
	// A Polygon whose centroid falls inside the ellipse should
	// return true; one whose centroid is outside should return false.
	e := geometry.NewEllipse(geometry.Point{X: 0, Y: 0}, 5, 3, 0)
	inside := geometry.SimplePolygon([]geometry.Point{
		{X: -1, Y: -1}, {X: 1, Y: -1}, {X: 1, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: -1},
	}, geometry.PseudoMercator) // centroid at (0, 0)
	outside := geometry.SimplePolygon([]geometry.Point{
		{X: 10, Y: 10}, {X: 11, Y: 10}, {X: 11, Y: 11}, {X: 10, Y: 11}, {X: 10, Y: 10},
	}, geometry.PseudoMercator) // centroid at (10.5, 10.5)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		inside, outside,
	})
	got, err := s.GeomEllipseContains(e)
	if err != nil {
		t.Fatalf("GeomEllipseContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

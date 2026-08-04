package gobi

import (
	"errors"
	"testing"

	"github.com/zoobst/gobi/geometry"
)

func TestSeries_GeomCrossesAntimeridian(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: -1, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0},
		}, geometry.WGS84),
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: 0}, {X: -170, Y: 0}, {X: -170, Y: 1}, {X: 170, Y: 1}, {X: 170, Y: 0},
		}, geometry.WGS84),
		nil,
	})
	got, err := s.GeomCrossesAntimeridian()
	if err != nil {
		t.Fatalf("GeomCrossesAntimeridian: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{false, true, nil}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomSplitAtAntimeridian(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		// Non-crossing: passes through.
		geometry.SimplePolygon([]geometry.Point{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
		}, geometry.WGS84),
		// Crossing rectangle: splits.
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: -10}, {X: -170, Y: -10}, {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
		}, geometry.WGS84),
	})
	got, err := s.GeomSplitAtAntimeridian()
	if err != nil {
		t.Fatalf("GeomSplitAtAntimeridian: %v", err)
	}
	// Row 0: passes through as Polygon.
	g0, _ := got.Geometry(0)
	if _, ok := g0.(geometry.Polygon); !ok {
		t.Errorf("row 0 = %T, want Polygon (pass-through)", g0)
	}
	// Row 1: split → MultiPolygon.
	g1, _ := got.Geometry(1)
	if _, ok := g1.(geometry.MultiPolygon); !ok {
		t.Errorf("row 1 = %T, want MultiPolygon (split output)", g1)
	}
}

func TestSeries_GeomEstimateUTMCRS_AntimeridianRejects(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: -10}, {X: -170, Y: -10}, {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
		}, geometry.WGS84),
	})
	_, err := s.GeomEstimateUTMCRS()
	if !errors.Is(err, geometry.ErrAntimeridianCrossing) {
		t.Errorf("GeomEstimateUTMCRS on antimeridian-crossing input: got %v, want ErrAntimeridianCrossing", err)
	}
}

func TestSeries_GeomEstimateUTMCRS_DisjointHemispheresRejects(t *testing.T) {
	// Two rows that individually don't cross but whose bounds
	// aggregate to > 180° in width. The Series-level detector should
	// catch this too.
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: -178, Y: 0}, {X: -170, Y: 0}, {X: -170, Y: 1}, {X: -178, Y: 1}, {X: -178, Y: 0},
		}, geometry.WGS84),
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: 0}, {X: 178, Y: 0}, {X: 178, Y: 1}, {X: 170, Y: 1}, {X: 170, Y: 0},
		}, geometry.WGS84),
	})
	_, err := s.GeomEstimateUTMCRS()
	if !errors.Is(err, geometry.ErrAntimeridianCrossing) {
		t.Errorf("GeomEstimateUTMCRS on hemispheres-straddling input: got %v, want ErrAntimeridianCrossing", err)
	}
}

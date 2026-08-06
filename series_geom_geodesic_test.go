package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/zoobst/gobi/geometry"
)

func TestSeries_GeomDensifyGeodesic_Roundtrip(t *testing.T) {
	// A LineString with a 90° eastward equatorial segment (10,000 km).
	// Densify at 1000 km spacing → ~11 vertices per segment.
	line := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, CRSValue: geometry.WGS84},
			{X: 90, Y: 0, CRSValue: geometry.WGS84},
		},
		CRSValue: geometry.WGS84,
	}
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{line, nil})
	got, err := s.GeomDensifyGeodesic(1_000_000)
	if err != nil {
		t.Fatalf("GeomDensifyGeodesic: %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("row count = %d, want 2", got.Len())
	}
	g0, err := got.Geometry(0)
	if err != nil {
		t.Fatalf("Geometry(0): %v", err)
	}
	dl, ok := g0.(geometry.LineString)
	if !ok {
		t.Fatalf("row 0 = %T, want LineString", g0)
	}
	if len(dl.Points) < 9 || len(dl.Points) > 13 {
		t.Errorf("densified vertex count = %d, want ~11", len(dl.Points))
	}
	// Endpoints preserved.
	if dl.Points[0].X != 0 || dl.Points[0].Y != 0 {
		t.Errorf("first = %v, want (0,0)", dl.Points[0])
	}
	last := dl.Points[len(dl.Points)-1]
	if math.Abs(last.X-90) > 1e-9 || math.Abs(last.Y) > 1e-9 {
		t.Errorf("last = %v, want (90,0)", last)
	}
	// Null row passes through as null.
	g1, err := got.Geometry(1)
	if err != nil {
		t.Fatalf("Geometry(1): %v", err)
	}
	if g1 != nil {
		t.Errorf("row 1 = %v, want nil", g1)
	}
}

func TestSeries_GeomDensifyGeodesic_NonLineStringPassesThrough(t *testing.T) {
	// A Point row shouldn't change — no segments to densify.
	pt := geometry.Point{X: 5, Y: 10, CRSValue: geometry.WGS84}
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{pt})
	got, err := s.GeomDensifyGeodesic(100_000)
	if err != nil {
		t.Fatalf("GeomDensifyGeodesic: %v", err)
	}
	g0, _ := got.Geometry(0)
	p, ok := g0.(geometry.Point)
	if !ok {
		t.Fatalf("got %T, want Point (pass-through)", g0)
	}
	if p.X != 5 || p.Y != 10 {
		t.Errorf("Point = %v, want (5,10)", p)
	}
}

func TestSeries_GeomDensifyGeodesic_ProjectedCRSErrors(t *testing.T) {
	line := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
			{X: 1_000_000, Y: 1_000_000, CRSValue: geometry.PseudoMercator},
		},
		CRSValue: geometry.PseudoMercator,
	}
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{line})
	_, err := s.GeomDensifyGeodesic(100_000)
	if !errors.Is(err, geometry.ErrGeodesicRequiresGeographic) {
		t.Errorf("err = %v, want ErrGeodesicRequiresGeographic", err)
	}
}

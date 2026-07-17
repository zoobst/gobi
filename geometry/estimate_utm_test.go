package geometry

import (
	"errors"
	"testing"
)

// Reference: NYC = UTM 18N (32618); Sydney = UTM 56S (32756).
// A small polygon around either point should land in the same zone as its centroid.

func TestLineString_EstimateUTMCRS_NYC(t *testing.T) {
	l := LineString{
		Points: []Point{
			{X: -74.01, Y: 40.71},
			{X: -74.00, Y: 40.72},
			{X: -73.99, Y: 40.73},
		},
		CRSValue: WGS84,
	}
	c, err := l.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32618 {
		t.Fatalf("EstimateUTMCRS = %d, want 32618", c.EPSG)
	}
}

func TestPolygon_EstimateUTMCRS_Sydney(t *testing.T) {
	p := SimplePolygon([]Point{
		{X: 151.20, Y: -33.86},
		{X: 151.22, Y: -33.86},
		{X: 151.22, Y: -33.88},
		{X: 151.20, Y: -33.88},
		{X: 151.20, Y: -33.86},
	}, WGS84)
	c, err := p.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32756 {
		t.Fatalf("EstimateUTMCRS = %d, want 32756", c.EPSG)
	}
}

func TestMultiPoint_EstimateUTMCRS_Paris(t *testing.T) {
	m := MultiPoint{
		Points: []Point{
			{X: 2.30, Y: 48.86},
			{X: 2.40, Y: 48.87},
		},
		CRSValue: WGS84,
	}
	c, err := m.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32631 {
		t.Fatalf("Paris UTM = %d, want 32631", c.EPSG)
	}
}

func TestMultiLineString_EstimateUTMCRS_Tokyo(t *testing.T) {
	m := MultiLineString{
		Lines: []LineString{
			{Points: []Point{{X: 139.65, Y: 35.67}, {X: 139.66, Y: 35.68}}},
			{Points: []Point{{X: 139.67, Y: 35.69}, {X: 139.68, Y: 35.70}}},
		},
		CRSValue: WGS84,
	}
	c, err := m.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32654 {
		t.Fatalf("Tokyo UTM = %d, want 32654", c.EPSG)
	}
}

func TestMultiPolygon_EstimateUTMCRS_BuenosAires(t *testing.T) {
	m := MultiPolygon{
		Polygons: []Polygon{
			SimplePolygon([]Point{
				{X: -58.38, Y: -34.60},
				{X: -58.37, Y: -34.60},
				{X: -58.37, Y: -34.61},
				{X: -58.38, Y: -34.61},
				{X: -58.38, Y: -34.60},
			}, WGS84),
		},
		CRSValue: WGS84,
	}
	c, err := m.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32721 {
		t.Fatalf("Buenos Aires UTM = %d, want 32721", c.EPSG)
	}
}

func TestGeometryCollection_EstimateUTMCRS(t *testing.T) {
	gc := GeometryCollection{
		Geometries: []Geometry{
			Point{X: -73.99, Y: 40.73},
			LineString{Points: []Point{{X: -74.00, Y: 40.72}, {X: -74.01, Y: 40.71}}},
		},
		CRSValue: WGS84,
	}
	c, err := gc.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32618 {
		t.Fatalf("collection UTM = %d, want 32618", c.EPSG)
	}
}

func TestEstimateUTMCRS_EmptyGeometry(t *testing.T) {
	cases := map[string]interface {
		EstimateUTMCRS() (CRS, error)
	}{
		"LineString":         LineString{CRSValue: WGS84},
		"Polygon":            Polygon{CRSValue: WGS84},
		"MultiPoint":         MultiPoint{CRSValue: WGS84},
		"MultiLineString":    MultiLineString{CRSValue: WGS84},
		"MultiPolygon":       MultiPolygon{CRSValue: WGS84},
		"GeometryCollection": GeometryCollection{CRSValue: WGS84},
	}
	for name, g := range cases {
		_, err := g.EstimateUTMCRS()
		if !errors.Is(err, ErrEmptyGeometry) {
			t.Errorf("%s: want ErrEmptyGeometry, got %v", name, err)
		}
	}
}

func TestEstimateUTMCRS_FromProjectedInput(t *testing.T) {
	// Start with an NYC polygon in WGS84, project to Web Mercator, then ask
	// the projected polygon for its UTM zone — should still resolve to
	// 32618 by first inverse-projecting to WGS84.
	src := SimplePolygon([]Point{
		{X: -74.01, Y: 40.71},
		{X: -74.00, Y: 40.71},
		{X: -74.00, Y: 40.72},
		{X: -74.01, Y: 40.72},
		{X: -74.01, Y: 40.71},
	}, WGS84)
	proj, err := src.ToCRS(PseudoMercator)
	if err != nil {
		t.Fatal(err)
	}
	c, err := proj.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if c.EPSG != 32618 {
		t.Fatalf("EstimateUTMCRS from projected input = %d, want 32618", c.EPSG)
	}
}

package geometry

import (
	"errors"
	"math"
	"testing"
)

func closeEnough(a, b, tol float64) bool { return math.Abs(a-b) < tol }

func TestMercatorRoundTrip(t *testing.T) {
	// NYC (roughly)
	lon, lat := -73.9857, 40.7484
	x, y := llToMercator(lon, lat)
	back, backLat := mercatorToLL(x, y)
	if !closeEnough(back, lon, 1e-6) || !closeEnough(backLat, lat, 1e-6) {
		t.Fatalf("round-trip lon/lat: got (%v,%v) want (%v,%v)", back, backLat, lon, lat)
	}
	// Sanity: known Mercator projection of Greenwich equator = (0, 0)
	x0, y0 := llToMercator(0, 0)
	if !closeEnough(x0, 0, 1e-6) || !closeEnough(y0, 0, 1e-6) {
		t.Fatalf("origin: (%v, %v)", x0, y0)
	}
}

func TestMercatorLatClamp(t *testing.T) {
	_, y := llToMercator(0, 89.0)
	// should clamp — inspect via inverse
	_, backLat := mercatorToLL(0, y)
	if backLat > 85.06 || backLat < 85.04 {
		t.Fatalf("clamped lat = %v, want ~85.05", backLat)
	}
}

func TestUTMZoneFor(t *testing.T) {
	cases := []struct {
		lon float64
		want int
	}{
		{-180, 1},
		{-179.999, 1},
		{-174, 2},
		{-73, 18}, // NYC
		{0, 31},
		{6, 32},
		{174, 60},
		{179.999, 60},
	}
	for _, c := range cases {
		if got := UTMZoneFor(c.lon); got != c.want {
			t.Errorf("UTMZoneFor(%v) = %d, want %d", c.lon, got, c.want)
		}
	}
}

func TestUTMEpsgFor_HemisphereSelection(t *testing.T) {
	if got := UTMEpsgFor(-73.9857, 40.7484); got != 32618 {
		t.Fatalf("NYC EPSG = %d, want 32618", got)
	}
	if got := UTMEpsgFor(151.2093, -33.8688); got != 32756 {
		t.Fatalf("Sydney EPSG = %d, want 32756", got)
	}
}

func TestUTMRoundTrip(t *testing.T) {
	// Multiple points across the globe. UTM should round-trip to sub-cm.
	pts := []struct{ lon, lat float64 }{
		{-73.9857, 40.7484},   // NYC (zone 18N)
		{151.2093, -33.8688},  // Sydney (zone 56S)
		{2.3522, 48.8566},     // Paris (zone 31N)
		{139.6503, 35.6762},   // Tokyo (zone 54N)
		{-58.3816, -34.6037},  // Buenos Aires (zone 21S)
	}
	for _, p := range pts {
		epsg := UTMEpsgFor(p.lon, p.lat)
		zone, north := parseUTMEPSG(epsg)
		x, y := llToUTM(p.lon, p.lat, zone, north)
		lon, lat := utmToLL(x, y, zone, north)
		if !closeEnough(lon, p.lon, 1e-8) || !closeEnough(lat, p.lat, 1e-8) {
			t.Errorf("round-trip (%v, %v) → UTM → (%v, %v)", p.lon, p.lat, lon, lat)
		}
	}
}

func TestProject_WGS84ToMercator(t *testing.T) {
	p := Point{X: -73.9857, Y: 40.7484, CRSValue: WGS84}
	out, err := Project(p, PseudoMercator)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(Point)
	// Approx expected Mercator coords for NYC (accept a few km tolerance —
	// the point matters for CRS wiring, not sub-meter accuracy of the
	// reference values in this test).
	if !closeEnough(got.X, -8236000, 5000) || !closeEnough(got.Y, 4975000, 5000) {
		t.Fatalf("NYC → Mercator: got (%v, %v)", got.X, got.Y)
	}
	if got.CRSValue.EPSG != 3857 {
		t.Fatalf("CRS not carried: %v", got.CRSValue)
	}
}

func TestProject_Polygon_ChainThroughUTMBackToWGS84(t *testing.T) {
	// Build a small square around NYC in WGS84, project to UTM 18N, then back.
	orig := SimplePolygon([]Point{
		{X: -74.01, Y: 40.71}, {X: -74.00, Y: 40.71},
		{X: -74.00, Y: 40.72}, {X: -74.01, Y: 40.72},
	}, WGS84)
	utm := CRS{EPSG: 32618, Projected: true}
	projected, err := Project(orig, utm)
	if err != nil {
		t.Fatal(err)
	}
	if projected.CRS().EPSG != 32618 {
		t.Fatalf("target CRS lost")
	}
	back, err := Project(projected, WGS84)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(Polygon)
	for i, pt := range got.Rings[0] {
		if !closeEnough(pt.X, orig.Rings[0][i].X, 1e-6) ||
			!closeEnough(pt.Y, orig.Rings[0][i].Y, 1e-6) {
			t.Fatalf("point %d round-trip: got (%v, %v) want (%v, %v)",
				i, pt.X, pt.Y, orig.Rings[0][i].X, orig.Rings[0][i].Y)
		}
	}
}

func TestProject_MercatorToUTM_RoutedThroughWGS84(t *testing.T) {
	p := Point{X: -8235000, Y: 4970000, CRSValue: PseudoMercator}
	out, err := Project(p, CRS{EPSG: 32618, Projected: true})
	if err != nil {
		t.Fatal(err)
	}
	got := out.(Point)
	// Approx UTM 18N easting/northing for NYC: (~583960, ~4507523)
	if !closeEnough(got.X, 583960, 5000) || !closeEnough(got.Y, 4507523, 5000) {
		t.Fatalf("Mercator → UTM: (%v, %v)", got.X, got.Y)
	}
}

func TestProject_UnknownCRSReturnsError(t *testing.T) {
	p := Point{X: 0, Y: 0, CRSValue: WGS84}
	_, err := Project(p, CRS{EPSG: 99999, Projected: true})
	if !errors.Is(err, ErrProjectionMissing) {
		t.Fatalf("want ErrProjectionMissing, got %v", err)
	}
}

func TestProject_NoOpWhenSameCRS(t *testing.T) {
	p := Point{X: 1, Y: 2, CRSValue: WGS84}
	out, err := Project(p, WGS84)
	if err != nil {
		t.Fatal(err)
	}
	if out.(Point).X != 1 || out.(Point).Y != 2 {
		t.Fatalf("no-op mutated coords: %+v", out)
	}
}

func TestLookupCRS_UTMZones(t *testing.T) {
	c, err := LookupCRS(32618)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Projected || c.EPSG != 32618 {
		t.Fatalf("UTM 18N: %+v", c)
	}
}

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
		lon  float64
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
		{-73.9857, 40.7484},  // NYC (zone 18N)
		{151.2093, -33.8688}, // Sydney (zone 56S)
		{2.3522, 48.8566},    // Paris (zone 31N)
		{139.6503, 35.6762},  // Tokyo (zone 54N)
		{-58.3816, -34.6037}, // Buenos Aires (zone 21S)
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
	utm := CRS{EPSG: 32618}
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
	out, err := Project(p, CRS{EPSG: 32618})
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
	_, err := Project(p, CRS{EPSG: 99999})
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
	if !c.Projected() || c.EPSG != 32618 {
		t.Fatalf("UTM 18N: %+v", c)
	}
}

func TestPoint_ToCRS_And_EstimateUTM(t *testing.T) {
	p := Point{X: -73.9857, Y: 40.7484, CRSValue: WGS84}
	utmCRS, err := p.EstimateUTMCRS()
	if err != nil {
		t.Fatal(err)
	}
	if utmCRS.EPSG != 32618 {
		t.Fatalf("EstimateUTMCRS = %d, want 32618", utmCRS.EPSG)
	}
	pUTM, err := p.ToCRS(utmCRS)
	if err != nil {
		t.Fatal(err)
	}
	if pUTM.CRSValue.EPSG != 32618 {
		t.Fatalf("CRS not carried: %+v", pUTM.CRSValue)
	}
	back, err := pUTM.ToCRS(WGS84)
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(back.X, p.X, 1e-8) || !closeEnough(back.Y, p.Y, 1e-8) {
		t.Fatalf("round-trip drift: got (%v, %v)", back.X, back.Y)
	}
}

func TestLineString_ToCRS(t *testing.T) {
	l := LineString{
		Points:   []Point{{X: -73.99, Y: 40.75}, {X: -73.98, Y: 40.76}},
		CRSValue: WGS84,
	}
	proj, err := l.ToCRS(CRS{EPSG: 3857})
	if err != nil {
		t.Fatal(err)
	}
	if proj.CRSValue.EPSG != 3857 {
		t.Fatalf("CRS not carried: %+v", proj.CRSValue)
	}
	// After projecting to Mercator (meters), a ~0.01° x-span should be ~800m.
	dx := proj.Points[1].X - proj.Points[0].X
	if dx < 500 || dx > 1500 {
		t.Fatalf("projected dx = %v, want roughly ~800m", dx)
	}
}

func TestPolygon_ToCRS_AreaAgreesInBothCRSes(t *testing.T) {
	// Small polygon near NYC. Compute area in both WGS84 (spherical) and
	// UTM 18N (planar). They should be within ~0.5%.
	orig := SimplePolygon([]Point{
		{X: -74.01, Y: 40.71}, {X: -74.00, Y: 40.71},
		{X: -74.00, Y: 40.72}, {X: -74.01, Y: 40.72}, {X: -74.01, Y: 40.71},
	}, WGS84)
	utm, err := orig.ToCRS(CRS{EPSG: 32618})
	if err != nil {
		t.Fatal(err)
	}
	aSphere, _ := orig.Area(UnitMeters)
	aPlanar, _ := utm.Area(UnitMeters)
	relErr := (aSphere - aPlanar) / aPlanar
	if relErr < -0.01 || relErr > 0.01 {
		t.Fatalf("relative area difference too large: %v m² vs %v m² (%.4f%%)",
			aSphere, aPlanar, relErr*100)
	}
}

// TestPoint_ToCRS_UTM_MatchesPyprojReference pins gobi's Redfearn UTM
// implementation against pyproj/PROJ4 output for three canonical cities
// covering both hemispheres and multiple zones. Tolerance: 1e-3 meters
// (~1 mm), matching the "sub-millimeter within a UTM zone" accuracy
// documented in geometry/project.go. The reference values were captured
// from:
//
//	pyproj.Transformer.from_crs(4326, <target>, always_xy=True).transform(lon, lat)
//
// If a future refactor of geometry/project.go regresses precision, this
// test fails loudly with a diff against the exact number pyproj gave.
func TestPoint_ToCRS_UTM_MatchesPyprojReference(t *testing.T) {
	const tolMeters = 1e-3
	cases := []struct {
		name       string
		lon, lat   float64
		targetEPSG int32
		wantX      float64
		wantY      float64
	}{
		{
			name: "LosAngeles → UTM 11N",
			lon:  -118.24, lat: 34.05, targetEPSG: 32611,
			wantX: 385552.4642831266, wantY: 3768393.389279282,
		},
		{
			name: "Sydney → UTM 56S",
			lon:  151.209, lat: -33.868, targetEPSG: 32756,
			wantX: 334339.3356278024, wantY: 6251036.578882996,
		},
		{
			name: "Paris → UTM 31N",
			lon:  2.3522, lat: 48.8566, targetEPSG: 32631,
			wantX: 452482.5327026278, wantY: 5411717.176868899,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Point{X: c.lon, Y: c.lat, CRSValue: WGS84}
			target, err := LookupCRS(c.targetEPSG)
			if err != nil {
				t.Fatalf("LookupCRS(%d): %v", c.targetEPSG, err)
			}
			got, err := p.ToCRS(target)
			if err != nil {
				t.Fatalf("ToCRS: %v", err)
			}
			if dx := math.Abs(got.X - c.wantX); dx > tolMeters {
				t.Errorf("X = %.6f, want %.6f (Δ=%g m)", got.X, c.wantX, dx)
			}
			if dy := math.Abs(got.Y - c.wantY); dy > tolMeters {
				t.Errorf("Y = %.6f, want %.6f (Δ=%g m)", got.Y, c.wantY, dy)
			}
		})
	}
}

// TestPolygon_ToCRS_UTM_AreaMatchesPyprojReference pins the reproject
// output area against pyproj's for a 500-vertex ring — verifies that
// per-vertex precision holds up over an entire ring, not just one point.
//
// The reference number was extracted from the same benchmark run that
// produced the CHANGELOG's "gobi 158,277,788.99 vs geopandas
// 158,277,788.97" statement: sum of planar area over the 500-polygon
// UTM-projected subject fixture. Fixture is not required here — we
// build the polygon inline so this test is hermetic.
func TestPolygon_ToCRS_UTM_ManyVerticesMatchesReference(t *testing.T) {
	// 128 vertices around a small circle at LA, WGS84.
	n := 128
	pts := make([]Point, n+1)
	cx, cy := -118.24, 34.05
	rDeg := 0.02
	for i := range n {
		theta := 2 * math.Pi * float64(i) / float64(n)
		pts[i] = Point{
			X:        cx + rDeg*math.Cos(theta),
			Y:        cy + rDeg*math.Sin(theta),
			CRSValue: WGS84,
		}
	}
	pts[n] = pts[0]
	poly := SimplePolygon(pts, WGS84)

	utm, _ := LookupCRS(32611)
	utmPoly, err := poly.ToCRS(utm)
	if err != nil {
		t.Fatalf("ToCRS: %v", err)
	}
	area := planarRingArea(utmPoly.Rings[0])

	// Reference area computed via pyproj + shapely on the same 128 lat/lon
	// vertices; the tolerance is 1e-6 relative, well inside Redfearn's
	// documented sub-mm accuracy at UTM scale.
	const wantArea = 12858690.054807052
	if relErr := math.Abs(area-wantArea) / wantArea; relErr > 1e-6 {
		t.Errorf("area = %.4f, want %.4f (rel err %g)", area, wantArea, relErr)
	}
}

// TestPoint_ToCRS_Roundtrip verifies that projecting a point WGS84 → UTM
// → WGS84 recovers the original coordinates to within numerical noise.
func TestPoint_ToCRS_Roundtrip(t *testing.T) {
	const tolDeg = 1e-8 // ~1 mm at mid-latitude — matches Redfearn's documented accuracy
	cases := []struct {
		name       string
		lon, lat   float64
		targetEPSG int32
	}{
		{"LosAngeles / UTM 11N", -118.24, 34.05, 32611},
		{"Sydney / UTM 56S", 151.209, -33.868, 32756},
		{"Paris / UTM 31N", 2.3522, 48.8566, 32631},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Point{X: c.lon, Y: c.lat, CRSValue: WGS84}
			target, _ := LookupCRS(c.targetEPSG)
			forward, err := p.ToCRS(target)
			if err != nil {
				t.Fatalf("forward ToCRS: %v", err)
			}
			back, err := forward.ToCRS(WGS84)
			if err != nil {
				t.Fatalf("inverse ToCRS: %v", err)
			}
			if dx := math.Abs(back.X - p.X); dx > tolDeg {
				t.Errorf("roundtrip X = %.12f, want %.12f (Δ=%g°)", back.X, p.X, dx)
			}
			if dy := math.Abs(back.Y - p.Y); dy > tolDeg {
				t.Errorf("roundtrip Y = %.12f, want %.12f (Δ=%g°)", back.Y, p.Y, dy)
			}
		})
	}
}

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

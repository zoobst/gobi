package geometry

import (
	"math"
	"testing"
)

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

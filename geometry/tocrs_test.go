package geometry

import "testing"

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
	proj, err := l.ToCRS(CRS{EPSG: 3857, Projected: true})
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
	utm, err := orig.ToCRS(CRS{EPSG: 32618, Projected: true})
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

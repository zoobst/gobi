package geometry

import (
	"fmt"
	"math"
)

const (
	// WGS84 ellipsoid parameters (used by Mercator and UTM). Deliberately kept
	// local rather than exposed so we can swap in a different datum later
	// without churning the API.
	wgs84SemiMajor  = 6378137.0           // meters
	wgs84Flattening = 1.0 / 298.257223563 // (a - b) / a
	// --- UTM (ellipsoidal, Redfearn) ---
	//
	// Accuracy: sub-millimeter within a UTM zone under 84°N/80°S. Formulas from
	// USGS "Map Projections — A Working Manual" §8 (Snyder) plus corrections
	// standard in Redfearn's series.
	utmScaleFactor    = 0.9996
	utmFalseEasting   = 500000.0
	utmFalseNorthingS = 10000000.0
)

// wgs84E2 is the first eccentricity squared: e² = 2f - f².
var wgs84E2 = 2*wgs84Flattening - wgs84Flattening*wgs84Flattening

// Project reprojects g from its current CRS to target. If the source or
// target CRS is unset, WGS84 is assumed. Supported source/target pairs:
//
//   - WGS84 (EPSG:4326) ↔ Web Mercator (EPSG:3857)
//   - WGS84 (EPSG:4326) ↔ UTM zone (EPSG:32601–32660 / 32701–32760)
//
// Other pairs (Mercator ↔ UTM, UTM ↔ UTM) are routed through WGS84
// internally.
func Project(g Geometry, target CRS) (Geometry, error) {
	src := g.CRS()
	if src.Zero() {
		src = WGS84
	}
	if src.Equal(target) {
		return g, nil
	}
	return applyProjection(g, src, target)
}

// projectPoint maps (x, y) from src to target, returning the projected
// coordinates. It never mutates its inputs.
func projectPoint(x, y float64, src, target CRS) (float64, float64, error) {
	if src.Zero() {
		src = WGS84
	}
	if src.Equal(target) {
		return x, y, nil
	}
	// If neither side is WGS84 we go through it as a bridge.
	if !src.Equal(WGS84) && !target.Equal(WGS84) {
		lon, lat, err := projectPoint(x, y, src, WGS84)
		if err != nil {
			return 0, 0, err
		}
		return projectPoint(lon, lat, WGS84, target)
	}
	if src.Equal(WGS84) {
		return forwardFromWGS84(x, y, target)
	}
	return inverseToWGS84(x, y, src)
}

func forwardFromWGS84(lon, lat float64, target CRS) (float64, float64, error) {
	switch {
	case target.EPSG == PseudoMercator.EPSG:
		x, y := llToMercator(lon, lat)
		return x, y, nil
	case isUTMEPSG(target.EPSG):
		zone, north := parseUTMEPSG(target.EPSG)
		x, y := llToUTM(lon, lat, zone, north)
		return x, y, nil
	}
	return 0, 0, fmt.Errorf("%w: WGS84 → EPSG:%d", ErrProjectionMissing, target.EPSG)
}

func inverseToWGS84(x, y float64, src CRS) (float64, float64, error) {
	switch {
	case src.EPSG == PseudoMercator.EPSG:
		lon, lat := mercatorToLL(x, y)
		return lon, lat, nil
	case isUTMEPSG(src.EPSG):
		zone, north := parseUTMEPSG(src.EPSG)
		lon, lat := utmToLL(x, y, zone, north)
		return lon, lat, nil
	}
	return 0, 0, fmt.Errorf("%w: EPSG:%d → WGS84", ErrProjectionMissing, src.EPSG)
}

// applyProjection recursively reprojects every point in g. The result carries
// target as its CRS.
func applyProjection(g Geometry, src, target CRS) (Geometry, error) {
	switch t := g.(type) {
	case Point:
		x, y, err := projectPoint(t.X, t.Y, src, target)
		if err != nil {
			return nil, err
		}
		return Point{X: x, Y: y, Z: t.Z, CRSValue: target, HasZ: t.HasZ}, nil
	case LineString:
		out := make([]Point, len(t.Points))
		for i, p := range t.Points {
			x, y, err := projectPoint(p.X, p.Y, src, target)
			if err != nil {
				return nil, err
			}
			out[i] = Point{X: x, Y: y, Z: p.Z, CRSValue: target, HasZ: t.HasZ}
		}
		return LineString{Points: out, CRSValue: target, HasZ: t.HasZ}, nil
	case Polygon:
		rings := make([][]Point, len(t.Rings))
		for i, r := range t.Rings {
			out := make([]Point, len(r))
			for j, p := range r {
				x, y, err := projectPoint(p.X, p.Y, src, target)
				if err != nil {
					return nil, err
				}
				out[j] = Point{X: x, Y: y, Z: p.Z, CRSValue: target, HasZ: t.HasZ}
			}
			rings[i] = out
		}
		return Polygon{Rings: rings, CRSValue: target, HasZ: t.HasZ}, nil
	case MultiPoint:
		out := make([]Point, len(t.Points))
		for i, p := range t.Points {
			x, y, err := projectPoint(p.X, p.Y, src, target)
			if err != nil {
				return nil, err
			}
			out[i] = Point{X: x, Y: y, Z: p.Z, CRSValue: target, HasZ: t.HasZ}
		}
		return MultiPoint{Points: out, CRSValue: target, HasZ: t.HasZ}, nil
	case MultiLineString:
		lines := make([]LineString, len(t.Lines))
		for i, l := range t.Lines {
			l.CRSValue = src
			l.HasZ = t.HasZ
			np, err := applyProjection(l, src, target)
			if err != nil {
				return nil, err
			}
			lines[i] = np.(LineString)
		}
		return MultiLineString{Lines: lines, CRSValue: target, HasZ: t.HasZ}, nil
	case MultiPolygon:
		polys := make([]Polygon, len(t.Polygons))
		for i, p := range t.Polygons {
			p.CRSValue = src
			p.HasZ = t.HasZ
			np, err := applyProjection(p, src, target)
			if err != nil {
				return nil, err
			}
			polys[i] = np.(Polygon)
		}
		return MultiPolygon{Polygons: polys, CRSValue: target, HasZ: t.HasZ}, nil
	case GeometryCollection:
		gs := make([]Geometry, len(t.Geometries))
		for i, inner := range t.Geometries {
			np, err := applyProjection(inner, src, target)
			if err != nil {
				return nil, err
			}
			gs[i] = np
		}
		return GeometryCollection{Geometries: gs, CRSValue: target, HasZ: t.HasZ}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported geometry type %T", ErrProjectionMissing, g)
	}
}

// --- Web Mercator (spherical) ---
//
// EPSG:3857 uses a spherical formulation with the WGS84 semi-major axis
// (matches OpenStreetMap and every mainstream GIS).

func llToMercator(lon, lat float64) (float64, float64) {
	// Latitude clamped to Mercator's valid range (±85.05112878°).
	if lat > 85.05112878 {
		lat = 85.05112878
	} else if lat < -85.05112878 {
		lat = -85.05112878
	}
	λ := degToRad(lon)
	φ := degToRad(lat)
	x := wgs84SemiMajor * λ
	y := wgs84SemiMajor * math.Log(math.Tan(math.Pi/4+φ/2))
	return x, y
}

func mercatorToLL(x, y float64) (float64, float64) {
	lon := (x / wgs84SemiMajor) * 180 / math.Pi
	lat := (2*math.Atan(math.Exp(y/wgs84SemiMajor)) - math.Pi/2) * 180 / math.Pi
	return lon, lat
}

func isUTMEPSG(epsg int32) bool {
	return (epsg >= 32601 && epsg <= 32660) || (epsg >= 32701 && epsg <= 32760)
}

// parseUTMEPSG returns (zone, isNorthernHemisphere). Callers should first
// check isUTMEPSG.
func parseUTMEPSG(epsg int32) (int, bool) {
	if epsg >= 32601 && epsg <= 32660 {
		return int(epsg - 32600), true
	}
	return int(epsg - 32700), false
}

// UTMZoneFor returns the UTM zone number [1..60] whose central meridian
// nearest longitude lon.
func UTMZoneFor(lon float64) int {
	z := int(math.Floor((lon+180)/6)) + 1
	return min(max(z, 1), 60)
}

// UTMEpsgFor returns the EPSG code of the UTM CRS covering (lon, lat) on the
// WGS84 datum. Latitude sign selects hemisphere.
func UTMEpsgFor(lon, lat float64) int32 {
	zone := UTMZoneFor(lon)
	if lat >= 0 {
		return int32(32600 + zone)
	}
	return int32(32700 + zone)
}

// estimateUTMFromXY resolves the UTM CRS covering (x, y) as expressed in the
// given source CRS. If crs is projected, the point is first inverse-projected
// to WGS84 to pick the zone.
func estimateUTMFromXY(x, y float64, crs CRS) (CRS, error) {
	if crs.Zero() {
		crs = WGS84
	}
	lon, lat := x, y
	if crs.Projected {
		var err error
		lon, lat, err = projectPoint(x, y, crs, WGS84)
		if err != nil {
			return CRS{}, err
		}
	}
	return LookupCRS(UTMEpsgFor(lon, lat))
}

func llToUTM(lon, lat float64, zone int, north bool) (float64, float64) {
	a := wgs84SemiMajor
	e2 := wgs84E2
	ep2 := e2 / (1 - e2)

	λ0 := degToRad(float64((zone-1)*6 - 180 + 3)) // central meridian of the zone
	λ := degToRad(lon)
	φ := degToRad(lat)

	sinφ := math.Sin(φ)
	cosφ := math.Cos(φ)
	tanφ := sinφ / cosφ

	N := a / math.Sqrt(1-e2*sinφ*sinφ)
	T := tanφ * tanφ
	C := ep2 * cosφ * cosφ
	A := cosφ * (λ - λ0)

	// Meridional arc M from equator to φ.
	M := a * ((1-e2/4-3*e2*e2/64-5*e2*e2*e2/256)*φ -
		(3*e2/8+3*e2*e2/32+45*e2*e2*e2/1024)*math.Sin(2*φ) +
		(15*e2*e2/256+45*e2*e2*e2/1024)*math.Sin(4*φ) -
		(35*e2*e2*e2/3072)*math.Sin(6*φ))

	x := utmScaleFactor*N*(A+(1-T+C)*A*A*A/6+(5-18*T+T*T+72*C-58*ep2)*A*A*A*A*A/120) + utmFalseEasting
	y := utmScaleFactor * (M + N*tanφ*(A*A/2+(5-T+9*C+4*C*C)*A*A*A*A/24+(61-58*T+T*T+600*C-330*ep2)*A*A*A*A*A*A/720))

	if !north {
		y += utmFalseNorthingS
	}
	return x, y
}

func utmToLL(x, y float64, zone int, north bool) (float64, float64) {
	a := wgs84SemiMajor
	e2 := wgs84E2
	ep2 := e2 / (1 - e2)
	e1 := (1 - math.Sqrt(1-e2)) / (1 + math.Sqrt(1-e2))

	xShifted := x - utmFalseEasting
	yShifted := y
	if !north {
		yShifted -= utmFalseNorthingS
	}

	M := yShifted / utmScaleFactor
	μ := M / (a * (1 - e2/4 - 3*e2*e2/64 - 5*e2*e2*e2/256))

	// Footprint latitude
	φ1 := μ +
		(3*e1/2-27*e1*e1*e1/32)*math.Sin(2*μ) +
		(21*e1*e1/16-55*e1*e1*e1*e1/32)*math.Sin(4*μ) +
		(151*e1*e1*e1/96)*math.Sin(6*μ) +
		(1097*e1*e1*e1*e1/512)*math.Sin(8*μ)

	sinφ1 := math.Sin(φ1)
	cosφ1 := math.Cos(φ1)
	tanφ1 := sinφ1 / cosφ1

	C1 := ep2 * cosφ1 * cosφ1
	T1 := tanφ1 * tanφ1
	N1 := a / math.Sqrt(1-e2*sinφ1*sinφ1)
	R1 := a * (1 - e2) / math.Pow(1-e2*sinφ1*sinφ1, 1.5)
	D := xShifted / (N1 * utmScaleFactor)

	φ := φ1 - (N1*tanφ1/R1)*(D*D/2-
		(5+3*T1+10*C1-4*C1*C1-9*ep2)*D*D*D*D/24+
		(61+90*T1+298*C1+45*T1*T1-252*ep2-3*C1*C1)*D*D*D*D*D*D/720)

	λ0 := degToRad(float64((zone-1)*6 - 180 + 3))
	λ := λ0 + (D-
		(1+2*T1+C1)*D*D*D/6+
		(5-2*C1+28*T1-3*C1*C1+8*ep2+24*T1*T1)*D*D*D*D*D/120)/cosφ1

	return λ * 180 / math.Pi, φ * 180 / math.Pi
}

// registerUTMZones adds all 120 WGS84/UTM zone CRSes to the runtime registry
// so LookupCRS can find them. Called from init.
func registerUTMZones() {
	for zone := 1; zone <= 60; zone++ {
		RegisterCRS(CRS{EPSG: int32(32600 + zone), Name: fmt.Sprintf("WGS 84 / UTM zone %dN", zone), Projected: true})
		RegisterCRS(CRS{EPSG: int32(32700 + zone), Name: fmt.Sprintf("WGS 84 / UTM zone %dS", zone), Projected: true})
	}
}

func init() { registerUTMZones() }

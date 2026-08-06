package geometry

import (
	"errors"
	"fmt"
	"math"
)

// ErrGeodesicRequiresGeographic is returned by SampleGeodesic /
// DensifyGeodesic when the input isn't in a geographic CRS.
// "Great-circle" only makes sense on lon/lat coordinates; projected-
// CRS callers wanting "insert intermediate vertices in a straight
// line" should linearly interpolate themselves.
var ErrGeodesicRequiresGeographic = errors.New(
	"geometry: geodesic ops require a geographic CRS (X = longitude, Y = latitude in degrees)")

// ErrAntipodalPoints is returned when SampleGeodesic is asked to
// interpolate between two points that are exact antipodes: the great
// circle through them isn't unique, so there's no canonical arc to
// sample. Callers can dodge this by nudging one endpoint slightly or
// splitting the path through an intermediate waypoint.
var ErrAntipodalPoints = errors.New("geometry: cannot interpolate a unique great circle through antipodal points")

// SampleGeodesic returns n points along the great-circle arc from a
// to b (inclusive of both endpoints). n must be >= 2. Both points
// must be in a geographic CRS with X = longitude and Y = latitude in
// degrees. Uses standard spherical linear interpolation (slerp) on
// unit sphere vectors — no ellipsoidal-Earth corrections.
//
// Output points carry the input's CRS (or WGS84 if the CRS is unset)
// and lon in [-180, 180].
func SampleGeodesic(a, b Point, n int) ([]Point, error) {
	if err := requireGeographic(a, b); err != nil {
		return nil, err
	}
	if n < 2 {
		return nil, fmt.Errorf("geometry: SampleGeodesic requires n>=2, got %d", n)
	}
	crs := a.CRSValue
	if crs.Zero() {
		crs = WGS84
	}
	ax, ay, az := latLonToUnitVec(a.Y, a.X)
	bx, by, bz := latLonToUnitVec(b.Y, b.X)
	// Dot product = cos(Ω) where Ω is the angle between the two
	// unit vectors. Clamp to [-1, 1] against float noise.
	dot := ax*bx + ay*by + az*bz
	switch {
	case dot > 1:
		dot = 1
	case dot < -1:
		dot = -1
	}
	// Antipodal check first: the great circle through two antipodal
	// points isn't unique, so there's no canonical arc to sample.
	// Threshold is 1e-12 because float64 sin(π) doesn't hit zero
	// exactly — checking dot directly avoids the numerical dance.
	if dot <= -1+1e-12 {
		return nil, ErrAntipodalPoints
	}
	// Coincident-points shortcut (dot ≈ 1): the arc is degenerate but
	// not an error. Replicate a for every sample.
	if dot >= 1-1e-12 {
		out := make([]Point, n)
		for i := range n {
			out[i] = Point{X: a.X, Y: a.Y, Z: a.Z, HasZ: a.HasZ, CRSValue: crs}
		}
		return out, nil
	}
	omega := math.Acos(dot)
	sinOmega := math.Sin(omega)
	out := make([]Point, n)
	for i := range n {
		t := float64(i) / float64(n-1)
		// Slerp weights.
		wa := math.Sin((1-t)*omega) / sinOmega
		wb := math.Sin(t*omega) / sinOmega
		x := wa*ax + wb*bx
		y := wa*ay + wb*by
		z := wa*az + wb*bz
		lat, lon := unitVecToLatLon(x, y, z)
		out[i] = Point{X: lon, Y: lat, CRSValue: crs}
	}
	// Force exact endpoint match — slerp is analytically exact but
	// float64 lat/lon reconstruction can drift by a ULP at the
	// endpoints. Callers should get the exact input coordinates
	// back at index 0 and n-1.
	out[0] = Point{X: a.X, Y: a.Y, Z: a.Z, HasZ: a.HasZ, CRSValue: crs}
	out[n-1] = Point{X: b.X, Y: b.Y, Z: b.Z, HasZ: b.HasZ, CRSValue: crs}
	return out, nil
}

// DensifyGeodesic replaces every segment of l with its great-circle
// densification at ≤ stepMeters spacing. Endpoints of each original
// segment are always preserved. Requires l to be in a geographic CRS
// (see SampleGeodesic).
//
// stepMeters is measured on a sphere of Earth radius (matching the
// existing Haversine implementation). Values <= 0 return the input
// unchanged.
func DensifyGeodesic(l LineString, stepMeters float64) (LineString, error) {
	if stepMeters <= 0 || len(l.Points) < 2 {
		return l, nil
	}
	// Check the whole line's CRS once (each segment gets validated
	// by SampleGeodesic anyway, but doing it up front lets us bail
	// with one clean error instead of a garbled multi-segment failure).
	if l.CRSValue.Projected {
		return LineString{}, fmt.Errorf("%w: got %s", ErrGeodesicRequiresGeographic, l.CRSValue)
	}
	crs := l.CRSValue
	if crs.Zero() {
		crs = WGS84
	}
	radiusMeters := EarthRadiusKM * 1000
	out := make([]Point, 0, len(l.Points))
	for i := 0; i < len(l.Points)-1; i++ {
		a := l.Points[i]
		b := l.Points[i+1]
		a.CRSValue = crs
		b.CRSValue = crs
		// Arc length in meters via Haversine.
		distMeters, err := Haversine(a, b, UnitMeters)
		if err != nil {
			return LineString{}, err
		}
		// n = ceil(dist / step) + 1 gives spacing ≤ stepMeters and
		// preserves both endpoints. Minimum 2 (a and b, no interior).
		n := 2
		if distMeters > stepMeters {
			n = int(math.Ceil(distMeters/stepMeters)) + 1
		}
		samples, err := SampleGeodesic(a, b, n)
		if err != nil {
			return LineString{}, err
		}
		if i == 0 {
			out = append(out, samples...)
		} else {
			// Skip samples[0]; it's the same as the previous segment's
			// last emitted point.
			out = append(out, samples[1:]...)
		}
		_ = radiusMeters // reserved for future ellipsoidal upgrade
	}
	return LineString{Points: out, CRSValue: crs, HasZ: l.HasZ}, nil
}

// requireGeographic errors if either p1 or p2 carries a projected
// CRS. Points with unset CRS are treated as WGS84 (matching the
// broader package convention).
func requireGeographic(p1, p2 Point) error {
	for _, p := range []Point{p1, p2} {
		if !p.CRSValue.Zero() && p.CRSValue.Projected {
			return fmt.Errorf("%w: got %s", ErrGeodesicRequiresGeographic, p.CRSValue)
		}
	}
	return nil
}

// latLonToUnitVec maps (lat, lon) in degrees to a unit vector on the
// sphere. Longitude 0 / latitude 0 lands at (1, 0, 0).
func latLonToUnitVec(latDeg, lonDeg float64) (x, y, z float64) {
	lat := degToRad(latDeg)
	lon := degToRad(lonDeg)
	cLat := math.Cos(lat)
	return cLat * math.Cos(lon), cLat * math.Sin(lon), math.Sin(lat)
}

// unitVecToLatLon inverts latLonToUnitVec. Returned lat in [-90, 90],
// lon in [-180, 180].
func unitVecToLatLon(x, y, z float64) (latDeg, lonDeg float64) {
	// Clamp z against float noise around ±1.
	switch {
	case z > 1:
		z = 1
	case z < -1:
		z = -1
	}
	lat := math.Asin(z)
	lon := math.Atan2(y, x)
	return lat * 180 / math.Pi, lon * 180 / math.Pi
}

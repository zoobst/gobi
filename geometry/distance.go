package geometry

import (
	"fmt"
	"math"
)

// EarthRadiusKM is the mean Earth radius used by haversine calculations.
const EarthRadiusKM = 6371.0088

// Unit represents a linear distance unit.
type Unit string

const (
	UnitMeters        Unit = "m"
	UnitKilometers    Unit = "km"
	UnitMiles         Unit = "mi"
	UnitFeet          Unit = "ft"
	UnitNauticalMiles Unit = "nmi"
)

// MetersPerUnit returns the number of meters in one of the given
// unit. Exported so callers building their own bulk distance
// kernels can hoist the scale factor outside a hot loop instead
// of paying a per-call `metersPerUnit` lookup.
func MetersPerUnit(u Unit) (float64, error) {
	switch u {
	case UnitMeters, "":
		return 1, nil
	case UnitKilometers:
		return 1000, nil
	case UnitMiles:
		return 1609.344, nil
	case UnitFeet:
		return 0.3048, nil
	case UnitNauticalMiles:
		return 1852, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrInvalidUnit, u)
	}
}

// metersPerUnit is the unexported alias kept so the internal call
// sites don't churn. Delegates to MetersPerUnit.
func metersPerUnit(u Unit) (float64, error) { return MetersPerUnit(u) }

// convertMeters converts a value in meters to the specified unit.
func convertMeters(meters float64, u Unit) (float64, error) {
	m, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	return meters / m, nil
}

func degToRad(d float64) float64 { return d * math.Pi / 180 }

// Haversine returns the great-circle distance between two lon/lat
// Points on a sphere of Earth radius, in the requested unit. Point
// X = longitude, Y = latitude (WKB convention); Z / CRS are ignored.
//
// Breaking change in v0.2.16: previously took four float64 args
// (lon1, lat1, lon2, lat2). The new signature aligns with
// HaversineBatch and Point.Distance so callers holding geometry
// types can pass them directly. Migration: replace
// `Haversine(a.X, a.Y, b.X, b.Y, u)` with `Haversine(a, b, u)`.
func Haversine(from, to Point, u Unit) (float64, error) {
	perM, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	φ1 := degToRad(from.Y)
	φ2 := degToRad(to.Y)
	dφ := degToRad(to.Y - from.Y)
	dλ := degToRad(to.X - from.X)
	a := math.Sin(dφ/2)*math.Sin(dφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	distMeters := EarthRadiusKM * 1000 * c
	return distMeters / perM, nil
}

// HaversineBatch returns per-pair great-circle distances between
// from[i] and to[i] in the requested unit. Semantically equivalent
// to calling Haversine(from[i], to[i], u) in a loop, but
// bulk-optimized:
//
//   - The unit conversion factor, Earth-radius constant, and
//     degree-to-radian scale are hoisted outside the inner loop
//     (one metersPerUnit call for the whole batch).
//   - Per-row math runs in a fixed-count vars body — Go's inliner
//     keeps it tight vs. the per-call scalar Haversine which pays
//     a function-call boundary + defer + err-check per row.
//
// Both input slices must be the same length; a mismatch returns
// ErrColumnLenMismatch. Empty slices are legal and return an empty
// (non-nil) result.
//
// CRS on each Point is ignored — Haversine is a lon/lat sphere
// computation that expects Y=latitude, X=longitude in degrees.
// Points in a projected CRS give nonsensical distances; converting
// via ToCRS(WGS84) before calling is the caller's responsibility.
//
// Return type is a flat []float64 — same shape a downstream SIMD
// kernel or arrow builder wants. Nulls in the input aren't
// signaled (Point isn't a nullable type); to skip-and-preserve
// row positions, either pre-filter or pass sentinel points and
// mask the output.
func HaversineBatch(from, to []Point, u Unit) ([]float64, error) {
	if len(from) != len(to) {
		return nil, fmt.Errorf("HaversineBatch: length mismatch: from=%d to=%d",
			len(from), len(to))
	}
	perM, err := metersPerUnit(u)
	if err != nil {
		return nil, err
	}
	// scale converts the great-circle central angle (radians) to
	// the requested output unit. Factored out so the inner loop
	// only pays for the trig, not the constants.
	scale := EarthRadiusKM * 1000 / perM
	const deg2rad = math.Pi / 180

	out := make([]float64, len(from))
	for i := range from {
		p := from[i]
		q := to[i]
		phi1 := p.Y * deg2rad
		phi2 := q.Y * deg2rad
		dphi := (q.Y - p.Y) * deg2rad
		dlam := (q.X - p.X) * deg2rad
		sinHP := math.Sin(dphi / 2)
		sinHL := math.Sin(dlam / 2)
		a := sinHP*sinHP + math.Cos(phi1)*math.Cos(phi2)*sinHL*sinHL
		c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
		out[i] = scale * c
	}
	return out, nil
}

// Euclidean returns the planar distance between two Points, in the
// requested unit. The input coordinates are assumed to already be
// in meters (projected CRS); Z / CRS are ignored.
//
// Breaking change in v0.2.16: previously took four float64 args
// (x1, y1, x2, y2). The new signature aligns with Haversine +
// HaversineBatch + Point.Distance. Migration: replace
// `Euclidean(a.X, a.Y, b.X, b.Y, u)` with `Euclidean(a, b, u)`.
func Euclidean(from, to Point, u Unit) (float64, error) {
	dx := to.X - from.X
	dy := to.Y - from.Y
	return convertMeters(math.Sqrt(dx*dx+dy*dy), u)
}

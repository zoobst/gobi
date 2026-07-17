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

// metersPerUnit returns the number of meters in one of the given unit.
func metersPerUnit(u Unit) (float64, error) {
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

// convertMeters converts a value in meters to the specified unit.
func convertMeters(meters float64, u Unit) (float64, error) {
	m, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	return meters / m, nil
}

func degToRad(d float64) float64 { return d * math.Pi / 180 }

// Haversine returns the great-circle distance between two lon/lat points on a
// sphere of Earth radius, in the requested unit.
func Haversine(lon1, lat1, lon2, lat2 float64, u Unit) (float64, error) {
	perM, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	φ1 := degToRad(lat1)
	φ2 := degToRad(lat2)
	dφ := degToRad(lat2 - lat1)
	dλ := degToRad(lon2 - lon1)
	a := math.Sin(dφ/2)*math.Sin(dφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	distMeters := EarthRadiusKM * 1000 * c
	return distMeters / perM, nil
}

// Euclidean returns the planar distance between two points, in the requested
// unit. The input coordinates are assumed to already be in meters (projected
// CRS).
func Euclidean(x1, y1, x2, y2 float64, u Unit) (float64, error) {
	dx := x2 - x1
	dy := y2 - y1
	return convertMeters(math.Sqrt(dx*dx+dy*dy), u)
}

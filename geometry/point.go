package geometry

import (
	"fmt"
	"math"
)

// Point is a 2D or optionally 3D (XYZ) point. Z is populated only when
// HasZ is true; otherwise it is ignored by encoders and decoders.
type Point struct {
	X, Y, Z  float64
	CRSValue CRS
	HasZ     bool
}

// NewPoint returns a 2D Point with the given coordinates and CRS. Passing
// the zero CRS leaves the CRS unset (interpreted as WGS84 by most
// operations).
func NewPoint(x, y float64, crs CRS) Point {
	return Point{X: x, Y: y, CRSValue: crs}
}

// NewPointZ returns a 3D Point (X, Y, Z) with the given CRS.
func NewPointZ(x, y, z float64, crs CRS) Point {
	return Point{X: x, Y: y, Z: z, CRSValue: crs, HasZ: true}
}

func (p Point) Type() Type { return TypePoint }
func (p Point) CRS() CRS   { return p.CRSValue }
func (p Point) Is3D() bool { return p.HasZ }

// Bounds returns the XY bounding box. Z, if present, is ignored (the Bounds
// type is deliberately 2D).
func (p Point) Bounds() Bounds {
	return Bounds{MinX: p.X, MinY: p.Y, MaxX: p.X, MaxY: p.Y}
}

// Equal reports whether p and o are equal in coordinates, CRS, and
// dimensionality. Z is compared only if both points are 3D.
func (p Point) Equal(o Point) bool {
	if p.X != o.X || p.Y != o.Y || !p.CRSValue.Equal(o.CRSValue) || p.HasZ != o.HasZ {
		return false
	}
	if p.HasZ && p.Z != o.Z {
		return false
	}
	return true
}

func (p Point) WKT() string {
	if p.HasZ {
		return fmt.Sprintf("POINT Z (%s %s %s)",
			formatCoord(p.X), formatCoord(p.Y), formatCoord(p.Z))
	}
	return fmt.Sprintf("POINT (%s %s)", formatCoord(p.X), formatCoord(p.Y))
}

func (p Point) AppendWKB(buf []byte) []byte {
	if p.HasZ {
		buf = appendWKBHeader(buf, wkbPointZ)
		buf = appendFloat64LE(buf, p.X)
		buf = appendFloat64LE(buf, p.Y)
		buf = appendFloat64LE(buf, p.Z)
		return buf
	}
	buf = appendWKBHeader(buf, wkbPoint)
	buf = appendFloat64LE(buf, p.X)
	buf = appendFloat64LE(buf, p.Y)
	return buf
}

// ToCRS reprojects p into target. Z is carried through unchanged.
func (p Point) ToCRS(target CRS) (Point, error) {
	g, err := Project(p, target)
	if err != nil {
		return Point{}, err
	}
	return g.(Point), nil
}

// EstimateUTMCRS returns the CRS of the UTM zone covering p (on the WGS84
// datum). If p is in a projected CRS, it is first inverse-projected to
// WGS84 to pick the zone.
func (p Point) EstimateUTMCRS() (CRS, error) {
	return estimateUTMFromXY(p.X, p.Y, p.CRSValue)
}

// Distance returns the planar (XY) distance from p to o in the requested
// unit. Z is ignored. For geographic CRSes Haversine is used; for projected
// CRSes Euclidean.
func (p Point) Distance(o Point, u Unit) (float64, error) {
	if !p.CRSValue.Equal(o.CRSValue) {
		return 0, ErrCRSMismatch
	}
	if p.CRSValue.Projected {
		return Euclidean(p.X, p.Y, o.X, o.Y, u)
	}
	return Haversine(p.X, p.Y, o.X, o.Y, u)
}

// Distance3D returns the 3D Euclidean distance from p to o, treating Z as
// coplanar with X/Y (i.e. as if measured in the same unit as the CRS's
// linear unit). Requires both points to be in the same projected CRS and to
// have HasZ set on both sides; otherwise returns ErrCRSMismatch or an error
// noting the missing dimension.
func (p Point) Distance3D(o Point, u Unit) (float64, error) {
	if !p.CRSValue.Equal(o.CRSValue) {
		return 0, ErrCRSMismatch
	}
	if !p.HasZ || !o.HasZ {
		return 0, fmt.Errorf("%w: Distance3D requires 3D points on both sides", ErrTypeMismatch)
	}
	if !p.CRSValue.Projected && !p.CRSValue.Zero() {
		return 0, fmt.Errorf("%w: Distance3D requires a projected CRS", ErrCRSMismatch)
	}
	dx := o.X - p.X
	dy := o.Y - p.Y
	dz := o.Z - p.Z
	return convertMeters(math.Sqrt(dx*dx+dy*dy+dz*dz), u)
}

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

// Centroid returns p itself — a Point is its own centroid. Present so
// Point satisfies the Geometry interface's Centroid method and the
// top-level geometry.Centroid dispatch collapses to interface dispatch.
func (p Point) Centroid() Point { return p }

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
		return Euclidean(p, o, u)
	}
	return Haversine(p, o, u)
}

// Distance3D returns the 3D distance from p to o, dispatching on
// CRS to match the 2D Distance method's shape:
//
//   - Projected CRS: 3D Cartesian Euclidean in the CRS's linear
//     unit, converted to u. Treats Z as coplanar with X/Y (same
//     unit).
//   - Geographic CRS: ECEF Euclidean via the WGS84 ellipsoid, in
//     meters, converted to u. Computes the straight-line
//     (through-Earth chord) distance between the two 3D points.
//     For altitude-agnostic surface arc distance use Distance /
//     Haversine (2D).
//   - Zero CRS: 3D Cartesian in the caller's units — assumes the
//     coordinates are already in a common projected frame.
//
// Both sides must share the same CRS (ErrCRSMismatch on
// disagreement) and have HasZ set (ErrTypeMismatch otherwise).
//
// Pre-v0.4.7 behavior was Cartesian-only, erroring on geographic
// input. The new dispatch keeps the projected path bit-identical
// (existing callers unaffected) and adds correct-math handling for
// geographic 3D points that previously errored.
func (p Point) Distance3D(o Point, u Unit) (float64, error) {
	if !p.CRSValue.Equal(o.CRSValue) {
		return 0, ErrCRSMismatch
	}
	if !p.HasZ || !o.HasZ {
		return 0, fmt.Errorf("%w: Distance3D requires 3D points on both sides", ErrTypeMismatch)
	}
	if p.CRSValue.Projected || p.CRSValue.Zero() {
		// Cartesian path — the pre-existing behavior. Z is
		// coplanar with X/Y (same linear unit).
		dx := o.X - p.X
		dy := o.Y - p.Y
		dz := o.Z - p.Z
		return convertMeters(math.Sqrt(dx*dx+dy*dy+dz*dz), u)
	}
	// Geographic path — ECEF Euclidean via WGS84.
	//
	// Dispatches into the SoA kernel with N=1 slabs so the scalar
	// and Series-level paths share exactly one implementation of
	// the math. Stack-allocated 1-element slabs — no heap use.
	lons1 := [1]float64{p.X}
	lats1 := [1]float64{p.Y}
	alts1 := [1]float64{p.Z}
	lons2 := [1]float64{o.X}
	lats2 := [1]float64{o.Y}
	alts2 := [1]float64{o.Z}
	var out [1]float64
	Distance3DGeodesicFromSlabs(lons1[:], lats1[:], alts1[:], lons2[:], lats2[:], alts2[:], out[:], nil)
	return convertMeters(out[0], u)
}

// Force2D returns p with Z dropped and HasZ cleared. Idempotent on
// 2D points. Mirrors PostGIS ST_Force2D semantics.
func (p Point) Force2D() Point {
	p.Z = 0
	p.HasZ = false
	return p
}

// ForceZ returns p with Z set to alt and HasZ = true. Overwrites any
// existing Z value. Mirrors PostGIS ST_Force3D / ST_Force3DZ.
func (p Point) ForceZ(alt float64) Point {
	p.Z = alt
	p.HasZ = true
	return p
}

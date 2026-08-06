package geometry

import (
	"errors"
	"fmt"
	"math"
)

// Ellipse is an axis-aligned-then-rotated elliptical shape. Like
// Circle it is deliberately NOT a Geometry — OGC SFA / WKB has no
// ellipse encoding, so callers who need to serialize should lower
// via Boundary(n) → Polygon or BoundaryLine(n) → LineString.
//
// Rotation is measured in radians, counter-clockwise from the +X
// axis. When Rotation == 0, SemiA runs along +X and SemiB along +Y.
// Units follow Center.CRSValue (meters for UTM, degrees for WGS84);
// for physically-meaningful ellipses on the ground, reproject the
// input via ToCRS to a projected CRS first.
type Ellipse struct {
	Center   Point
	SemiA    float64 // "first" semi-axis (along +X in ellipse-local frame)
	SemiB    float64 // "second" semi-axis (along +Y in ellipse-local frame)
	Rotation float64 // CCW radians from +X to ellipse-local +X
}

// ErrEllipseFromFoci is returned by EllipseFromFoci when the given
// major-axis length is too small to accommodate the two foci
// (majorAxis < |f1 - f2|), which is geometrically impossible.
var ErrEllipseFromFoci = errors.New("geometry: cannot construct ellipse from foci")

// NewEllipse returns an Ellipse with the given center, semi-axes,
// and rotation. Semi-axis order is not enforced: callers may pass
// SemiA < SemiB (rotation still applies).
func NewEllipse(center Point, semiA, semiB, rotation float64) Ellipse {
	return Ellipse{Center: center, SemiA: semiA, SemiB: semiB, Rotation: rotation}
}

// EllipseFromFoci constructs the unique ellipse with the two given
// foci and a specified major-axis length. Requires majorAxis >=
// |f1 - f2| (otherwise no ellipse satisfies the definition).
// SemiA is placed along the f1→f2 direction; SemiB perpendicular.
//
// Both foci must share (or leave unset) the same CRS; the output
// Ellipse inherits it from f1.
func EllipseFromFoci(f1, f2 Point, majorAxis float64) (Ellipse, error) {
	if majorAxis <= 0 {
		return Ellipse{}, fmt.Errorf("%w: majorAxis must be positive, got %v",
			ErrEllipseFromFoci, majorAxis)
	}
	dx := f2.X - f1.X
	dy := f2.Y - f1.Y
	focalDist := math.Hypot(dx, dy)
	if majorAxis < focalDist {
		return Ellipse{}, fmt.Errorf("%w: majorAxis %v < |f1-f2| %v",
			ErrEllipseFromFoci, majorAxis, focalDist)
	}
	semiA := majorAxis / 2
	// Semi-minor from the standard ellipse identity: c² = a² - b², so
	// b = sqrt(a² - c²) where c is the linear eccentricity (half the
	// focal distance).
	c := focalDist / 2
	semiB := math.Sqrt(semiA*semiA - c*c)
	// Rotation from +X axis to the f1→f2 direction. If the foci
	// coincide (focalDist == 0) the ellipse is a circle and the
	// rotation is irrelevant — set to 0.
	rotation := 0.0
	if focalDist > 0 {
		rotation = math.Atan2(dy, dx)
	}
	return Ellipse{
		Center: Point{
			X:        (f1.X + f2.X) / 2,
			Y:        (f1.Y + f2.Y) / 2,
			CRSValue: f1.CRSValue,
		},
		SemiA:    semiA,
		SemiB:    semiB,
		Rotation: rotation,
	}, nil
}

// Contains reports whether p lies inside or on the boundary of the
// ellipse. Transforms p to the ellipse-local frame (translate to
// center, rotate by -Rotation) and evaluates (x'/a)² + (y'/b)² ≤ 1.
// Only the 2D X/Y coordinates are considered.
func (e Ellipse) Contains(p Point) bool {
	dx := p.X - e.Center.X
	dy := p.Y - e.Center.Y
	cosR := math.Cos(-e.Rotation)
	sinR := math.Sin(-e.Rotation)
	xLocal := dx*cosR - dy*sinR
	yLocal := dx*sinR + dy*cosR
	if e.SemiA == 0 || e.SemiB == 0 {
		return xLocal == 0 && yLocal == 0
	}
	return (xLocal*xLocal)/(e.SemiA*e.SemiA)+(yLocal*yLocal)/(e.SemiB*e.SemiB) <= 1
}

// Area returns π·a·b — exact.
func (e Ellipse) Area() float64 {
	return math.Pi * math.Abs(e.SemiA) * math.Abs(e.SemiB)
}

// Circumference returns an approximation via Ramanujan's second
// formula:
//
//	C ≈ π(a+b) · (1 + 3h / (10 + √(4 - 3h)))
//	where h = ((a-b)/(a+b))²
//
// Accurate to ~1e-9 for eccentricity < 0.9 (i.e. axis ratio > 0.44),
// degrading to ~1e-4 near extreme eccentricity. No closed-form exact
// solution exists for the ellipse's perimeter (it's an incomplete
// elliptic integral of the second kind).
func (e Ellipse) Circumference() float64 {
	a := math.Abs(e.SemiA)
	b := math.Abs(e.SemiB)
	if a+b == 0 {
		return 0
	}
	h := (a - b) / (a + b)
	h2 := h * h
	return math.Pi * (a + b) * (1 + 3*h2/(10+math.Sqrt(4-3*h2)))
}

// Bounds returns the axis-aligned bounding box of the rotated
// ellipse. Derived from the parametric extremum:
//
//	x_max = √(a²cos²θ + b²sin²θ)
//	y_max = √(a²sin²θ + b²cos²θ)
//
// where θ is the rotation. The box is centered on e.Center and has
// full width 2·x_max, full height 2·y_max.
func (e Ellipse) Bounds() Bounds {
	a := math.Abs(e.SemiA)
	b := math.Abs(e.SemiB)
	cosR := math.Cos(e.Rotation)
	sinR := math.Sin(e.Rotation)
	xHalf := math.Sqrt(a*a*cosR*cosR + b*b*sinR*sinR)
	yHalf := math.Sqrt(a*a*sinR*sinR + b*b*cosR*cosR)
	return Bounds{
		MinX: e.Center.X - xHalf,
		MinY: e.Center.Y - yHalf,
		MaxX: e.Center.X + xHalf,
		MaxY: e.Center.Y + yHalf,
	}
}

// Boundary returns a closed CCW Polygon approximating the ellipse
// with nVertices segments. Each vertex is the parametric point
// (a cos t, b sin t) transformed by Rotation and translated to
// Center. Points nVertices < 4 fall back to DefaultBufferSegments
// (matching Circle.Boundary and Point.Buffer for consistency).
func (e Ellipse) Boundary(nVertices int) Polygon {
	if nVertices < 4 {
		nVertices = DefaultBufferSegments
	}
	cosR := math.Cos(e.Rotation)
	sinR := math.Sin(e.Rotation)
	ring := make([]Point, nVertices+1)
	for i := range nVertices {
		t := 2 * math.Pi * float64(i) / float64(nVertices)
		x := e.SemiA * math.Cos(t)
		y := e.SemiB * math.Sin(t)
		ring[i] = Point{
			X:        e.Center.X + x*cosR - y*sinR,
			Y:        e.Center.Y + x*sinR + y*cosR,
			CRSValue: e.Center.CRSValue,
		}
	}
	ring[nVertices] = ring[0]
	return Polygon{Rings: [][]Point{ring}, CRSValue: e.Center.CRSValue}
}

// BoundaryLine returns an open LineString along the ellipse's
// circumference (nVertices points, NOT closed — the last point is
// not a repeat of the first). Useful for representing an arc
// without polygon closure semantics.
func (e Ellipse) BoundaryLine(nVertices int) LineString {
	if nVertices < 4 {
		nVertices = DefaultBufferSegments
	}
	cosR := math.Cos(e.Rotation)
	sinR := math.Sin(e.Rotation)
	pts := make([]Point, nVertices)
	for i := range nVertices {
		t := 2 * math.Pi * float64(i) / float64(nVertices)
		x := e.SemiA * math.Cos(t)
		y := e.SemiB * math.Sin(t)
		pts[i] = Point{
			X:        e.Center.X + x*cosR - y*sinR,
			Y:        e.Center.Y + x*sinR + y*cosR,
			CRSValue: e.Center.CRSValue,
		}
	}
	return LineString{Points: pts, CRSValue: e.Center.CRSValue}
}

package geometry

import (
	"errors"
	"fmt"
	"math"
)

// Circle is a first-class circular shape defined by its center and
// radius. Deliberately NOT a Geometry: OGC SFA / WKB has no encoding
// for circles, so callers who need to serialize should lower via
// Boundary or BoundaryLine first.
//
// Radius units follow Center.CRSValue: meters for UTM / PseudoMercator,
// degrees for WGS84. Callers wanting a Circle "in meters" from a
// geographic-CRS point cluster should reproject the input via ToCRS
// before calling FitCircle.
type Circle struct {
	Center Point
	Radius float64
}

// ErrCircleFit is returned by FitCircle when the input cannot resolve
// to a unique circle (e.g. fewer than 3 points, or points collinear
// within numerical tolerance).
var ErrCircleFit = errors.New("geometry: cannot fit circle to input")

// CircleFitMethod selects the least-squares algorithm.
type CircleFitMethod int

const (
	// FitTaubin (default): geometrically-weighted algebraic fit
	// (Taubin 1991 / Chernov). Closed-form via one Newton step on a
	// cubic; unbiased when the point cloud spans only a partial arc.
	// Preferred for GIS-style fits where points may cluster on one
	// side of the true circle.
	FitTaubin CircleFitMethod = iota
	// FitKasa: plain algebraic fit (Kasa 1976). Faster (linear
	// solve, no root-find) but biased toward smaller radii on
	// partial-arc inputs. Use when speed dominates and inputs are
	// known to span most of the circumference.
	FitKasa
)

// CircleFitOptions tunes FitCircle behavior. The zero value picks
// sensible defaults.
type CircleFitOptions struct {
	Method CircleFitMethod
}

// FitCircle fits a Circle to the input points via least squares and
// returns per-point signed geometric residuals — residuals[i] is
// (|points[i] - center| - radius), positive when the point is outside
// the fit circle, negative when inside. Requires at least 3 points.
// Returns ErrCircleFit when the points are collinear or the system
// is otherwise degenerate.
//
// The fit is 2D in the coordinate plane of the input Points; Z is
// ignored. Output Circle's CRS is inherited from the first point.
func FitCircle(points []Point, opts CircleFitOptions) (Circle, []float64, error) {
	if len(points) < 3 {
		return Circle{}, nil, fmt.Errorf("%w: need at least 3 points, got %d",
			ErrCircleFit, len(points))
	}
	var c Circle
	var err error
	switch opts.Method {
	case FitKasa:
		c, err = fitCircleKasa(points)
	case FitTaubin:
		c, err = fitCircleTaubin(points)
	default:
		c, err = fitCircleTaubin(points)
	}
	if err != nil {
		return Circle{}, nil, err
	}
	// Inherit CRS from the first input point.
	c.Center.CRSValue = points[0].CRSValue
	// Compute residuals: distance from each point to circle boundary.
	resid := make([]float64, len(points))
	for i, p := range points {
		dx := p.X - c.Center.X
		dy := p.Y - c.Center.Y
		resid[i] = math.Sqrt(dx*dx+dy*dy) - c.Radius
	}
	return c, resid, nil
}

// Contains reports whether p lies inside (or on the boundary of) the
// circle. Uses the Euclidean distance from p to the center. Only the
// 2D X/Y coordinates are considered.
func (c Circle) Contains(p Point) bool {
	dx := p.X - c.Center.X
	dy := p.Y - c.Center.Y
	return dx*dx+dy*dy <= c.Radius*c.Radius
}

// Distance returns the SIGNED distance from p to the circle's
// boundary in the coordinate plane's units: negative when p is
// inside the circle, positive when outside, zero on the boundary.
// This matches the "level set" convention (φ(x) = |x - center| - r).
func (c Circle) Distance(p Point) float64 {
	dx := p.X - c.Center.X
	dy := p.Y - c.Center.Y
	return math.Sqrt(dx*dx+dy*dy) - c.Radius
}

// Area returns πr².
func (c Circle) Area() float64 { return math.Pi * c.Radius * c.Radius }

// Circumference returns 2πr.
func (c Circle) Circumference() float64 { return 2 * math.Pi * c.Radius }

// Boundary returns a closed Polygon approximating the circle with
// nVertices segments (nVertices < 4 falls back to
// DefaultBufferSegments for consistency with Point.Buffer). The
// resulting polygon has exactly nVertices+1 points (the last equals
// the first) and is wound CCW.
func (c Circle) Boundary(nVertices int) Polygon {
	if nVertices < 4 {
		nVertices = DefaultBufferSegments
	}
	ring := make([]Point, nVertices+1)
	for i := range nVertices {
		theta := 2 * math.Pi * float64(i) / float64(nVertices)
		ring[i] = Point{
			X:        c.Center.X + c.Radius*math.Cos(theta),
			Y:        c.Center.Y + c.Radius*math.Sin(theta),
			CRSValue: c.Center.CRSValue,
		}
	}
	ring[nVertices] = ring[0]
	return Polygon{Rings: [][]Point{ring}, CRSValue: c.Center.CRSValue}
}

// BoundaryLine returns an open LineString along the circle's
// circumference (n vertices, NOT closed — the last point is not a
// repeat of the first). Useful when callers want to represent an arc
// without polygon closure semantics. nVertices < 4 falls back to
// DefaultBufferSegments.
func (c Circle) BoundaryLine(nVertices int) LineString {
	if nVertices < 4 {
		nVertices = DefaultBufferSegments
	}
	pts := make([]Point, nVertices)
	for i := range nVertices {
		theta := 2 * math.Pi * float64(i) / float64(nVertices)
		pts[i] = Point{
			X:        c.Center.X + c.Radius*math.Cos(theta),
			Y:        c.Center.Y + c.Radius*math.Sin(theta),
			CRSValue: c.Center.CRSValue,
		}
	}
	return LineString{Points: pts, CRSValue: c.Center.CRSValue}
}

// fitCircleKasa solves the linear system for the algebraic circle
// coefficients A, B, C in x² + y² + Ax + By + C = 0, using the
// standard 3×3 normal-equations formulation. The center is
// (-A/2, -B/2) and r² = (A² + B²)/4 - C. Solve is via Cramer's rule
// (the system is small and fixed).
func fitCircleKasa(points []Point) (Circle, error) {
	// Recenter on the centroid for numerical stability — the recovered
	// center is then shifted back at the end.
	var mx, my float64
	for _, p := range points {
		mx += p.X
		my += p.Y
	}
	n := float64(len(points))
	mx /= n
	my /= n

	var sxx, sxy, sx, syy, sy float64
	var sxz, syz, sz float64
	for _, p := range points {
		u := p.X - mx
		v := p.Y - my
		z := u*u + v*v
		sxx += u * u
		sxy += u * v
		sx += u
		syy += v * v
		sy += v
		sxz += u * z
		syz += v * z
		sz += z
	}
	// Recentered coords have Σu = Σv = 0, but we keep sx / sy in the
	// system for the general form (they're just numerically ~0).
	// Solve:
	//   [ sxx sxy sx ] [A]   [ -sxz ]
	//   [ sxy syy sy ] [B] = [ -syz ]
	//   [ sx  sy  n  ] [C]   [ -sz  ]
	det := sxx*(syy*n-sy*sy) - sxy*(sxy*n-sy*sx) + sx*(sxy*sy-syy*sx)
	if math.Abs(det) < 1e-30 {
		return Circle{}, fmt.Errorf("%w: system is singular (collinear input?)", ErrCircleFit)
	}
	rhsA := -sxz
	rhsB := -syz
	rhsC := -sz
	A := (rhsA*(syy*n-sy*sy) - sxy*(rhsB*n-sy*rhsC) + sx*(rhsB*sy-syy*rhsC)) / det
	B := (sxx*(rhsB*n-sy*rhsC) - rhsA*(sxy*n-sy*sx) + sx*(sxy*rhsC-rhsB*sx)) / det
	C := (sxx*(syy*rhsC-rhsB*sy) - sxy*(sxy*rhsC-rhsB*sx) + rhsA*(sxy*sy-syy*sx)) / det

	uc := -A / 2
	vc := -B / 2
	r2 := uc*uc + vc*vc - C
	if r2 <= 0 {
		return Circle{}, fmt.Errorf("%w: fitted radius² is non-positive", ErrCircleFit)
	}
	return Circle{
		Center: Point{X: uc + mx, Y: vc + my},
		Radius: math.Sqrt(r2),
	}, nil
}

// fitCircleTaubin implements the Taubin algebraic circle fit as
// presented in Chernov (2005), "On the convergence of fitting
// algorithms in computer vision." Same closed-form linear cost as
// Kasa but adds one Newton step on a cubic characteristic polynomial
// to remove the small-radius bias. Preferred when points cover only
// a partial arc of the true circle.
//
// Reference implementation:
//
//	https://people.cas.uab.edu/~mosya/cl/CircleFitByTaubin.cpp
func fitCircleTaubin(points []Point) (Circle, error) {
	n := len(points)
	// Center the point cloud.
	var mx, my float64
	for _, p := range points {
		mx += p.X
		my += p.Y
	}
	nF := float64(n)
	mx /= nF
	my /= nF

	// Compute moments in recentered coords.
	var Mxx, Mxy, Myy, Mxz, Myz, Mzz float64
	for _, p := range points {
		u := p.X - mx
		v := p.Y - my
		z := u*u + v*v
		Mxx += u * u
		Mxy += u * v
		Myy += v * v
		Mxz += u * z
		Myz += v * z
		Mzz += z * z
	}
	Mxx /= nF
	Mxy /= nF
	Myy /= nF
	Mxz /= nF
	Myz /= nF
	Mzz /= nF

	// Characteristic polynomial: A3*x³ + A2*x² + A22*x + A0 = 0.
	// We look for the smallest positive root x; the true circle
	// corresponds to that root.
	Mz := Mxx + Myy
	CovXY := Mxx*Myy - Mxy*Mxy
	A3 := 4 * Mz
	A2 := -3*Mz*Mz - Mzz
	A1 := Mzz*Mz + 4*CovXY*Mz - Mxz*Mxz - Myz*Myz - Mz*Mz*Mz
	A0 := Mxz*Mxz*Myy + Myz*Myz*Mxx - Mzz*CovXY - 2*Mxz*Myz*Mxy + Mz*Mz*CovXY
	A22 := A2 + A2
	A33 := A3 + A3 + A3

	// Newton iteration starting at x = 0.
	x := 0.0
	y := A0
	for range 99 {
		Dy := A1 + x*(A22+A33*x)
		if Dy == 0 {
			break
		}
		xNew := x - y/Dy
		if xNew == x || math.IsNaN(xNew) {
			break
		}
		yNew := A0 + xNew*(A1+xNew*(A2+xNew*A3))
		if math.Abs(yNew) >= math.Abs(y) {
			break
		}
		x = xNew
		y = yNew
	}

	// Recover center and radius from the root.
	DET := x*x - x*Mz + CovXY
	if math.Abs(DET) < 1e-30 {
		return Circle{}, fmt.Errorf("%w: Taubin system is singular (collinear input?)", ErrCircleFit)
	}
	uc := (Mxz*(Myy-x) - Myz*Mxy) / DET / 2
	vc := (Myz*(Mxx-x) - Mxz*Mxy) / DET / 2
	r2 := uc*uc + vc*vc + Mz
	if r2 <= 0 {
		return Circle{}, fmt.Errorf("%w: Taubin radius² is non-positive", ErrCircleFit)
	}
	return Circle{
		Center: Point{X: uc + mx, Y: vc + my},
		Radius: math.Sqrt(r2),
	}, nil
}

// CircleIntersectionPoints returns the points at which the
// boundaries of c1 and c2 cross:
//
//   - two points for two circles that properly overlap
//   - one point when the circles are externally or internally tangent
//   - zero points when the circles are disjoint, one properly
//     contains the other, or the circles are concentric (in which
//     case the "intersection" is either empty or the whole circle,
//     neither of which is representable as a finite point set)
//
// Result points inherit CRS from c1.Center.
func CircleIntersectionPoints(c1, c2 Circle) []Point {
	dx := c2.Center.X - c1.Center.X
	dy := c2.Center.Y - c1.Center.Y
	d := math.Hypot(dx, dy)
	// Concentric — no well-defined intersection point set.
	if d == 0 {
		return nil
	}
	// Disjoint.
	if d > c1.Radius+c2.Radius {
		return nil
	}
	// One properly contains the other.
	if d+math.Min(c1.Radius, c2.Radius) < math.Max(c1.Radius, c2.Radius) {
		return nil
	}
	// Chord-midpoint distance from c1.Center along the c1→c2 direction.
	a := (c1.Radius*c1.Radius - c2.Radius*c2.Radius + d*d) / (2 * d)
	h2 := c1.Radius*c1.Radius - a*a
	if h2 < 0 {
		// Numerical noise near tangent — clip to 0 so we return the
		// single tangent point rather than nothing.
		h2 = 0
	}
	h := math.Sqrt(h2)
	crs := c1.Center.CRSValue
	// Chord midpoint on the line between the centers.
	mx := c1.Center.X + a*dx/d
	my := c1.Center.Y + a*dy/d
	if h == 0 {
		return []Point{{X: mx, Y: my, CRSValue: crs}}
	}
	// Perpendicular unit vector to (dx, dy)/d.
	px := -dy / d
	py := dx / d
	return []Point{
		{X: mx + h*px, Y: my + h*py, CRSValue: crs},
		{X: mx - h*px, Y: my - h*py, CRSValue: crs},
	}
}

// LensPolygon returns the intersection region of c1 and c2 as a
// Polygon, sampled analytically (no sweep-line involved). arcSegments
// is the number of samples PER arc — the returned ring has ~2·
// arcSegments vertices. Values < 4 fall back to
// DefaultBufferSegments / 2.
//
// Special cases:
//   - Disjoint circles → empty Polygon (Rings == nil).
//   - One circle properly contains the other → the smaller circle's
//     Boundary (the lens IS the smaller circle).
//   - Concentric circles with equal radius → the shared circle's
//     Boundary.
//   - Tangent circles → empty Polygon (a lens with zero area).
//
// Output is wound CCW (as gobi's convention prefers for exterior
// rings) and inherits CRS from c1.Center.
func LensPolygon(c1, c2 Circle, arcSegments int) Polygon {
	if arcSegments < 4 {
		arcSegments = DefaultBufferSegments / 2
	}
	dx := c2.Center.X - c1.Center.X
	dy := c2.Center.Y - c1.Center.Y
	d := math.Hypot(dx, dy)
	crs := c1.Center.CRSValue

	// Disjoint (including exact tangent — a zero-area lens is
	// indistinguishable from empty for downstream consumers).
	if d >= c1.Radius+c2.Radius {
		return Polygon{CRSValue: crs}
	}
	// One contains the other → the lens is the smaller circle.
	if d+math.Min(c1.Radius, c2.Radius) <= math.Max(c1.Radius, c2.Radius) {
		if c1.Radius <= c2.Radius {
			return c1.Boundary(2 * arcSegments)
		}
		return c2.Boundary(2 * arcSegments)
	}

	pts := CircleIntersectionPoints(c1, c2)
	if len(pts) < 2 {
		return Polygon{CRSValue: crs}
	}
	i1, i2 := pts[0], pts[1]

	// Arc on c2 from i1 to i2, passing through the direction from
	// c2's center toward c1's center. Then arc on c1 from i2 to i1,
	// passing through the direction from c1's center toward c2's
	// center. Concatenated, they form the closed lens boundary.
	arcOnC2 := sampleArcThrough(c2.Center, c2.Radius, i1, i2, c1.Center, arcSegments+1)
	arcOnC1 := sampleArcThrough(c1.Center, c1.Radius, i2, i1, c2.Center, arcSegments+1)

	ring := make([]Point, 0, len(arcOnC2)+len(arcOnC1))
	ring = append(ring, arcOnC2...)
	// Skip the shared joint vertex (arcOnC1[0] == i2, which is
	// arcOnC2[len-1]).
	ring = append(ring, arcOnC1[1:]...)
	// Ensure closed.
	if ring[0].X != ring[len(ring)-1].X || ring[0].Y != ring[len(ring)-1].Y {
		ring = append(ring, ring[0])
	}
	// Force CCW winding to match the package convention for exterior
	// rings. Reversal is cheap and doesn't affect area.
	if !isCCW(ring) {
		reversePoints(ring)
	}
	return Polygon{Rings: [][]Point{ring}, CRSValue: crs}
}

// sampleArcThrough returns nPoints along the arc of the circle
// (center, radius) from pStart to pEnd, choosing the direction that
// passes through the ray from center toward throughPoint. The first
// and last output points are pStart and pEnd exactly (post-sample
// overwrite handles any float64 reconstruction drift).
//
// Direction picking: normalize the CCW sweep from a_start to a_end
// into [0, 2π). If a_through (also normalized) lies in that CCW
// range, sweep CCW; otherwise sweep CW.
func sampleArcThrough(center Point, radius float64, pStart, pEnd, throughPoint Point, nPoints int) []Point {
	if nPoints < 2 {
		nPoints = 2
	}
	aStart := math.Atan2(pStart.Y-center.Y, pStart.X-center.X)
	aEnd := math.Atan2(pEnd.Y-center.Y, pEnd.X-center.X)
	aThrough := math.Atan2(throughPoint.Y-center.Y, throughPoint.X-center.X)

	ccwDelta := normalizeAngleTwoPi(aEnd - aStart)
	throughDelta := normalizeAngleTwoPi(aThrough - aStart)
	var totalDelta float64
	if throughDelta <= ccwDelta {
		totalDelta = ccwDelta // CCW arc contains aThrough
	} else {
		totalDelta = ccwDelta - 2*math.Pi // sweep CW instead
	}

	out := make([]Point, nPoints)
	for i := range nPoints {
		t := float64(i) / float64(nPoints-1)
		angle := aStart + t*totalDelta
		out[i] = Point{
			X:        center.X + radius*math.Cos(angle),
			Y:        center.Y + radius*math.Sin(angle),
			CRSValue: center.CRSValue,
		}
	}
	// Force endpoints exact — the trigonometric reconstruction can
	// drift by a ULP at the endpoints and downstream Clip / topology
	// code prefers coincident vertices to be bit-exact.
	out[0] = pStart
	out[nPoints-1] = pEnd
	return out
}

// normalizeAngleTwoPi maps a to [0, 2π).
func normalizeAngleTwoPi(a float64) float64 {
	two := 2 * math.Pi
	a = math.Mod(a, two)
	if a < 0 {
		a += two
	}
	return a
}

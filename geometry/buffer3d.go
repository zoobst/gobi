package geometry

import "fmt"

// -----------------------------------------------------------------------------
// 3D buffer shapes — Sphere (Point buffer), Capsule (LineString
// buffer), and expanded ExtrudedPolygon (polygon buffer). All buffer
// operations construct one of these shape types and expose SoA
// point-in-shape kernels for downstream containment queries.
//
// Coordinate frame: shape coordinates are in the input geometry's
// CRS. For projected CRSes that's the linear unit (feet/meters);
// for geographic CRSes, the sphere/capsule are conceptually ECEF-
// meters but the input Point's lon/lat/alt is retained on the
// shape as-is — the ECEF conversion happens inside the
// PointsInSphere kernel at query time. Kept this way so buffers
// round-trip cleanly through geometry.WKB (via a future 3D shape
// codec) without a lossy ECEF flatten.
//
// Buffer semantics: Minkowski sum with a ball of radius r — i.e.
// "every point within distance r of the source shape." Matches
// PostGIS ST_3DBuffer for equivalent inputs.
// -----------------------------------------------------------------------------

// Sphere is a 3D ball: every point within R of (X, Y, Z). Used as
// the buffered shape around a Point.
//
// CRSValue.Projected → (X, Y, Z) are Cartesian in the CRS's linear
// unit; R is in the same unit. Point-in-sphere is one squared-
// distance compare per query point.
//
// Non-projected (geographic) CRS → (X, Y) are lon/lat degrees, Z
// is ellipsoid-height meters, R is meters. Point-in-sphere
// converts both center and query point to ECEF, then compares
// squared distances. Slightly more expensive but honest.
type Sphere struct {
	X, Y, Z  float64
	R        float64
	CRSValue CRS
}

// Capsule is a swept sphere: every point within R of the segment
// (Ax, Ay, Az) → (Bx, By, Bz). Used as the buffered shape around a
// LineString segment. Multi-segment lines produce a slice of
// Capsules; caller ORs the per-capsule containment results.
type Capsule struct {
	Ax, Ay, Az float64
	Bx, By, Bz float64
	R          float64
	CRSValue   CRS
}

// -----------------------------------------------------------------------------
// Buffer3D constructors.
// -----------------------------------------------------------------------------

// Buffer3DPoint returns a Sphere of radius r around p. Requires
// p.HasZ; 2D-only points yield an error (caller who wants a 2D
// buffer disc should Force2D → 2D Buffer via existing planar
// path).
func Buffer3DPoint(p Point, r float64) (Sphere, error) {
	if !p.HasZ {
		return Sphere{}, fmt.Errorf("%w: Buffer3DPoint requires 3D input", ErrTypeMismatch)
	}
	if r < 0 {
		return Sphere{}, fmt.Errorf("geometry: Buffer3DPoint: negative radius %v", r)
	}
	return Sphere{
		X: p.X, Y: p.Y, Z: p.Z,
		R:        r,
		CRSValue: p.CRSValue,
	}, nil
}

// Buffer3DLineString returns one Capsule per segment of ls. Every
// (n-1)-segment line produces n-1 capsules. Empty / single-vertex
// input returns an empty slice with a nil error. 2D-only lines
// yield an error.
//
// Callers evaluating "point within r of line" test each returned
// capsule and OR the results — a helper on top of the raw
// capsules will land in a follow-up.
func Buffer3DLineString(ls LineString, r float64) ([]Capsule, error) {
	if !ls.HasZ {
		return nil, fmt.Errorf("%w: Buffer3DLineString requires 3D input", ErrTypeMismatch)
	}
	if r < 0 {
		return nil, fmt.Errorf("geometry: Buffer3DLineString: negative radius %v", r)
	}
	n := len(ls.Points)
	if n < 2 {
		return nil, nil
	}
	out := make([]Capsule, n-1)
	for i := 0; i < n-1; i++ {
		a, b := ls.Points[i], ls.Points[i+1]
		out[i] = Capsule{
			Ax: a.X, Ay: a.Y, Az: a.Z,
			Bx: b.X, By: b.Y, Bz: b.Z,
			R:        r,
			CRSValue: ls.CRSValue,
		}
	}
	return out, nil
}

// Buffer3DExtrudedPolygon returns an ExtrudedPolygon expanded by
// r on every axis: the footprint is 2D-buffered outward by r
// (falls through to the existing planar Buffer engine — 2D Polygon
// buffer with a round cap), Z range extends by r on both ends.
//
// The composed shape isn't strictly a Minkowski sum with a ball —
// the corners where the top/bottom Z-caps meet the vertical wall
// are square-cornered, not rounded (a true Minkowski sum would
// produce a rounded torus at each edge). For the vast majority of
// "3D containment" use cases the square-cornered approximation is
// indistinguishable and cheaper. Callers who want the fully-round
// shape need to compose ExtrudedPolygon + a set of spherical caps
// at every edge — pending as a follow-up if a real use case
// surfaces.
func Buffer3DExtrudedPolygon(p ExtrudedPolygon, r float64) (ExtrudedPolygon, error) {
	if r < 0 {
		return ExtrudedPolygon{}, fmt.Errorf("geometry: Buffer3DExtrudedPolygon: negative radius %v", r)
	}
	if r == 0 {
		return p, nil
	}
	// 2D-buffer the footprint using the existing planar Buffer
	// engine. Buffer returns a Geometry; assume Polygon output for
	// a Polygon input — the general case may return MultiPolygon
	// for self-intersecting rings, which the day-one path treats
	// as an error (caller reprojects or simplifies first).
	buffered, err := Buffer(p.Footprint, r, BufferOptions{})
	if err != nil {
		return ExtrudedPolygon{}, fmt.Errorf("geometry: Buffer3DExtrudedPolygon: 2D buffer: %w", err)
	}
	poly, ok := buffered.(Polygon)
	if !ok {
		return ExtrudedPolygon{}, fmt.Errorf("geometry: Buffer3DExtrudedPolygon: 2D buffer produced non-Polygon (%T); simplify the footprint first",
			buffered)
	}
	return ExtrudedPolygon{
		Footprint: poly,
		MinZ:      p.MinZ - r,
		MaxZ:      p.MaxZ + r,
		CRSValue:  p.CRSValue,
	}, nil
}

// -----------------------------------------------------------------------------
// SoA point-in-shape kernels.
// -----------------------------------------------------------------------------

// PointsInSphereFromXYZ writes out[i] = true when (xs[i], ys[i],
// zs[i]) is within (or on) the sphere. Coordinate interpretation
// depends on the sphere's CRS (projected → Cartesian; geographic
// → ECEF-Euclidean).
//
// Projected path: vectorized (Δx² + Δy² + Δz²) ≤ R². Zero-alloc.
//
// Geographic path: convert center + all query points to ECEF via
// LonLatAltToECEFSlabs (batched, single scratch alloc), then the
// same squared-distance kernel. Callers can pass a *ECEFScratch
// to reuse buffers across calls.
func PointsInSphereFromXYZ(xs, ys, zs []float64, s Sphere, out []bool, scratch *ECEFScratch) {
	n := min(len(out), minLen3(xs, ys, zs))
	if n == 0 {
		return
	}
	r2 := s.R * s.R
	if s.CRSValue.Projected || s.CRSValue.Zero() {
		for i := 0; i < n; i++ {
			dx := xs[i] - s.X
			dy := ys[i] - s.Y
			dz := zs[i] - s.Z
			out[i] = (dx*dx + dy*dy + dz*dz) <= r2
		}
		return
	}
	// Geographic path — batch ECEF conversion.
	var local ECEFScratch
	if scratch == nil {
		scratch = &local
	}
	scratch.reset(n)
	LonLatAltToECEFSlabs(xs[:n], ys[:n], zs[:n], scratch.X1, scratch.Y1, scratch.Z1)
	// Center → ECEF (single point).
	cLons := [1]float64{s.X}
	cLats := [1]float64{s.Y}
	cAlts := [1]float64{s.Z}
	var cX, cY, cZ [1]float64
	LonLatAltToECEFSlabs(cLons[:], cLats[:], cAlts[:], cX[:], cY[:], cZ[:])
	for i := range n {
		dx := scratch.X1[i] - cX[0]
		dy := scratch.Y1[i] - cY[0]
		dz := scratch.Z1[i] - cZ[0]
		out[i] = (dx*dx + dy*dy + dz*dz) <= r2
	}
}

// PointsInCapsuleFromXYZ writes out[i] = true when (xs[i], ys[i],
// zs[i]) is within (or on) the capsule — i.e. within the capsule's
// R of the segment (A, B). Standard clamped-projection formula:
// project the query point onto the line AB, clamp t to [0, 1],
// compute the residual squared distance, compare to R².
//
// Projected only in the day-one implementation. Geographic capsule
// containment (ECEF-project the segment endpoints and every query
// point) is a follow-up — the shape isn't a straight segment in
// ECEF (great-circle over the ground); needs a proper geodesic-
// segment formulation. Callers who need geographic 3D "within r
// of a line" today should sample the line to sub-r spacing and
// use Sphere-per-vertex containment.
func PointsInCapsuleFromXYZ(xs, ys, zs []float64, c Capsule, out []bool) {
	n := min(len(out), minLen3(xs, ys, zs))
	if n == 0 {
		return
	}
	if !c.CRSValue.Projected && !c.CRSValue.Zero() {
		// Zero out for now; docstring flags this as follow-up.
		// Alternative: panic. Silent-false is safer for the
		// day-one "wire everything up" path — geographic callers
		// see empty results, not corrupt ones.
		for i := range n {
			out[i] = false
		}
		return
	}
	abx := c.Bx - c.Ax
	aby := c.By - c.Ay
	abz := c.Bz - c.Az
	segLen2 := abx*abx + aby*aby + abz*abz
	r2 := c.R * c.R
	if segLen2 == 0 {
		// Degenerate capsule — collapses to a sphere at A.
		for i := range n {
			dx := xs[i] - c.Ax
			dy := ys[i] - c.Ay
			dz := zs[i] - c.Az
			out[i] = (dx*dx + dy*dy + dz*dz) <= r2
		}
		return
	}
	invSegLen2 := 1.0 / segLen2
	for i := range n {
		apx := xs[i] - c.Ax
		apy := ys[i] - c.Ay
		apz := zs[i] - c.Az
		// t = clamp((AP · AB) / |AB|², 0, 1)
		t := (apx*abx + apy*aby + apz*abz) * invSegLen2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		// Nearest point on segment: A + t * AB
		nx := c.Ax + t*abx
		ny := c.Ay + t*aby
		nz := c.Az + t*abz
		dx := xs[i] - nx
		dy := ys[i] - ny
		dz := zs[i] - nz
		out[i] = (dx*dx + dy*dy + dz*dz) <= r2
	}
}

// PointsInAnyCapsuleFromXYZ writes out[i] = true when (xs[i],
// ys[i], zs[i]) is inside ANY of the given capsules. Convenience
// for "point within r of a multi-segment LineString buffer."
// Iterates capsules with an early-exit on first hit per point.
func PointsInAnyCapsuleFromXYZ(xs, ys, zs []float64, capsules []Capsule, out []bool) {
	n := min(len(out), minLen3(xs, ys, zs))
	if n == 0 || len(capsules) == 0 {
		for i := range n {
			out[i] = false
		}
		return
	}
	scratch := make([]bool, n)
	// First capsule — write directly to out.
	PointsInCapsuleFromXYZ(xs[:n], ys[:n], zs[:n], capsules[0], out[:n])
	for _, c := range capsules[1:] {
		PointsInCapsuleFromXYZ(xs[:n], ys[:n], zs[:n], c, scratch)
		for i := range n {
			out[i] = out[i] || scratch[i]
		}
	}
}

package geometry

import "math"

// -----------------------------------------------------------------------------
// 3D geodesic math — ECEF conversion + slab-form distance kernels.
//
// The primary shape here is SoA: kernels take parallel Xs/Ys/Zs
// float64 slabs (or lons/lats/alts for geographic inputs) and write
// into caller-provided output slabs. Point-shaped scalar wrappers
// (Point.Distance3D, Point.Force2D, Point.ForceZ) dispatch into
// these kernels with N=1 slabs — the AoS methods are the wrapper,
// not the primary. This matches gobi's Slice-6/8/9 SoA convention
// so Series-level Geom3D fast paths never allocate []Point.
//
// The math:
//
//   ECEF (Earth-Centered Earth-Fixed) is the WGS84 Cartesian frame.
//   For a point (lon, lat, alt) in degrees + ellipsoid height:
//
//     N = a / sqrt(1 - e² * sin²(lat))       // prime vertical radius
//     X = (N + alt) * cos(lat) * cos(lon)
//     Y = (N + alt) * cos(lat) * sin(lon)
//     Z = (N * (1 - e²) + alt) * sin(lat)
//
//   where a = semi-major axis, e² = squared first eccentricity.
//   Distance is Euclidean in ECEF: sqrt(ΔX² + ΔY² + ΔZ²) in meters.
//
//   For any two points this returns the straight-line distance
//   through the Earth (chord distance), not the arc distance along
//   the surface. That's the right thing for 3D applications
//   (aviation, drones, subsurface) — surface-only callers should
//   use Haversine (2D) instead.
// -----------------------------------------------------------------------------

// Ellipsoid parameterizes a reference ellipsoid for ECEF conversion.
// WGS84 is the day-one default; the Ellipsoid type is exposed so a
// future release can plumb this out to callers without an API break.
type Ellipsoid struct {
	// A is the semi-major axis in meters (Earth's equatorial radius
	// for WGS84).
	A float64
	// FInv is the inverse flattening (1 / f). Semi-minor axis is
	// A * (1 - 1/FInv).
	FInv float64
}

// WGS84Ellipsoid holds the WGS84 constants. Values from the WGS84
// standard (EPSG:4326 / EPSG:7030).
var WGS84Ellipsoid = Ellipsoid{
	A:    6378137.0,
	FInv: 298.257223563,
}

// eSquared returns the squared first eccentricity e² = 2f - f² where
// f = 1/FInv. Precomputed on the ellipsoid so the ECEF conversion
// loop doesn't recompute per iteration.
func (e Ellipsoid) eSquared() float64 {
	f := 1.0 / e.FInv
	return 2*f - f*f
}

// LonLatAltToECEFSlabs converts N geographic points (lons, lats in
// degrees; alts in meters, ellipsoid height) to WGS84 ECEF (X, Y, Z)
// meters. Writes into caller-provided output slabs — matches
// BoundsFromXY's slab-in/slab-out shape.
//
// All input and output slabs must have length ≥ N (where N = min of
// input lengths). The hot loop does one sin/cos pair per row + a
// handful of multiplies, all vectorizable by the Go compiler.
//
// Zero-alloc. Callers can reuse output slabs across calls by
// wrapping them in an ECEFScratch (see the Series-level dispatchers).
func LonLatAltToECEFSlabs(lons, lats, alts, outX, outY, outZ []float64) {
	n := len(lons)
	if len(lats) < n {
		n = len(lats)
	}
	if len(alts) < n {
		n = len(alts)
	}
	e2 := WGS84Ellipsoid.eSquared()
	a := WGS84Ellipsoid.A
	deg := math.Pi / 180
	for i := range n {
		latRad := lats[i] * deg
		lonRad := lons[i] * deg
		sinLat, cosLat := math.Sincos(latRad)
		sinLon, cosLon := math.Sincos(lonRad)
		N := a / math.Sqrt(1-e2*sinLat*sinLat)
		nph := N + alts[i]
		outX[i] = nph * cosLat * cosLon
		outY[i] = nph * cosLat * sinLon
		outZ[i] = (N*(1-e2) + alts[i]) * sinLat
	}
}

// Distance3DProjectedFromSlabs writes the pair-wise 3D Cartesian
// Euclidean distance from (xs1[i], ys1[i], zs1[i]) to (xs2[i],
// ys2[i], zs2[i]) into out[i]. All coordinates in the same linear
// unit (typically the CRS's linear unit).
//
// Zero-alloc. The `sqrt` is unavoidable per row; the compiler
// auto-vectorizes the multiply-adds on both amd64 and arm64. For
// callers who only need squared distance (rank comparisons), a
// _SqFromSlabs variant would be a straight subtract-and-write.
func Distance3DProjectedFromSlabs(xs1, ys1, zs1, xs2, ys2, zs2, out []float64) {
	n := minLen6(xs1, ys1, zs1, xs2, ys2, zs2, out)
	for i := range n {
		dx := xs2[i] - xs1[i]
		dy := ys2[i] - ys1[i]
		dz := zs2[i] - zs1[i]
		out[i] = math.Sqrt(dx*dx + dy*dy + dz*dz)
	}
}

// ECEFScratch is a reusable per-call buffer for the geographic-3D
// distance path. Callers holding a `*ECEFScratch` avoid the six
// N-float64 slabs the geodesic path would otherwise allocate on
// each call. Series-level dispatchers pool these via sync.Pool.
//
// Grow to fit an N-row call by calling reset(n) — reallocates only
// when the current capacity is short.
type ECEFScratch struct {
	X1, Y1, Z1 []float64
	X2, Y2, Z2 []float64
}

// reset resizes every slab in s to length n, growing capacity if
// needed. Existing capacity is preserved on shrink — same shape as
// the standard append-and-truncate pool pattern.
func (s *ECEFScratch) reset(n int) {
	s.X1 = growSlab(s.X1, n)
	s.Y1 = growSlab(s.Y1, n)
	s.Z1 = growSlab(s.Z1, n)
	s.X2 = growSlab(s.X2, n)
	s.Y2 = growSlab(s.Y2, n)
	s.Z2 = growSlab(s.Z2, n)
}

// Distance3DGeodesicFromSlabs writes the pair-wise ECEF Euclidean
// distance (in meters) from geographic point A_i (lon, lat, alt)
// to geographic point B_i (lon, lat, alt) into out[i].
//
// The math is: convert both sides to ECEF via the WGS84 ellipsoid,
// then straight-line 3D distance. Result is the chord (through-
// Earth) distance in meters. For altitude-agnostic surface arc
// distance, callers want Haversine (2D) or a geodesic-arc variant
// instead.
//
// scratch may be nil — in which case the function allocates its
// own six-slab buffer per call. Series-level callers hitting this
// on N=1M row columns should pass a pooled scratch to avoid the
// per-call GC pressure.
//
// All input slabs must have length ≥ N (min of input lengths).
// out must also have length ≥ N.
func Distance3DGeodesicFromSlabs(
	lons1, lats1, alts1, lons2, lats2, alts2 []float64,
	out []float64,
	scratch *ECEFScratch,
) {
	n := minLen6(lons1, lats1, alts1, lons2, lats2, alts2, out)
	if n == 0 {
		return
	}
	var local ECEFScratch
	if scratch == nil {
		scratch = &local
	}
	scratch.reset(n)
	LonLatAltToECEFSlabs(lons1[:n], lats1[:n], alts1[:n], scratch.X1, scratch.Y1, scratch.Z1)
	LonLatAltToECEFSlabs(lons2[:n], lats2[:n], alts2[:n], scratch.X2, scratch.Y2, scratch.Z2)
	Distance3DProjectedFromSlabs(scratch.X1, scratch.Y1, scratch.Z1, scratch.X2, scratch.Y2, scratch.Z2, out[:n])
}

// Distance3DProjectedFromSlabsToPoint writes the pair-wise 3D
// Cartesian Euclidean distance from each row (xs[i], ys[i], zs[i])
// to a single scalar target (tx, ty, tz) into out[i]. Scalar-target
// companion to Distance3DProjectedFromSlabs — call this when one
// side is a broadcast constant to skip the O(N) broadcast slab
// alloc the symmetric kernel would otherwise force.
//
// Zero-alloc. Compiler auto-vectorizes the same subtract-mul-add
// pattern.
func Distance3DProjectedFromSlabsToPoint(xs, ys, zs []float64, tx, ty, tz float64, out []float64) {
	n := min(len(out), minLen3(xs, ys, zs))
	for i := range n {
		dx := xs[i] - tx
		dy := ys[i] - ty
		dz := zs[i] - tz
		out[i] = math.Sqrt(dx*dx + dy*dy + dz*dz)
	}
}

// Distance3DGeodesicFromSlabsToPoint writes the pair-wise ECEF
// Euclidean distance from each geographic row (lons[i], lats[i],
// alts[i]) to a single scalar geographic target (tlon, tlat, talt)
// into out[i] in meters. Scalar-target companion to
// Distance3DGeodesicFromSlabs.
//
// Converts the target to ECEF once via a 1-element slab call, then
// converts each row via a batched N-slab call, then Cartesian
// distance to the constant. Skips the 3 * N broadcast-slab alloc
// the symmetric-kernel path would otherwise force.
//
// scratch may be nil; when provided, only the ECEF scratch buffers
// for the row side are reused across calls — the target ECEF triple
// lives on the stack via 1-element slabs.
func Distance3DGeodesicFromSlabsToPoint(
	lons, lats, alts []float64,
	tlon, tlat, talt float64,
	out []float64,
	scratch *ECEFScratch,
) {
	n := min(len(out), minLen3(lons, lats, alts))
	if n == 0 {
		return
	}
	var local ECEFScratch
	if scratch == nil {
		scratch = &local
	}
	scratch.reset(n)
	LonLatAltToECEFSlabs(lons[:n], lats[:n], alts[:n], scratch.X1, scratch.Y1, scratch.Z1)
	tLons := [1]float64{tlon}
	tLats := [1]float64{tlat}
	tAlts := [1]float64{talt}
	var tX, tY, tZ [1]float64
	LonLatAltToECEFSlabs(tLons[:], tLats[:], tAlts[:], tX[:], tY[:], tZ[:])
	Distance3DProjectedFromSlabsToPoint(scratch.X1, scratch.Y1, scratch.Z1, tX[0], tY[0], tZ[0], out[:n])
}

// LineString3DProjectedLengthFromXYZ returns the arc length of a
// LineString whose vertices are held in parallel Xs/Ys/Zs slabs, in
// the coordinate frame's linear unit. Sum of segment Euclidean
// distances; no per-segment allocation.
//
// For an n-vertex line, walks n-1 segments. Empty / single-vertex
// input returns 0. Callers get to keep the SoA slab layout end-to-
// end — no []Point intermediate materialized here.
func LineString3DProjectedLengthFromXYZ(xs, ys, zs []float64) float64 {
	n := minLen3(xs, ys, zs)
	if n < 2 {
		return 0
	}
	var total float64
	for i := 1; i < n; i++ {
		dx := xs[i] - xs[i-1]
		dy := ys[i] - ys[i-1]
		dz := zs[i] - zs[i-1]
		total += math.Sqrt(dx*dx + dy*dy + dz*dz)
	}
	return total
}

// LineString3DGeodesicLengthFromXYZ returns the arc length of a
// geographic 3D LineString (lon/lat/alt vertex slabs) in meters.
// Converts every vertex to ECEF once, then sums straight-line
// segment distances between consecutive ECEF points.
//
// The "geodesic" naming refers to the ECEF-through-Earth path — not
// a great-circle arc along the surface. For sufficiently short
// segments (a few km) the chord ≈ the arc, so the sum is a close
// approximation of the true surface + altitude length. For long
// segments callers who want the true geodesic-arc length should
// sample-then-sum via SampleGeodesic3D.
//
// scratch is optional; same pooling shape as
// Distance3DGeodesicFromSlabs.
func LineString3DGeodesicLengthFromXYZ(lons, lats, alts []float64, scratch *ECEFScratch) float64 {
	n := minLen3(lons, lats, alts)
	if n < 2 {
		return 0
	}
	var local ECEFScratch
	if scratch == nil {
		scratch = &local
	}
	scratch.reset(n)
	// Reuse X1/Y1/Z1 as the whole line's ECEF slab; X2/Y2/Z2 unused
	// here. The reset above sized them anyway — no extra alloc.
	LonLatAltToECEFSlabs(lons[:n], lats[:n], alts[:n], scratch.X1, scratch.Y1, scratch.Z1)
	var total float64
	for i := 1; i < n; i++ {
		dx := scratch.X1[i] - scratch.X1[i-1]
		dy := scratch.Y1[i] - scratch.Y1[i-1]
		dz := scratch.Z1[i] - scratch.Z1[i-1]
		total += math.Sqrt(dx*dx + dy*dy + dz*dz)
	}
	return total
}

// SampleGeodesic3DFromSlabs writes N intermediate samples along the
// path from (lon1, lat1, alt1) to (lon2, lat2, alt2), inclusive of
// both endpoints. Horizontal interpolation follows a great-circle
// arc; altitude interpolates linearly between the two ellipsoid
// heights.
//
// n must be ≥ 2 (endpoints); n=2 returns just the two endpoints.
// outLons/outLats/outAlts must have length ≥ n.
//
// Uses the SLERP (spherical linear interpolation) formulation on
// unit vectors derived from the endpoint lat/lon pairs — robust
// even for antipodal endpoints (where the arc is undefined; the
// output smoothly degenerates to one of the great-circle poles).
func SampleGeodesic3DFromSlabs(
	lon1, lat1, alt1, lon2, lat2, alt2 float64, n int,
	outLons, outLats, outAlts []float64,
) {
	if n < 2 {
		return
	}
	deg := math.Pi / 180
	// Endpoint unit vectors on the unit sphere.
	sinLat1, cosLat1 := math.Sincos(lat1 * deg)
	sinLon1, cosLon1 := math.Sincos(lon1 * deg)
	sinLat2, cosLat2 := math.Sincos(lat2 * deg)
	sinLon2, cosLon2 := math.Sincos(lon2 * deg)
	x1 := cosLat1 * cosLon1
	y1 := cosLat1 * sinLon1
	z1 := sinLat1
	x2 := cosLat2 * cosLon2
	y2 := cosLat2 * sinLon2
	z2 := sinLat2
	// Central angle via dot product; clamp for numerical safety
	// against |cos| slightly > 1 when endpoints coincide.
	dot := x1*x2 + y1*y2 + z1*z2
	if dot > 1 {
		dot = 1
	} else if dot < -1 {
		dot = -1
	}
	omega := math.Acos(dot)
	sinOmega := math.Sin(omega)
	radToDeg := 180.0 / math.Pi
	for i := range n {
		t := float64(i) / float64(n-1)
		var a, b float64
		if sinOmega < 1e-12 {
			// Endpoints coincide or nearly so — linear blend on the
			// unit vectors is safe.
			a = 1 - t
			b = t
		} else {
			a = math.Sin((1-t)*omega) / sinOmega
			b = math.Sin(t*omega) / sinOmega
		}
		x := a*x1 + b*x2
		y := a*y1 + b*y2
		z := a*z1 + b*z2
		outLats[i] = math.Asin(z) * radToDeg
		outLons[i] = math.Atan2(y, x) * radToDeg
		outAlts[i] = alt1 + (alt2-alt1)*t
	}
}

// -----------------------------------------------------------------------------
// Slab helpers — keep these here (not in a shared file) to keep the
// 3D module self-contained.
// -----------------------------------------------------------------------------

func minLen3(a, b, c []float64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if len(c) < n {
		n = len(c)
	}
	return n
}

func minLen6(a, b, c, d, e, f, g []float64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if len(c) < n {
		n = len(c)
	}
	if len(d) < n {
		n = len(d)
	}
	if len(e) < n {
		n = len(e)
	}
	if len(f) < n {
		n = len(f)
	}
	if len(g) < n {
		n = len(g)
	}
	return n
}

// growSlab returns s resized to exactly length n. Reuses the backing
// array when cap(s) ≥ n; otherwise allocates a fresh slab. Match's
// the sync.Pool-friendly grow shape used elsewhere in the codebase.
func growSlab(s []float64, n int) []float64 {
	if cap(s) >= n {
		return s[:n]
	}
	return make([]float64, n)
}


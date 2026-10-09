package geometry

import (
	"fmt"
	"math"
)

// -----------------------------------------------------------------------------
// ExtrudedPolygon — a 2D polygon footprint extended vertically between
// MinZ and MaxZ, forming a prism. Covers ~90% of practical 3D GIS
// workloads (buildings, airspace volumes, bathymetric prisms) without
// requiring a mesh / polyhedra library.
//
// Point-in-prism: 2D point-in-polygon (footprint) AND minZ ≤ z ≤ maxZ.
// Prism-prism intersects: 2D bbox intersects (footprint) AND Z range
// overlap. Both compose over the existing SoA kernels (PIPRingFromXY,
// Bounds.Intersects) so no new hot-path code — just Z-band guards.
// -----------------------------------------------------------------------------

// ExtrudedPolygon is a 2D Polygon footprint × [MinZ, MaxZ] vertical
// extent. The footprint's rings are XY-only; Z lives on the prism
// container. MinZ ≤ MaxZ; NewExtrudedPolygon errors on inverted Z
// range and empty footprint. Callers building the struct directly
// (bypassing NewExtrudedPolygon) skip validation — an inverted
// prism silently matches nothing via the Z band-pass check inside
// Contains3D / PointsInPrismFromXYZ.
//
// CRSValue is the CRS of the footprint coordinates AND the Z axis
// linear unit. Projected CRSes carry Z in the same linear unit as
// X/Y (feet, meters). Geographic CRSes carry Z as ellipsoid-height
// meters (WGS84 convention).
type ExtrudedPolygon struct {
	Footprint Polygon
	MinZ      float64
	MaxZ      float64
	CRSValue  CRS
}

// NewExtrudedPolygon returns an ExtrudedPolygon with the given
// footprint and Z range. Errors when MinZ > MaxZ or when the
// footprint is empty (no exterior ring).
func NewExtrudedPolygon(footprint Polygon, minZ, maxZ float64) (ExtrudedPolygon, error) {
	if minZ > maxZ {
		return ExtrudedPolygon{}, fmt.Errorf("geometry: ExtrudedPolygon: MinZ (%v) > MaxZ (%v)", minZ, maxZ)
	}
	if len(footprint.Rings) == 0 {
		return ExtrudedPolygon{}, fmt.Errorf("geometry: ExtrudedPolygon: footprint has no rings")
	}
	return ExtrudedPolygon{
		Footprint: footprint,
		MinZ:      minZ,
		MaxZ:      maxZ,
		CRSValue:  footprint.CRSValue,
	}, nil
}

// Bounds3D is an axis-aligned 3D bounding box. Zero value is
// EmptyBounds3D (MinX > MaxX), matching the 2D Bounds convention so
// nil-frame R-tree slots stay filterable.
type Bounds3D struct {
	MinX, MinY, MinZ float64
	MaxX, MaxY, MaxZ float64
}

// EmptyBounds3D returns a sentinel 3D bounds whose MinX > MaxX. Used
// by callers (SJoin3D nil-row slots, unpopulated builders) that need
// a "definitely empty, no query touches it" marker.
func EmptyBounds3D() Bounds3D {
	return Bounds3D{
		MinX: +1, MinY: +1, MinZ: +1,
		MaxX: -1, MaxY: -1, MaxZ: -1,
	}
}

// Empty reports whether b is the empty sentinel.
func (b Bounds3D) Empty() bool {
	return b.MinX > b.MaxX || b.MinY > b.MaxY || b.MinZ > b.MaxZ
}

// Contains reports whether (x, y, z) lies within (or on the boundary
// of) b. Upper-inclusive on every axis; matches Bounds.Contains.
func (b Bounds3D) Contains(x, y, z float64) bool {
	return b.MinX <= x && x <= b.MaxX &&
		b.MinY <= y && y <= b.MaxY &&
		b.MinZ <= z && z <= b.MaxZ
}

// Intersects reports whether b overlaps o. Empty inputs return false.
// Upper-inclusive on every axis to match Bounds.Intersects.
func (b Bounds3D) Intersects(o Bounds3D) bool {
	if b.Empty() || o.Empty() {
		return false
	}
	return b.MinX <= o.MaxX && o.MinX <= b.MaxX &&
		b.MinY <= o.MaxY && o.MinY <= b.MaxY &&
		b.MinZ <= o.MaxZ && o.MinZ <= b.MaxZ
}

// Bounds3D returns p's 3D axis-aligned bounding box. The 2D bbox
// comes from the footprint; the Z axis comes from p's MinZ/MaxZ.
func (p ExtrudedPolygon) Bounds3D() Bounds3D {
	b := p.Footprint.Bounds()
	if b.Empty() {
		return EmptyBounds3D()
	}
	return Bounds3D{
		MinX: b.MinX, MinY: b.MinY, MinZ: p.MinZ,
		MaxX: b.MaxX, MaxY: b.MaxY, MaxZ: p.MaxZ,
	}
}

// Contains3D reports whether a single point (x, y, z) lies inside
// (or on the boundary of) p. Composes: 2D point-in-polygon on the
// footprint AND minZ ≤ z ≤ maxZ.
//
// For batch queries (N points × 1 prism) prefer PointsInPrismFromXYZ
// — that path skips the per-call ring-view materialization and
// composes with SoA slabs end-to-end.
func (p ExtrudedPolygon) Contains3D(x, y, z float64) bool {
	if z < p.MinZ || z > p.MaxZ {
		return false
	}
	rings := p.Footprint.RingViews()
	return PIPPolygonFromRings(rings, x, y)
}

// Intersects3D reports whether p and o's prisms share any point.
// Two-stage: 2D bounding-box intersect on the footprints AND Z
// range overlap. This is the necessary condition (bboxes) + the
// Z-axis specific check — for footprints that share the 2D bbox
// but are geometrically disjoint (e.g. two L-shapes in the same
// bbox that don't overlap), the current implementation returns
// true (over-report). Callers who need exact 2D intersect testing
// should compose Bounds.Intersects with the existing 2D
// Boolean(a.Footprint, b.Footprint, OpIntersection) — pending a
// dedicated 2D-polygon-intersects predicate that skips result
// materialization.
func (p ExtrudedPolygon) Intersects3D(o ExtrudedPolygon) bool {
	if p.MaxZ < o.MinZ || o.MaxZ < p.MinZ {
		return false
	}
	return p.Footprint.Bounds().Intersects(o.Footprint.Bounds())
}

// PointsInPrismFromXYZ writes out[i] = true when (xs[i], ys[i],
// zs[i]) lies inside prism. Zero-alloc: reuses the prism's ring
// views once (materialized here, once, not per row) and runs the
// existing SoA PIPPolygonFromRings over each point.
//
// The Z-range check is a vectorized band-pass over the zs slab
// (compiler auto-vectorizes the comparison); the 2D PIP path is
// the same slab kernel Series.GeomContains already uses.
//
// All input slabs must have length ≥ N (min of input lengths);
// out must have length ≥ N. The kernel doesn't allocate — ring
// materialization happens once at the prism boundary, before the
// loop.
func PointsInPrismFromXYZ(xs, ys, zs []float64, prism ExtrudedPolygon, out []bool) {
	n := min(len(out), minLen3(xs, ys, zs))
	if n == 0 {
		return
	}
	rings := prism.Footprint.RingViews()
	for i := 0; i < n; i++ {
		if zs[i] < prism.MinZ || zs[i] > prism.MaxZ {
			out[i] = false
			continue
		}
		out[i] = PIPPolygonFromRings(rings, xs[i], ys[i])
	}
}

// PrismsIntersectFromBounds writes out[i] = true when the i-th
// prism (identified by parallel bounds slabs) intersects the
// query 3D bbox. All ops are vectorized band-pass comparisons —
// SoA-friendly, no per-row branching beyond the six bounds checks.
//
// Same over-approximation contract as ExtrudedPolygon.Intersects3D:
// 2D bbox intersect + Z overlap, not full 2D polygon intersect.
// Callers refining after this cheap filter can compose with the
// existing 2D geometry primitives.
func PrismsIntersectFromBounds(
	minXs, minYs, minZs, maxXs, maxYs, maxZs []float64,
	qMinX, qMinY, qMinZ, qMaxX, qMaxY, qMaxZ float64,
	out []bool,
) {
	n := len(minXs)
	for _, s := range [][]float64{minYs, minZs, maxXs, maxYs, maxZs} {
		n = min(n, len(s))
	}
	n = min(n, len(out))
	for i := 0; i < n; i++ {
		out[i] = minXs[i] <= qMaxX && qMinX <= maxXs[i] &&
			minYs[i] <= qMaxY && qMinY <= maxYs[i] &&
			minZs[i] <= qMaxZ && qMinZ <= maxZs[i]
	}
}

// -----------------------------------------------------------------------------
// 3D convex hull — prism approximation.
//
// True 3D convex hull of an arbitrary point cloud is a polyhedron
// (list of triangles connected by shared edges), requiring a
// quickhull-3D or gift-wrapping implementation with careful
// numerical robustness on degenerate coplanar / collinear inputs.
// That's a several-hundred-line project on its own; not day-one
// scope.
//
// Day-one shape: compute the 2D convex hull of the XY-projected
// points (via the existing SoA ConvexHullFromXY), wrap it in an
// ExtrudedPolygon spanning [min(zs), max(zs)]. This is a
// **super-set** of the true 3D convex hull — the true hull's top
// and bottom faces slope with the Z-varying extremes, whereas the
// prism has flat parallel top/bottom faces.
//
// Practical use cases that benefit from this shape:
//   - LiDAR ground-return hulls (Z variance is small; approximation
//     is exact).
//   - Building footprints tagged with elevation.
//   - Admin boundaries with a single ceiling altitude.
//
// Callers who need the true polyhedral hull should implement it out
// of tree today; a future revision may add ConvexHull3DPolyhedron.
// -----------------------------------------------------------------------------

// Hull3D is the return type of the 3D convex hull family. In the
// day-one prism approximation it always carries a Prism; a future
// polyhedron path would populate Polyhedron. Callers can pattern-
// match on which field is non-nil to know which shape they got.
type Hull3D struct {
	// Prism is the extruded 2D-hull-of-XY prism, populated by
	// ConvexHull3DFromXYZ. Never nil in current shipping code.
	Prism *ExtrudedPolygon

	// Polyhedron is reserved for future true-3D-convex-hull output.
	// Always nil today; leaving the field so callers can pattern-
	// match without a breaking change when the polyhedron path
	// lands.
	Polyhedron *Polyhedron3D
}

// Polyhedron3D is a placeholder for the future true-3D convex hull
// return type — a mesh of triangular faces indexed into a shared
// vertex slab. Kept as a named type (not `any`) so callers writing
// against Hull3D.Polyhedron get useful compile-time signals when
// the type gains fields in a later release.
type Polyhedron3D struct {
	// Vs are the vertex coordinate slabs (SoA). Faces index into
	// these by vertex position.
	Xs, Ys, Zs []float64
	// Faces are triangle vertex-index triples, three int32 per
	// triangle. Length is a multiple of 3.
	Faces []int32
	// CRSValue carries through from the input.
	CRSValue CRS
}

// ConvexHull3DFromXYZ returns the extruded-prism approximation of
// the 3D convex hull of the point set carried in parallel Xs/Ys/Zs
// slabs. Uses the SoA 2D convex hull (ConvexHullFromXY) on the
// XY-projected points; wraps the result in an ExtrudedPolygon
// spanning [min(zs), max(zs)].
//
// The returned Hull3D has Prism populated and Polyhedron nil.
//
// # Semantics
//
// The returned prism is a SUPER-SET of the true 3D convex hull —
// it strictly contains every input point AND every point of the
// true 3D hull, plus the corners where the flat top/bottom faces
// meet the extruded vertical walls of the 2D XY-hull. For point
// sets with small Z variance (common LiDAR / footprint-with-
// elevation shapes) the approximation is exact or nearly so; for
// point sets with large Z variance the approximation over-reports
// interior volume noticeably.
//
// # Edge cases
//
//   - Fewer than 3 input points: returned prism has a degenerate
//     footprint (matches ConvexHullFromXY's behavior on <3 points).
//     Callers should IsEmpty-check the resulting Prism.Footprint
//     before use.
//   - All-Z-equal input: the prism collapses to MinZ == MaxZ, i.e.
//     a flat 2D shape at that Z. Contains3D still works
//     correctly (upper-inclusive bounds check).
//   - Empty input: returned Prism is nil (Hull3D{} zero value with
//     no Prism); callers should nil-check.
func ConvexHull3DFromXYZ(xs, ys, zs []float64, crs CRS) Hull3D {
	n := minLen3(xs, ys, zs)
	if n == 0 {
		return Hull3D{}
	}
	hullXs, hullYs := ConvexHullFromXY(xs[:n], ys[:n])
	// A valid polygon ring needs at least 3 unique vertices; the
	// closed-ring form (first == last) requires 4 slots. Empty /
	// collinear / 2-vertex hulls would produce an invalid Polygon
	// that PIPPolygonFromRings can't walk — return the empty
	// Hull3D sentinel so callers detect via `h.Prism == nil`.
	if len(hullXs) < 4 {
		return Hull3D{}
	}
	minZ, maxZ := zRange(zs[:n])
	// Build the footprint Polygon from the hull's SoA output. The
	// hull is a single closed ring; wrap it in a []Point ring for
	// Polygon compatibility. Not a purely SoA representation, but
	// Polygon is the type Buffer / Intersects consume — the SoA
	// win lives in the hull-construction kernel, not the boundary.
	pts := make([]Point, len(hullXs))
	for i, x := range hullXs {
		pts[i] = Point{X: x, Y: hullYs[i]}
	}
	footprint := Polygon{
		Rings:    [][]Point{pts},
		CRSValue: crs,
	}
	prism := ExtrudedPolygon{
		Footprint: footprint,
		MinZ:      minZ,
		MaxZ:      maxZ,
		CRSValue:  crs,
	}
	return Hull3D{Prism: &prism}
}

// Contains3D reports whether (x, y, z) lies inside (or on the
// boundary of) the hull. Delegates to the concrete shape currently
// held (Prism or, in a future release, Polyhedron).
func (h Hull3D) Contains3D(x, y, z float64) bool {
	if h.Prism != nil {
		return h.Prism.Contains3D(x, y, z)
	}
	// Polyhedron path not implemented; behave as "empty hull
	// contains nothing" rather than crashing.
	return false
}

// PointsInHull3DFromXYZ writes out[i] = true when (xs[i], ys[i],
// zs[i]) is contained by h. Delegates to the prism SoA kernel for
// the current shape. Same over-approximation caveat as
// ConvexHull3DFromXYZ.
func PointsInHull3DFromXYZ(xs, ys, zs []float64, h Hull3D, out []bool) {
	if h.Prism != nil {
		PointsInPrismFromXYZ(xs, ys, zs, *h.Prism, out)
		return
	}
	n := min(len(out), minLen3(xs, ys, zs))
	for i := range n {
		out[i] = false
	}
}

// zRange returns the min/max of a []float64 slab. NaN/NaN on empty
// input — callers on the ConvexHull3DFromXYZ hot path guard against
// empty upstream, so the NaN return is a defensive sentinel rather
// than a normal control-flow signal.
func zRange(zs []float64) (minZ, maxZ float64) {
	if len(zs) == 0 {
		return math.NaN(), math.NaN()
	}
	minZ, maxZ = zs[0], zs[0]
	for _, z := range zs[1:] {
		minZ = min(minZ, z)
		maxZ = max(maxZ, z)
	}
	return minZ, maxZ
}

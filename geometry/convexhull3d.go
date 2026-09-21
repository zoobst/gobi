package geometry

import "math"

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

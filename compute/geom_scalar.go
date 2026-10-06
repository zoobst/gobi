//go:build !goexperiment.simd || (!arm64 && !amd64)

// Scalar geometry kernels — BoundsF64 (Phase 6a),
// PolygonCentroidShoelace (Phase 6b), and PIPCrossingCount
// (Slice 8). Active on default builds and on architectures where
// the SIMD experiment isn't wired up. Same signatures as
// geom_simd.go — callers get identical behavior and semantics,
// only throughput changes.

package compute

// BoundsF64 computes the axis-aligned bounding box of the points
// held in parallel Xs / Ys slices. Returns (minX, minY, maxX,
// maxY, ok=true) when both slices are non-empty; ok=false when
// either is empty. Mismatched slice lengths are caller error;
// the kernel derives bounds from the shorter slice.
//
// Semantics match geometry.BoundsFromXY exactly. This is the
// scalar back-end; the SIMD version in geom_simd.go uses
// simd.Float64s.Min / .Max for a ~3-4× throughput win on large
// input.
func BoundsF64(xs, ys []float64) (minX, minY, maxX, maxY float64, ok bool) {
	if len(xs) == 0 || len(ys) == 0 {
		return 0, 0, 0, 0, false
	}
	n := len(xs)
	if len(ys) < n {
		n = len(ys)
	}
	minX, minY, maxX, maxY = boundsF64ScalarNaN(xs, ys, n)
	return minX, minY, maxX, maxY, true
}

// PolygonCentroidShoelace — scalar back-end. Delegates to the
// shared polygonCentroidShoelaceScalar helper in geom_common.go.
// SIMD variant with a lane-parallel body lives in geom_simd.go.
//
// Semantics: shoelace area-weighted centroid on the input ring.
// Returns (cx, cy, ok=true) when the ring has ≥3 points;
// (0, 0, false) when it has fewer. When areaTwo == 0 (degenerate
// zero-area ring) the arithmetic-mean-of-segment-starts fallback
// is returned via (sxFallback, syFallback, true).
//
// Handles both closed and unclosed rings — a closing edge from
// (last, first) is added iff last != first.
func PolygonCentroidShoelace(xs, ys []float64) (cx, cy float64, ok bool) {
	n := len(xs)
	if len(ys) < n {
		n = len(ys)
	}
	if n < 3 {
		return 0, 0, false
	}
	return polygonCentroidShoelaceScalar(xs, ys, n)
}

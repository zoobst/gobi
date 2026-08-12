package geometry

import "math"

// DefaultHilbertOrder is the resolution used when SortByHilbert or
// downstream helpers omit an explicit order. 16 bits per axis =
// 65,536 × 65,536 cells, which gives sub-parts-per-million spatial
// discrimination on any planet-scale bounding box (~30 meters on a
// WGS84 world-extent bbox) — enough that within-row-group locality
// dominates any residual quantization error.
const DefaultHilbertOrder = 16

// HilbertIndex returns the 1D position of (x, y) along a
// space-filling Hilbert curve of the given order, computed after
// normalizing (x, y) into a `2^order × 2^order` integer grid derived
// from bounds.
//
// The Hilbert curve preserves 2D locality: points close in (x, y)
// tend to be close in HilbertIndex. Sorting rows by their centroid's
// HilbertIndex before writing a GeoParquet file makes per-row-group
// bboxes small, which is what the v0.3.4 row-group pushdown machinery
// actually needs to prune usefully on real data.
//
// Contract:
//
//   - bounds must be a valid non-empty rectangle. Empty bounds
//     return 0 (all points "index to origin"), which sorts to
//     random-adjacent order — a no-op rather than a crash.
//   - order in [1, 31]. Values outside clamp to DefaultHilbertOrder.
//     Order 16 (default) is enough for continental datasets; order
//     24+ approaches meter-scale on a WGS84 world extent.
//   - Points outside bounds are clamped to the boundary before
//     quantization — an off-by-a-hair polygon still gets a stable
//     index.
//
// The 2D Hilbert construction is the standard iterative
// quadrant-rotation algorithm (see e.g. Wikipedia's "Hilbert curve"
// article, xy2d). No lookup tables — hot loop is O(order) integer
// ops, ~30 ns/call at order=16.
func HilbertIndex(x, y float64, bounds Bounds, order int) uint64 {
	if bounds.Empty() {
		return 0
	}
	if order < 1 || order > 31 {
		order = DefaultHilbertOrder
	}
	dx := bounds.MaxX - bounds.MinX
	dy := bounds.MaxY - bounds.MinY
	if dx <= 0 || dy <= 0 {
		return 0
	}
	n := uint32(1) << order
	// Normalize into [0, n). Clamp out-of-bounds points to the grid
	// edge — a common shape for datasets whose declared bbox comes
	// from a precomputed metadata blob that's slightly tighter than
	// the actual data envelope. Explicit switch for the negative /
	// in-range / above-range cases avoids relying on Go's
	// implementation-defined float→uint conversion for negatives.
	fx := (x - bounds.MinX) / dx
	fy := (y - bounds.MinY) / dy
	var ix, iy uint32
	switch {
	case fx < 0:
		ix = 0
	case fx >= 1:
		ix = n - 1
	default:
		ix = uint32(math.Floor(fx * float64(n)))
		if ix >= n {
			ix = n - 1 // guard against fx == 0.9999... rounding to n
		}
	}
	switch {
	case fy < 0:
		iy = 0
	case fy >= 1:
		iy = n - 1
	default:
		iy = uint32(math.Floor(fy * float64(n)))
		if iy >= n {
			iy = n - 1
		}
	}
	return hilbertXY2D(order, ix, iy)
}

// hilbertXY2D is the integer-arithmetic core: maps (ix, iy) in
// [0, 2^order)² to a Hilbert-curve position in [0, 4^order).
//
// The loop walks quadrants coarse-to-fine, adds each quadrant's
// contribution to d, then rotates the local frame so the next
// finer level is oriented consistently with the curve's recursive
// definition. Textbook implementation — see Wikipedia.
func hilbertXY2D(order int, x, y uint32) uint64 {
	var d uint64
	n := uint32(1) << order
	for s := n >> 1; s > 0; s >>= 1 {
		var rx, ry uint32
		if x&s != 0 {
			rx = 1
		}
		if y&s != 0 {
			ry = 1
		}
		d += uint64(s) * uint64(s) * uint64((3*rx)^ry)
		// Rotate quadrant.
		if ry == 0 {
			if rx == 1 {
				x = s - 1 - x
				y = s - 1 - y
			}
			x, y = y, x
		}
	}
	return d
}

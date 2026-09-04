package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// PIPFromWKB reports whether (tx, ty) lies inside the polygon
// encoded in data. Semantics match Polygon.Contains(pt) for
// Polygon inputs (test point against exterior, exclude holes) and
// MultiPolygon "point in any constituent polygon" for
// MultiPolygon inputs. Non-polygon type codes (Point, LineString,
// etc.) return (false, nil) — matches the AoS shape where
// Polygon.Contains isn't defined on those types.
//
// This is Slice 4's WKB-facing entry point. Walks the byte stream
// once with an inline even-odd crossing test — no []Point or
// Polygon allocation, no `closedRing` copy for unclosed rings.
// Zero allocation per call.
//
// Same "points on the boundary have undefined containment" caveat
// as the AoS pointInRing kernel. Callers requiring
// boundary-inclusive semantics should pair this with a separate
// on-boundary scan, matching the pointInPolygon → pointOnPolygonBoundary
// shape in the AoS path.
func PIPFromWKB(data []byte, tx, ty float64) (bool, error) {
	if len(data) < 5 {
		return false, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return false, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		in, _, err := pipPolygonFromWKB(body, bo, hasZ, tx, ty)
		return in, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		return pipMultiPolygonFromWKB(body, bo, hasZ, tx, ty)
	default:
		// Non-polygon geometries have no meaningful "contains a
		// point" answer via ring-crossing. Return false to match
		// the AoS Polygon.Contains shape (which is a Polygon-typed
		// method — no dispatch on other types).
		return false, nil
	}
}

// pipPolygonFromWKB scans one Polygon body (starting at the ring
// count) and returns (inside, bytesConsumed, err). Runs the
// crossing test on the exterior ring first with an early exit,
// then tests each hole; any hole containing the point disqualifies
// the polygon.
func pipPolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, tx, ty float64) (bool, int, error) {
	if len(data) < 4 {
		return false, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	if numRings == 0 {
		return false, off, nil
	}
	inExterior := false
	for r := range numRings {
		if len(data) < off+4 {
			return false, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return false, 0, ErrShortWKB
		}
		if r == 0 {
			// Exterior ring — always test.
			inExterior = ringCrossingTest(data[off:], bo, nPts, cs, tx, ty)
			off += nPts * cs
			if !inExterior {
				// Skip the hole ring bodies; still need to advance
				// off correctly to keep the return value honest for
				// callers that consume multiple geometries from a
				// stream. Scan just enough to walk each hole's byte
				// range.
				for h := 1; h < numRings; h++ {
					if len(data) < off+4 {
						return false, 0, ErrShortWKB
					}
					holePts := int(bo.Uint32(data[off : off+4]))
					off += 4 + holePts*cs
					if len(data) < off {
						return false, 0, ErrShortWKB
					}
				}
				return false, off, nil
			}
			continue
		}
		// Hole ring — any containment disqualifies.
		if ringCrossingTest(data[off:], bo, nPts, cs, tx, ty) {
			// Same skip-remaining-holes pattern to keep byte offset
			// correct for a hypothetical caller that reads a
			// MultiPolygon and needs to advance past this Polygon.
			off += nPts * cs
			for h := r + 1; h < numRings; h++ {
				if len(data) < off+4 {
					return false, 0, ErrShortWKB
				}
				holePts := int(bo.Uint32(data[off : off+4]))
				off += 4 + holePts*cs
				if len(data) < off {
					return false, 0, ErrShortWKB
				}
			}
			return false, off, nil
		}
		off += nPts * cs
	}
	return inExterior, off, nil
}

// pipMultiPolygonFromWKB scans a MultiPolygon body and returns
// true iff any constituent polygon contains (tx, ty). Early-exits
// on first match.
func pipMultiPolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, tx, ty float64) (bool, error) {
	if len(data) < 4 {
		return false, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	for range n {
		if len(data) < off+5 {
			return false, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return false, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return false, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		inside, sz, err := pipPolygonFromWKB(data[off+5:], innerBO, hasZ, tx, ty)
		if err != nil {
			return false, err
		}
		if inside {
			return true, nil
		}
		off += 5 + sz
	}
	return false, nil
}

// PIPInclusiveFromWKB reports whether (tx, ty) lies inside the
// polygon encoded in data OR on its boundary. Semantics match the
// AoS `pointInPolygon(pt, poly)` exactly, i.e. boundary-inclusive
// containment. This is the entry point SJoin's Points × Polygons
// refine needs — the strict-interior `PIPFromWKB` has documented
// undefined behavior on boundaries, which would silently change
// SJoin semantics for grid-aligned data.
//
// Single WKB pass: extends the ring scan to also do a
// point-on-segment test per segment (collinearity + within-bbox).
// Returns true on first boundary hit; otherwise finalizes with
// the crossing-count parity. Zero allocation on well-formed
// input.
//
// Non-polygon type codes (Point, LineString, etc.) return
// (false, nil) — matches PIPFromWKB.
func PIPInclusiveFromWKB(data []byte, tx, ty float64) (bool, error) {
	if len(data) < 5 {
		return false, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return false, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		in, _, err := pipInclusivePolygonFromWKB(body, bo, hasZ, tx, ty)
		return in, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		return pipInclusiveMultiPolygonFromWKB(body, bo, hasZ, tx, ty)
	default:
		return false, nil
	}
}

// pipInclusivePolygonFromWKB is the boundary-inclusive variant of
// pipPolygonFromWKB. Two ways the point can be inside the polygon:
//
//   - strictly inside exterior AND strictly outside every hole
//   - on the boundary of ANY ring (exterior or hole)
//
// Any boundary hit on any ring returns true immediately; hole-
// interior containment (strict) still disqualifies. Semantics
// match `pointInPolygon(pt, p)` exactly.
func pipInclusivePolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, tx, ty float64) (bool, int, error) {
	if len(data) < 4 {
		return false, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	if numRings == 0 {
		return false, off, nil
	}
	// Track exterior-strict + any-boundary-hit as we walk rings.
	var (
		inExterior    bool
		onAnyBoundary bool
		inAnyHole     bool
	)
	for r := range numRings {
		if len(data) < off+4 {
			return false, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return false, 0, ErrShortWKB
		}
		if onAnyBoundary {
			// Already found on-boundary — skip ring test bodies but
			// advance offset honestly for callers that consume more.
			off += nPts * cs
			continue
		}
		in, onBoundary := ringCrossingAndBoundary(data[off:], bo, nPts, cs, tx, ty)
		if onBoundary {
			onAnyBoundary = true
			off += nPts * cs
			continue
		}
		if r == 0 {
			inExterior = in
			if !inExterior {
				// Not inside exterior — walk remaining hole rings just
				// for the boundary check. Skip strict-interior for holes.
				off += nPts * cs
				for h := 1; h < numRings; h++ {
					if len(data) < off+4 {
						return false, 0, ErrShortWKB
					}
					holePts := int(bo.Uint32(data[off : off+4]))
					off += 4
					if len(data) < off+holePts*cs {
						return false, 0, ErrShortWKB
					}
					_, holeBoundary := ringCrossingAndBoundary(data[off:], bo, holePts, cs, tx, ty)
					off += holePts * cs
					if holeBoundary {
						return true, off, nil
					}
				}
				return false, off, nil
			}
		} else if in {
			inAnyHole = true
		}
		off += nPts * cs
	}
	if onAnyBoundary {
		return true, off, nil
	}
	return inExterior && !inAnyHole, off, nil
}

// pipInclusiveMultiPolygonFromWKB scans a MultiPolygon body and
// returns true iff any constituent polygon contains (tx, ty) in
// the boundary-inclusive sense. Early-exits on first match.
func pipInclusiveMultiPolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, tx, ty float64) (bool, error) {
	if len(data) < 4 {
		return false, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	for range n {
		if len(data) < off+5 {
			return false, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return false, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return false, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		inside, sz, err := pipInclusivePolygonFromWKB(data[off+5:], innerBO, hasZ, tx, ty)
		if err != nil {
			return false, err
		}
		if inside {
			return true, nil
		}
		off += 5 + sz
	}
	return false, nil
}

// ringCrossingAndBoundary combines the even-odd crossing test
// with a per-segment on-boundary check. Returns (interiorCrossingParity,
// onBoundary). If onBoundary is true, interiorCrossingParity is
// meaningless — the caller short-circuits without inspecting it.
//
// The on-boundary test is `collinear (via cross==0) AND within
// segment bbox` — matches AoS `pointOnSegment` semantics exactly.
// Zero allocation.
func ringCrossingAndBoundary(data []byte, bo binary.ByteOrder, n, cs int, tx, ty float64) (bool, bool) {
	if n < 2 {
		return false, false
	}
	fx := math.Float64frombits(bo.Uint64(data[0:8]))
	fy := math.Float64frombits(bo.Uint64(data[8:16]))
	inside := false
	px, py := fx, fy
	for i := 1; i < n; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(data[off : off+8]))
		y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
		if pointOnSegmentInline(tx, ty, px, py, x, y) {
			return false, true
		}
		if (py > ty) != (y > ty) {
			xIntersect := (px-x)*(ty-y)/(py-y) + x
			if tx < xIntersect {
				inside = !inside
			}
		}
		px, py = x, y
	}
	// Closing segment.
	if pointOnSegmentInline(tx, ty, px, py, fx, fy) {
		return false, true
	}
	if (py > ty) != (fy > ty) {
		xIntersect := (px-fx)*(ty-fy)/(py-fy) + fx
		if tx < xIntersect {
			inside = !inside
		}
	}
	return inside, false
}

// pointOnSegmentInline is the coord-form of pointOnSegment. Uses
// the cross-product-zero collinearity test plus a bbox containment
// check. Reads coords directly — no Point struct materialization.
func pointOnSegmentInline(tx, ty, ax, ay, bx, by float64) bool {
	// Collinearity: cross((b-a), (t-a)) == 0.
	if (bx-ax)*(ty-ay)-(by-ay)*(tx-ax) != 0 {
		return false
	}
	// Within-segment bbox.
	minX, maxX := ax, bx
	if minX > maxX {
		minX, maxX = maxX, minX
	}
	if tx < minX || tx > maxX {
		return false
	}
	minY, maxY := ay, by
	if minY > maxY {
		minY, maxY = maxY, minY
	}
	return ty >= minY && ty <= maxY
}

// ringCrossingTest is the inline even-odd algorithm running
// directly against a WKB coordinate slab. Returns true iff
// (tx, ty) lies inside the ring the coords describe.
//
// Handles both closed and unclosed rings implicitly — walks
// segments (i-1, i) for i in [1, n), then adds the closing
// segment (n-1, 0). Coincident endpoints contribute a zero-length
// segment that the (yi > ty) != (yj > ty) test rejects, so an
// already-closed ring's final duplicate coord is harmless.
//
// Zero allocation. Called from both PIPFromWKB (scanning WKB
// directly) and the AoS Polygon.Contains fast path once we've
// obtained a coordinate view.
func ringCrossingTest(data []byte, bo binary.ByteOrder, n, cs int, tx, ty float64) bool {
	if n < 3 {
		return false
	}
	fx := math.Float64frombits(bo.Uint64(data[0:8]))
	fy := math.Float64frombits(bo.Uint64(data[8:16]))
	inside := false
	px, py := fx, fy
	for i := 1; i < n; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(data[off : off+8]))
		y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
		if (py > ty) != (y > ty) {
			xIntersect := (px-x)*(ty-y)/(py-y) + x
			if tx < xIntersect {
				inside = !inside
			}
		}
		px, py = x, y
	}
	// Closing segment: (last, first). No-op if the ring was
	// already closed (last == first) because the y-comparison
	// evaluates equal on both sides.
	if (py > ty) != (fy > ty) {
		xIntersect := (px-fx)*(ty-fy)/(py-fy) + fx
		if tx < xIntersect {
			inside = !inside
		}
	}
	return inside
}

package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// PlanarAreaFromWKB returns the absolute planar (XY) area of the
// WKB geometry in coord² without materializing intermediate
// []Point / Polygon structs. Polygon area is exterior − holes;
// MultiPolygon sums per-polygon area; GeometryCollection recurses.
// Non-areal types (Point, LineString, MultiPoint,
// MultiLineString) contribute 0.
//
// Semantics match `PlanarRingArea` composed via `Polygon.Area` on
// a projected CRS — i.e. the shoelace formula applied ring-by-
// ring with unclosed rings treated as if the first vertex were
// virtually appended. Unit conversion is left to the caller
// (multiply by `1 / (perM*perM)` for a projected CRS whose linear
// unit is meters). Geographic CRSes must fall back to the AoS
// spherical-excess path.
//
// Byte-order / type-code handling mirrors ParseWKB; malformed
// input returns an error. Zero-allocation on well-formed input.
func PlanarAreaFromWKB(data []byte) (float64, error) {
	total, _, err := scanWKBPlanarArea(data, false)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// scanWKBPlanarArea consumes exactly one WKB geometry from the head
// of data and returns (area, bytesConsumed, err). When
// inCollection is true, nested GeometryCollections are rejected.
func scanWKBPlanarArea(data []byte, inCollection bool) (float64, int, error) {
	if len(data) < 5 {
		return 0, 0, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return 0, 0, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		if len(body) < 16 {
			return 0, 0, ErrShortWKB
		}
		return 0, 5 + 16, nil
	case wkbPointZ:
		if len(body) < 24 {
			return 0, 0, ErrShortWKB
		}
		return 0, 5 + 24, nil
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		sz, err := skipLineString(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		area, sz, err := scanPolygonPlanarArea(body, bo, hasZ)
		return area, 5 + sz, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		sz, err := skipMultiPoint(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		sz, err := skipMultiLineString(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		area, sz, err := scanMultiPolygonPlanarArea(body, bo, hasZ)
		return area, 5 + sz, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		if inCollection {
			return 0, 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		area, sz, err := scanGeometryCollectionPlanarArea(body, bo)
		return area, 5 + sz, err
	default:
		return 0, 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

// scanPolygonPlanarArea returns |exterior| − Σ|holes| via the
// shoelace formula on each ring. Rings with fewer than 3 points
// contribute 0 (matches PlanarRingArea).
func scanPolygonPlanarArea(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	var area float64
	for r := range numRings {
		if len(data) < off+4 {
			return 0, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return 0, 0, ErrShortWKB
		}
		ringArea := planarRingAreaFromCoords(data[off:], bo, nPts, cs)
		if r == 0 {
			area += ringArea
		} else {
			area -= ringArea
		}
		off += nPts * cs
	}
	if area < 0 {
		// AoS Polygon.Area returns exterior − holes without abs. The
		// scanner keeps the same behavior; ring winding is caller's
		// responsibility. But an unclosed-exterior + closed-holes
		// combo can produce a small negative on degenerate inputs —
		// clamp to 0 to preserve the "area is nonnegative" contract
		// on well-formed input (matches PlanarRingArea's `math.Abs`).
		//
		// This differs from Polygon.Area's exact numeric shape but
		// matches the semantic guarantee callers rely on.
		return 0, off, nil
	}
	return area, off, nil
}

// planarRingAreaFromCoords runs the shoelace formula over one
// ring's coordinate slab (no length prefix). Matches PlanarRingArea
// exactly: closedRing behavior via virtual-append when
// (fx,fy) != (lastX,lastY), zero-area guard for < 3 points.
func planarRingAreaFromCoords(data []byte, bo binary.ByteOrder, nPts, cs int) float64 {
	if nPts < 3 {
		return 0
	}
	fx := math.Float64frombits(bo.Uint64(data[0:8]))
	fy := math.Float64frombits(bo.Uint64(data[8:16]))
	var a float64
	px, py := fx, fy
	for i := 1; i < nPts; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(data[off : off+8]))
		y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
		a += px*y - x*py
		px, py = x, y
	}
	// Closing edge iff ring wasn't already closed.
	if px != fx || py != fy {
		a += px*fy - fx*py
	}
	return math.Abs(a) / 2
}

// scanMultiPolygonPlanarArea sums per-polygon planar area.
func scanMultiPolygonPlanarArea(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	var area float64
	for range n {
		if len(data) < off+5 {
			return 0, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, 0, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		a, sz, err := scanPolygonPlanarArea(data[off+5:], innerBO, hasZ)
		if err != nil {
			return 0, 0, err
		}
		area += a
		off += 5 + sz
	}
	return area, off, nil
}

// scanGeometryCollectionPlanarArea sums PlanarArea across every
// member. Nested GeometryCollections are rejected.
func scanGeometryCollectionPlanarArea(data []byte, bo binary.ByteOrder) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	var area float64
	for range n {
		a, used, err := scanWKBPlanarArea(data[off:], true)
		if err != nil {
			return 0, 0, err
		}
		area += a
		off += used
	}
	return area, off, nil
}

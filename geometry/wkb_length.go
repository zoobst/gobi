package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// PlanarLengthFromWKB returns the planar (XY) length of the WKB
// geometry in coordinate units without materializing any
// intermediate []Point / LineString structs. Sums Euclidean
// segment lengths for LineString / MultiLineString; recurses into
// GeometryCollections. All other type codes (Point, MultiPoint,
// Polygon, MultiPolygon) contribute 0 — matches the AoS shape of
// `geometry.Length` where non-linear geometries return 0.
//
// This is the SoA sibling of BoundsFromWKB / CentroidFromWKB for
// callers that only need the planar linear extent — e.g. a Series
// GeomLength column over a projected CRS. Returns coordinate-unit
// length; Unit conversion is left to the caller (multiply by
// `1 / metersPerUnit(u)` for a projected CRS whose linear unit is
// meters). Geographic CRSes must fall back to the AoS
// Haversine path — the WKB blob doesn't carry CRS context.
//
// Byte-order / type-code handling mirrors ParseWKB; malformed
// input returns an error. Zero-allocation on well-formed input.
func PlanarLengthFromWKB(data []byte) (float64, error) {
	total, _, err := scanWKBPlanarLength(data, false)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// scanWKBPlanarLength consumes exactly one WKB geometry from the
// head of data and returns (sum, bytesConsumed, err). When
// inCollection is true, nested GeometryCollections are rejected
// (matching ParseWKB's rule).
func scanWKBPlanarLength(data []byte, inCollection bool) (float64, int, error) {
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
		sum, sz, err := scanLineStringPlanarLength(body, bo, hasZ)
		return sum, 5 + sz, err
	case wkbPolygon, wkbPolygonZ:
		// Non-linear — Length returns 0. Walk the byte range to
		// advance the offset honestly.
		hasZ := typ == wkbPolygonZ
		sz, err := skipPolygon(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		sz, err := skipMultiPoint(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		sum, sz, err := scanMultiLineStringPlanarLength(body, bo, hasZ)
		return sum, 5 + sz, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		sz, err := skipMultiPolygon(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		if inCollection {
			return 0, 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		sum, sz, err := scanGeometryCollectionPlanarLength(body, bo)
		return sum, 5 + sz, err
	default:
		return 0, 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

// scanLineStringPlanarLength sums Euclidean distances between
// consecutive coordinate pairs. Returns (sum, bytesConsumed).
// Lines with fewer than 2 points contribute 0.
func scanLineStringPlanarLength(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return 0, 0, ErrShortWKB
	}
	if n < 2 {
		return 0, 4 + n*cs, nil
	}
	base := data[4:]
	px := math.Float64frombits(bo.Uint64(base[0:8]))
	py := math.Float64frombits(bo.Uint64(base[8:16]))
	var sum float64
	for i := 1; i < n; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(base[off : off+8]))
		y := math.Float64frombits(bo.Uint64(base[off+8 : off+16]))
		dx := x - px
		dy := y - py
		sum += math.Sqrt(dx*dx + dy*dy)
		px, py = x, y
	}
	return sum, 4 + n*cs, nil
}

// scanMultiLineStringPlanarLength sums the planar length of every
// inner LineString.
func scanMultiLineStringPlanarLength(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	var sum float64
	for range n {
		if len(data) < off+5 {
			return 0, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, 0, fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		s, sz, err := scanLineStringPlanarLength(data[off+5:], innerBO, hasZ)
		if err != nil {
			return 0, 0, err
		}
		sum += s
		off += 5 + sz
	}
	return sum, off, nil
}

// scanGeometryCollectionPlanarLength sums Length across every
// member. Nested GeometryCollections are rejected by
// scanWKBPlanarLength (inCollection=true).
func scanGeometryCollectionPlanarLength(data []byte, bo binary.ByteOrder) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	var sum float64
	for range n {
		s, used, err := scanWKBPlanarLength(data[off:], true)
		if err != nil {
			return 0, 0, err
		}
		sum += s
		off += used
	}
	return sum, off, nil
}

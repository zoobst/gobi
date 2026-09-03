package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// PlanarMinDistanceFromWKB returns the min planar Euclidean
// distance between two WKB-encoded geometries. Parses both to
// slab-form representations directly (no `[]Point` intermediate)
// and runs the Slice-11 min-distance nested loop with a single
// `math.Sqrt` at the end.
//
// # Non-intersection assumption
//
// This function is a fast path for the (bbox-disjoint → definitely
// non-intersecting) case that `Series.GeomDistance` uses when
// filtering rows against a fixed `other`. It does NOT run a
// segment-segment intersects check; intersecting geometries can
// produce a nonzero distance (matching the vertex-to-segment
// approach that `planarMinDistance` returns for non-intersecting
// inputs). Callers that need `Intersects → 0` semantics must
// verify bboxes are disjoint or fall through to the AoS
// `GeomDistance(a, b, u)` path.
//
// Returns math.Inf(+1) when both inputs are empty (no vertex or
// segment to compare against). Malformed WKB returns an error.
func PlanarMinDistanceFromWKB(a, b []byte) (float64, error) {
	var ag, bg distanceGeometry
	if err := distanceGeometryFromWKB(a, &ag); err != nil {
		return 0, err
	}
	if err := distanceGeometryFromWKB(b, &bg); err != nil {
		return 0, err
	}
	d2 := planarMinDistanceSquared(&ag, &bg)
	if math.IsInf(d2, 1) {
		return math.Inf(1), nil
	}
	return math.Sqrt(d2), nil
}

// distanceGeometryFromWKB parses a WKB blob directly into the
// polylines-plus-standalone-vertices representation used by
// planarMinDistanceSquared. No `[]Point` intermediate — extracts
// coord slabs while walking the byte stream.
func distanceGeometryFromWKB(data []byte, dst *distanceGeometry) error {
	if len(data) < 5 {
		return ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		return distancePointFromWKB(body, bo, false, dst)
	case wkbPointZ:
		return distancePointFromWKB(body, bo, true, dst)
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		return distanceLineStringFromWKB(body, bo, hasZ, dst)
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		_, err := distancePolygonFromWKB(body, bo, hasZ, dst)
		return err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		return distanceMultiPointFromWKB(body, bo, hasZ, dst)
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		return distanceMultiLineStringFromWKB(body, bo, hasZ, dst)
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		return distanceMultiPolygonFromWKB(body, bo, hasZ, dst)
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		return distanceGeometryCollectionFromWKB(body, bo, dst)
	default:
		return fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

func distancePointFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) error {
	need := coordSize(hasZ)
	if len(data) < need {
		return ErrShortWKB
	}
	x := math.Float64frombits(bo.Uint64(data[0:8]))
	y := math.Float64frombits(bo.Uint64(data[8:16]))
	dst.pointXs = append(dst.pointXs, x)
	dst.pointYs = append(dst.pointYs, y)
	return nil
}

func distanceLineStringFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) error {
	if len(data) < 4 {
		return ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return ErrShortWKB
	}
	if n < 2 {
		// Degenerate — treat any lone points as vertex contributions.
		for i := range n {
			off := 4 + i*cs
			x := math.Float64frombits(bo.Uint64(data[off : off+8]))
			y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
			dst.pointXs = append(dst.pointXs, x)
			dst.pointYs = append(dst.pointYs, y)
		}
		return nil
	}
	v, err := lineStringViewBody(data, bo, hasZ)
	if err != nil {
		return err
	}
	dst.polylines = append(dst.polylines, v)
	dst.closed = append(dst.closed, false)
	return nil
}

func distancePolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) (int, error) {
	rings, sz, err := polygonRingViewsBody(data, bo, hasZ)
	if err != nil {
		return 0, err
	}
	for _, r := range rings {
		switch r.Len() {
		case 0:
			// skip
		case 1:
			dst.pointXs = append(dst.pointXs, r.Xs[0])
			dst.pointYs = append(dst.pointYs, r.Ys[0])
		default:
			dst.polylines = append(dst.polylines, r)
			dst.closed = append(dst.closed, true)
		}
	}
	return sz, nil
}

func distanceMultiPointFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) error {
	if len(data) < 4 {
		return ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	elemSize := 5 + coordSize(hasZ)
	innerType := wkbPoint
	if hasZ {
		innerType = wkbPointZ
	}
	for range n {
		if len(data) < off+elemSize {
			return ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return fmt.Errorf("%w: expected Point inside MultiPoint", ErrTypeMismatch)
		}
		if err := distancePointFromWKB(data[off+5:off+elemSize], innerBO, hasZ, dst); err != nil {
			return err
		}
		off += elemSize
	}
	return nil
}

func distanceMultiLineStringFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) error {
	if len(data) < 4 {
		return ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	cs := coordSize(hasZ)
	for range n {
		if len(data) < off+5 {
			return ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		if len(data) < off+5+4 {
			return ErrShortWKB
		}
		nPts := int(innerBO.Uint32(data[off+5 : off+9]))
		if err := distanceLineStringFromWKB(data[off+5:], innerBO, hasZ, dst); err != nil {
			return err
		}
		off += 5 + 4 + nPts*cs
	}
	return nil
}

func distanceMultiPolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) error {
	if len(data) < 4 {
		return ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	for range n {
		if len(data) < off+5 {
			return ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		sz, err := distancePolygonFromWKB(data[off+5:], innerBO, hasZ, dst)
		if err != nil {
			return err
		}
		off += 5 + sz
	}
	return nil
}

func distanceGeometryCollectionFromWKB(data []byte, bo binary.ByteOrder, dst *distanceGeometry) error {
	if len(data) < 4 {
		return ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	for range n {
		if len(data) < off+5 {
			return ErrShortWKB
		}
		// Recurse via the top-level parser. Nested
		// GeometryCollections are rejected there.
		remainder := data[off:]
		if err := distanceGeometryFromWKB(remainder, dst); err != nil {
			return err
		}
		// Advance offset by the size of the consumed inner geometry.
		// Simplest: re-scan the byte range via a size helper. Since
		// distanceGeometryFromWKB doesn't return bytes-consumed, use
		// the WKB-size skip helper family.
		innerBO, err := byteOrder(remainder[0])
		if err != nil {
			return err
		}
		typ := innerBO.Uint32(remainder[1:5])
		var innerSize int
		switch typ {
		case wkbPoint:
			innerSize = 5 + 16
		case wkbPointZ:
			innerSize = 5 + 24
		case wkbLineString, wkbLineStringZ:
			hasZ := typ == wkbLineStringZ
			sz, err := skipLineString(remainder[5:], innerBO, hasZ)
			if err != nil {
				return err
			}
			innerSize = 5 + sz
		case wkbPolygon, wkbPolygonZ:
			hasZ := typ == wkbPolygonZ
			sz, err := skipPolygon(remainder[5:], innerBO, hasZ)
			if err != nil {
				return err
			}
			innerSize = 5 + sz
		case wkbMultiPoint, wkbMultiPointZ:
			hasZ := typ == wkbMultiPointZ
			sz, err := skipMultiPoint(remainder[5:], innerBO, hasZ)
			if err != nil {
				return err
			}
			innerSize = 5 + sz
		case wkbMultiLineString, wkbMultiLineStringZ:
			hasZ := typ == wkbMultiLineStringZ
			sz, err := skipMultiLineString(remainder[5:], innerBO, hasZ)
			if err != nil {
				return err
			}
			innerSize = 5 + sz
		case wkbMultiPolygon, wkbMultiPolygonZ:
			hasZ := typ == wkbMultiPolygonZ
			sz, err := skipMultiPolygon(remainder[5:], innerBO, hasZ)
			if err != nil {
				return err
			}
			innerSize = 5 + sz
		default:
			return fmt.Errorf("%w: nested collection or unsupported type %d in GeometryCollection",
				ErrUnsupportedWKB, typ)
		}
		off += innerSize
	}
	return nil
}

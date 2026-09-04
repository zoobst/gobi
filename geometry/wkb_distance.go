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
// # Allocation shape
//
// The parse skips `[]Point` materialization but does `append` onto
// per-role slabs (pointXs/pointYs, polylines). For most workloads
// this amortizes to a small constant number of `growslice` calls;
// callers that need strict zero-alloc must pre-scan geometry
// counts and pass hinted slabs — not a supported entry point today.
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
	if _, err := distanceGeometryFromWKB(a, &ag); err != nil {
		return 0, err
	}
	if _, err := distanceGeometryFromWKB(b, &bg); err != nil {
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
// planarMinDistanceSquared. Returns bytes consumed so the caller
// (top-level or GeometryCollection loop) can advance the cursor
// without re-parsing the header.
func distanceGeometryFromWKB(data []byte, dst *distanceGeometry) (int, error) {
	if len(data) < 5 {
		return 0, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return 0, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		sz, err := distancePointFromWKB(body, bo, false, dst)
		return 5 + sz, err
	case wkbPointZ:
		sz, err := distancePointFromWKB(body, bo, true, dst)
		return 5 + sz, err
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		sz, err := distanceLineStringFromWKB(body, bo, hasZ, dst)
		return 5 + sz, err
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		sz, err := distancePolygonFromWKB(body, bo, hasZ, dst)
		return 5 + sz, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		sz, err := distanceMultiPointFromWKB(body, bo, hasZ, dst)
		return 5 + sz, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		sz, err := distanceMultiLineStringFromWKB(body, bo, hasZ, dst)
		return 5 + sz, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		sz, err := distanceMultiPolygonFromWKB(body, bo, hasZ, dst)
		return 5 + sz, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		sz, err := distanceGeometryCollectionFromWKB(body, bo, dst)
		return 5 + sz, err
	default:
		return 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

func distancePointFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) (int, error) {
	need := coordSize(hasZ)
	if len(data) < need {
		return 0, ErrShortWKB
	}
	x := math.Float64frombits(bo.Uint64(data[0:8]))
	y := math.Float64frombits(bo.Uint64(data[8:16]))
	dst.pointXs = append(dst.pointXs, x)
	dst.pointYs = append(dst.pointYs, y)
	return need, nil
}

func distanceLineStringFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if !coordsFit(len(data)-4, n, cs) {
		return 0, ErrShortWKB
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
		return 4 + n*cs, nil
	}
	v, err := lineStringViewBody(data, bo, hasZ)
	if err != nil {
		return 0, err
	}
	dst.polylines = append(dst.polylines, v)
	dst.closed = append(dst.closed, false)
	return 4 + n*cs, nil
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

func distanceMultiPointFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
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
			return 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, fmt.Errorf("%w: expected Point inside MultiPoint", ErrTypeMismatch)
		}
		if _, err := distancePointFromWKB(data[off+5:off+elemSize], innerBO, hasZ, dst); err != nil {
			return 0, err
		}
		off += elemSize
	}
	return off, nil
}

func distanceMultiLineStringFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	for range n {
		if len(data) < off+5 {
			return 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		sz, err := distanceLineStringFromWKB(data[off+5:], innerBO, hasZ, dst)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

func distanceMultiPolygonFromWKB(data []byte, bo binary.ByteOrder, hasZ bool, dst *distanceGeometry) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	for range n {
		if len(data) < off+5 {
			return 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		sz, err := distancePolygonFromWKB(data[off+5:], innerBO, hasZ, dst)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

func distanceGeometryCollectionFromWKB(data []byte, bo binary.ByteOrder, dst *distanceGeometry) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	for range n {
		if len(data) < off+5 {
			return 0, ErrShortWKB
		}
		// Reject nested GeometryCollection to match ParseWKB's
		// contract (see decodeGeometryCollectionWKBSized in wkb.go).
		// Only the type code is inspected — cheaper than recursing
		// and then unwinding on failure, and avoids mutating dst
		// with points from the nested collection before the error.
		innerTyp, _, err := WKBTypeCode(data[off:])
		if err != nil {
			return 0, err
		}
		if innerTyp == wkbGeometryCollection {
			return 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		// Recurse via the top-level parser, which now reports the
		// size consumed — no header re-scan needed here.
		sz, err := distanceGeometryFromWKB(data[off:], dst)
		if err != nil {
			return 0, err
		}
		off += sz
	}
	return off, nil
}

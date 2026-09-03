package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// BoundsFromWKB computes the axis-aligned 2D bounding box of a WKB
// geometry without materializing any intermediate Point / Polygon
// / etc. structs. Walks the byte stream once, tracking running
// min/max on X and Y.
//
// This is Slice 2's SoA fast path for bbox-only callers — the
// parquetio bbox-covering-column write path, GeoParquet metadata
// bounds compute, and any Filter/predicate hot path that only
// needs the bbox of each input geometry. Skips the O(n)
// `[]Point` allocation that ParseWKB does even though the caller
// throws the geometry away immediately after `.Bounds()`.
//
// Semantics match `ParseWKB(data).Bounds()` exactly:
//
//   - Empty geometries (empty LineString / Polygon /
//     GeometryCollection) return EmptyBounds().
//   - Z coordinates are ignored — matches the 2D Bounds type.
//   - MultiPoint / MultiLineString / MultiPolygon /
//     GeometryCollection recursively include every sub-geometry's
//     coordinates. Nested GeometryCollections are rejected inside
//     a GeometryCollection (matching ParseWKB).
//   - Byte-order and type-code handling mirrors ParseWKB;
//     unsupported type codes return ErrUnsupportedWKB.
//
// The scanner is per-call zero-allocation on well-formed input.
// Malformed input returns an error without leaking partial state.
func BoundsFromWKB(data []byte) (Bounds, error) {
	// Use ±Inf as the accumulator sentinel so the hot-loop
	// extendBoundsInline can compare-and-update without a branch
	// on "is this the first coord?". EmptyBounds's inverted-sentinel
	// form (1, 1, -1, -1) doesn't compose with a naive < comparison
	// on the first extend, so we normalize the entry state and
	// convert back to EmptyBounds if no coord was ever seen.
	b := Bounds{
		MinX: math.Inf(1), MinY: math.Inf(1),
		MaxX: math.Inf(-1), MaxY: math.Inf(-1),
	}
	if _, err := scanWKBBounds(data, &b, false); err != nil {
		return EmptyBounds(), err
	}
	if math.IsInf(b.MinX, 1) {
		// No coordinate was scanned — matches ParseWKB's empty
		// geometry semantics.
		return EmptyBounds(), nil
	}
	return b, nil
}

// scanWKBBounds consumes exactly one WKB geometry from the head of
// data and extends b with every coordinate pair found. Returns the
// number of bytes consumed. When inCollection is true, nested
// GeometryCollections are rejected (matching ParseWKB's rule).
func scanWKBBounds(data []byte, b *Bounds, inCollection bool) (int, error) {
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
		return 5 + 16, scanPointBounds(body, bo, false, b)
	case wkbPointZ:
		return 5 + 24, scanPointBounds(body, bo, true, b)
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		size, err := scanLineStringBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		size, err := scanPolygonBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		size, err := scanMultiPointBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		size, err := scanMultiLineStringBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		size, err := scanMultiPolygonBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		if inCollection {
			return 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		size, err := scanGeometryCollectionBounds(body, bo, b)
		return 5 + size, err
	default:
		return 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

// scanPointBounds reads exactly one XY (or XYZ) coordinate from
// data and extends b. Point-typed WKB values always contribute a
// single coordinate — no empty-point encoding in OGC SFA 1.2.
func scanPointBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) error {
	need := coordSize(hasZ)
	if len(data) < need {
		return ErrShortWKB
	}
	x := math.Float64frombits(bo.Uint64(data[0:8]))
	y := math.Float64frombits(bo.Uint64(data[8:16]))
	extendBoundsInline(b, x, y)
	return nil
}

// scanLineStringBounds walks n coordinate tuples and extends b
// with each. Returns the total bytes consumed (including the
// 4-byte length prefix).
func scanLineStringBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return 0, ErrShortWKB
	}
	base := data[4:]
	for i := range n {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(base[off : off+8]))
		y := math.Float64frombits(bo.Uint64(base[off+8 : off+16]))
		extendBoundsInline(b, x, y)
	}
	return 4 + n*cs, nil
}

// scanPolygonBounds walks numRings, each ring being a length-
// prefixed run of coordinates.
func scanPolygonBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	for range numRings {
		if len(data) < off+4 {
			return 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return 0, ErrShortWKB
		}
		for i := range nPts {
			base := off + i*cs
			x := math.Float64frombits(bo.Uint64(data[base : base+8]))
			y := math.Float64frombits(bo.Uint64(data[base+8 : base+16]))
			extendBoundsInline(b, x, y)
		}
		off += nPts * cs
	}
	return off, nil
}

// scanMultiPointBounds — n inner Point WKBs, each with its own
// byte-order + type header.
func scanMultiPointBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPoint
	if hasZ {
		innerType = wkbPointZ
	}
	elemSize := 5 + coordSize(hasZ)
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
		if err := scanPointBounds(data[off+5:off+elemSize], innerBO, hasZ, b); err != nil {
			return 0, err
		}
		off += elemSize
	}
	return off, nil
}

// scanMultiLineStringBounds — n inner LineString WKBs.
func scanMultiLineStringBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
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
		sz, err := scanLineStringBounds(data[off+5:], innerBO, hasZ, b)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

// scanMultiPolygonBounds — n inner Polygon WKBs.
func scanMultiPolygonBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
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
		sz, err := scanPolygonBounds(data[off+5:], innerBO, hasZ, b)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

// scanGeometryCollectionBounds recurses into each member. The
// inCollection=true flag on the recursive call rejects nested
// GeometryCollections (matching ParseWKB).
func scanGeometryCollectionBounds(data []byte, bo binary.ByteOrder, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	for range n {
		used, err := scanWKBBounds(data[off:], b, true)
		if err != nil {
			return 0, err
		}
		off += used
	}
	return off, nil
}

// extendBoundsInline is the hot-loop version of Bounds.Extend.
// Kept manually inlined to keep the SoA scanner competitive on
// the (bbox-per-row × N-rows-per-column) shape parquetio drives.
// Same semantics as (b *Bounds).Extend — first-coord case handled
// by EmptyBounds's sentinel (MinX > MaxX so both comparisons hit).
func extendBoundsInline(b *Bounds, x, y float64) {
	if x < b.MinX {
		b.MinX = x
	}
	if x > b.MaxX {
		b.MaxX = x
	}
	if y < b.MinY {
		b.MinY = y
	}
	if y > b.MaxY {
		b.MaxY = y
	}
}

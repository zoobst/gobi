package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

const (
	wkbXDR byte = 0 // big endian
	wkbNDR byte = 1 // little endian

	// 2D (XY) type codes per OGC SFA 1.2.
	wkbPoint              uint32 = 1
	wkbLineString         uint32 = 2
	wkbPolygon            uint32 = 3
	wkbMultiPoint         uint32 = 4
	wkbMultiLineString    uint32 = 5
	wkbMultiPolygon       uint32 = 6
	wkbGeometryCollection uint32 = 7

	// 3D (XYZ) type codes per OGC SFA 1.2 (ISO variant).
	wkbPointZ              uint32 = 1001
	wkbLineStringZ         uint32 = 1002
	wkbPolygonZ            uint32 = 1003
	wkbMultiPointZ         uint32 = 1004
	wkbMultiLineStringZ    uint32 = 1005
	wkbMultiPolygonZ       uint32 = 1006
	wkbGeometryCollectionZ uint32 = 1007
)

// coordSize returns the byte size of a single coordinate tuple: 16 for XY,
// 24 for XYZ.
func coordSize(hasZ bool) int {
	if hasZ {
		return 24
	}
	return 16
}

// ParseWKB decodes a Well-Known Binary geometry. The resulting geometry has
// no CRS set; callers must supply one from context (e.g. a GeoParquet schema).
func ParseWKB(data []byte) (Geometry, error) {
	g, _, err := decodeInnerGeometryWKB(data)
	return g, err
}

func byteOrder(b byte) (binary.ByteOrder, error) {
	switch b {
	case wkbXDR:
		return binary.BigEndian, nil
	case wkbNDR:
		return binary.LittleEndian, nil
	default:
		return nil, ErrInvalidByteOrder
	}
}

// decodeInnerGeometryWKB consumes exactly one WKB geometry from the head of
// data and returns the parsed geometry plus the number of bytes consumed.
// Handles both 2D and 3D (XYZ) variants; nested GeometryCollections are
// rejected inside a GeometryCollection.
func decodeInnerGeometryWKB(data []byte) (Geometry, int, error) {
	if len(data) < 5 {
		return nil, 0, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return nil, 0, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		p, err := decodePointWKB(body, bo, false)
		return p, 5 + 16, err
	case wkbPointZ:
		p, err := decodePointWKB(body, bo, true)
		return p, 5 + 24, err
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		l, size, err := decodeLineStringWKBSized(body, bo, hasZ)
		return l, 5 + size, err
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		p, size, err := decodePolygonWKBSized(body, bo, hasZ)
		return p, 5 + size, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		mp, size, err := decodeMultiPointWKBSized(body, bo, hasZ)
		return mp, 5 + size, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		mls, size, err := decodeMultiLineStringWKBSized(body, bo, hasZ)
		return mls, 5 + size, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		mpoly, size, err := decodeMultiPolygonWKBSized(body, bo, hasZ)
		return mpoly, 5 + size, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		hasZ := typ == wkbGeometryCollectionZ
		gc, size, err := decodeGeometryCollectionWKBSized(body, bo, hasZ)
		return gc, 5 + size, err
	default:
		return nil, 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

func decodePointWKB(data []byte, bo binary.ByteOrder, hasZ bool) (Point, error) {
	need := coordSize(hasZ)
	if len(data) < need {
		return Point{}, ErrShortWKB
	}
	p := Point{
		X:    math.Float64frombits(bo.Uint64(data[0:8])),
		Y:    math.Float64frombits(bo.Uint64(data[8:16])),
		HasZ: hasZ,
	}
	if hasZ {
		p.Z = math.Float64frombits(bo.Uint64(data[16:24]))
	}
	return p, nil
}

func decodeLineStringWKBSized(data []byte, bo binary.ByteOrder, hasZ bool) (LineString, int, error) {
	if len(data) < 4 {
		return LineString{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return LineString{}, 0, ErrShortWKB
	}
	pts := make([]Point, n)
	for i := 0; i < n; i++ {
		off := 4 + i*cs
		p := Point{
			X:    math.Float64frombits(bo.Uint64(data[off : off+8])),
			Y:    math.Float64frombits(bo.Uint64(data[off+8 : off+16])),
			HasZ: hasZ,
		}
		if hasZ {
			p.Z = math.Float64frombits(bo.Uint64(data[off+16 : off+24]))
		}
		pts[i] = p
	}
	return LineString{Points: pts, HasZ: hasZ}, 4 + n*cs, nil
}

func decodePolygonWKBSized(data []byte, bo binary.ByteOrder, hasZ bool) (Polygon, int, error) {
	if len(data) < 4 {
		return Polygon{}, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	rings := make([][]Point, 0, numRings)
	for r := 0; r < numRings; r++ {
		if len(data) < off+4 {
			return Polygon{}, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return Polygon{}, 0, ErrShortWKB
		}
		pts := make([]Point, nPts)
		for i := 0; i < nPts; i++ {
			base := off + i*cs
			p := Point{
				X:    math.Float64frombits(bo.Uint64(data[base : base+8])),
				Y:    math.Float64frombits(bo.Uint64(data[base+8 : base+16])),
				HasZ: hasZ,
			}
			if hasZ {
				p.Z = math.Float64frombits(bo.Uint64(data[base+16 : base+24]))
			}
			pts[i] = p
		}
		rings = append(rings, pts)
		off += nPts * cs
	}
	return Polygon{Rings: rings, HasZ: hasZ}, off, nil
}

func decodeMultiPointWKBSized(data []byte, bo binary.ByteOrder, hasZ bool) (MultiPoint, int, error) {
	if len(data) < 4 {
		return MultiPoint{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	pts := make([]Point, 0, n)
	innerType := wkbPoint
	if hasZ {
		innerType = wkbPointZ
	}
	elemSize := 5 + coordSize(hasZ)
	for i := 0; i < n; i++ {
		if len(data) < off+elemSize {
			return MultiPoint{}, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return MultiPoint{}, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return MultiPoint{}, 0, fmt.Errorf("%w: expected Point inside MultiPoint", ErrTypeMismatch)
		}
		p, err := decodePointWKB(data[off+5:off+elemSize], innerBO, hasZ)
		if err != nil {
			return MultiPoint{}, 0, err
		}
		pts = append(pts, p)
		off += elemSize
	}
	return MultiPoint{Points: pts, HasZ: hasZ}, off, nil
}

func decodeMultiLineStringWKBSized(data []byte, bo binary.ByteOrder, hasZ bool) (MultiLineString, int, error) {
	if len(data) < 4 {
		return MultiLineString{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	lines := make([]LineString, 0, n)
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	for i := 0; i < n; i++ {
		if len(data) < off+5 {
			return MultiLineString{}, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return MultiLineString{}, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return MultiLineString{}, 0, fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		l, sz, err := decodeLineStringWKBSized(data[off+5:], innerBO, hasZ)
		if err != nil {
			return MultiLineString{}, 0, err
		}
		lines = append(lines, l)
		off += 5 + sz
	}
	return MultiLineString{Lines: lines, HasZ: hasZ}, off, nil
}

func decodeMultiPolygonWKBSized(data []byte, bo binary.ByteOrder, hasZ bool) (MultiPolygon, int, error) {
	if len(data) < 4 {
		return MultiPolygon{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	polys := make([]Polygon, 0, n)
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	for i := 0; i < n; i++ {
		if len(data) < off+5 {
			return MultiPolygon{}, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return MultiPolygon{}, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return MultiPolygon{}, 0, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		p, sz, err := decodePolygonWKBSized(data[off+5:], innerBO, hasZ)
		if err != nil {
			return MultiPolygon{}, 0, err
		}
		polys = append(polys, p)
		off += 5 + sz
	}
	return MultiPolygon{Polygons: polys, HasZ: hasZ}, off, nil
}

func decodeGeometryCollectionWKBSized(data []byte, bo binary.ByteOrder, hasZ bool) (GeometryCollection, int, error) {
	if len(data) < 4 {
		return GeometryCollection{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	gs := make([]Geometry, 0, n)
	for i := 0; i < n; i++ {
		g, used, err := decodeInnerGeometryWKB(data[off:])
		if err != nil {
			return GeometryCollection{}, 0, err
		}
		if g.Type() == TypeGeometryCollection {
			return GeometryCollection{}, 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		gs = append(gs, g)
		off += used
	}
	return GeometryCollection{Geometries: gs, HasZ: hasZ}, off, nil
}

func appendWKBHeader(buf []byte, typ uint32) []byte {
	buf = append(buf, wkbNDR)
	buf = appendUint32LE(buf, typ)
	return buf
}

func appendUint32LE(buf []byte, v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return append(buf, b[:]...)
}

func appendFloat64LE(buf []byte, v float64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	return append(buf, b[:]...)
}

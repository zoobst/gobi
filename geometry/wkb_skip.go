package geometry

import (
	"encoding/binary"
	"fmt"
)

// skip* helpers advance past a WKB body without decoding coord
// bytes. Used by the planar-length / planar-area scanners on types
// that contribute 0 to the running total but whose byte range
// still has to be consumed to keep the offset honest (e.g. a
// GeometryCollection with a Polygon among its members when the
// caller only wants Length).
//
// Each helper takes the body (bytes after the 5-byte
// endianness+typecode header) and returns bytes consumed.

func skipLineString(data []byte, bo binary.ByteOrder, hasZ bool) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return 0, ErrShortWKB
	}
	return 4 + n*cs, nil
}

func skipPolygon(data []byte, bo binary.ByteOrder, hasZ bool) (int, error) {
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
		off += nPts * cs
	}
	return off, nil
}

func skipMultiPoint(data []byte, bo binary.ByteOrder, hasZ bool) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	elemSize := 5 + coordSize(hasZ)
	off := 4
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
		off += elemSize
	}
	return off, nil
}

func skipMultiLineString(data []byte, bo binary.ByteOrder, hasZ bool) (int, error) {
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
		sz, err := skipLineString(data[off+5:], innerBO, hasZ)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

func skipMultiPolygon(data []byte, bo binary.ByteOrder, hasZ bool) (int, error) {
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
		sz, err := skipPolygon(data[off+5:], innerBO, hasZ)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

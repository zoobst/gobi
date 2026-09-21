package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// PointXYZFromWKB extracts (x, y, z) from a Point or PointZ WKB
// blob. For 2D points returns valid=true with z=0; for non-Point
// WKB (LineString / Polygon / null / etc.) returns valid=false and
// no error. Byte-order / short-WKB failures return valid=false + a
// wrapping error so callers can distinguish "not a point" from
// "bad bytes."
//
// Zero-alloc: reads directly from the byte slice via
// encoding/binary. Companion to WKBTypeCode, BoundsFromWKB, and
// the other WKB byte-stream scanners — same shape, same alloc
// profile.
func PointXYZFromWKB(data []byte) (x, y, z float64, valid bool, err error) {
	typ, hasZ, err := WKBTypeCode(data)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if typ != wkbPoint {
		return 0, 0, 0, false, nil
	}
	need := 5 + 16
	if hasZ {
		need = 5 + 24
	}
	if len(data) < need {
		return 0, 0, 0, false, fmt.Errorf("geometry: PointXYZFromWKB: WKB Point too short: %d bytes", len(data))
	}
	var bo binary.ByteOrder
	switch data[0] {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return 0, 0, 0, false, fmt.Errorf("geometry: PointXYZFromWKB: invalid WKB byte order %d", data[0])
	}
	x = math.Float64frombits(bo.Uint64(data[5:13]))
	y = math.Float64frombits(bo.Uint64(data[13:21]))
	if hasZ {
		z = math.Float64frombits(bo.Uint64(data[21:29]))
	}
	return x, y, z, true, nil
}

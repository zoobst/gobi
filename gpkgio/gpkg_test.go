package gpkgio

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/zoobst/gobi/geometry"
)

// buildGPKGGeom constructs a GeoPackage geometry blob (header + WKB payload).
// flags encode envelope size; srsID is written little-endian.
func buildGPKGGeom(flags byte, srsID int32, wkb []byte) []byte {
	header := []byte{'G', 'P', 0, flags}
	var srs [4]byte
	binary.LittleEndian.PutUint32(srs[:], uint32(srsID))
	return append(append(header, srs[:]...), wkb...)
}

func TestDecodeGeometry_PointNoEnvelope(t *testing.T) {
	p := geometry.Point{X: 1, Y: 2}
	blob := buildGPKGGeom(0, 4326, geometry.WKB(p))
	g, err := DecodeGeometry(blob)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("got %T", g)
	}
	if got.X != 1 || got.Y != 2 {
		t.Fatalf("point coords: %+v", got)
	}
	if got.CRSValue.EPSG != 4326 {
		t.Fatalf("CRS = %v", got.CRSValue)
	}
}

func TestDecodeGeometry_MissingMagic(t *testing.T) {
	_, err := DecodeGeometry([]byte{'X', 'X', 0, 0, 0, 0, 0, 0})
	if !errors.Is(err, ErrInvalidHeader) {
		t.Fatalf("want ErrInvalidHeader, got %v", err)
	}
}

func TestDecodeGeometry_WithXYEnvelope(t *testing.T) {
	p := geometry.Point{X: 1, Y: 2}
	// flags bits 3..1 = 001 (XY envelope, 32 bytes)
	flags := byte(1 << 1)
	env := make([]byte, 32) // dummy envelope bytes (min/max x, y)
	// Pass env as buildGPKGGeom's WKB slot so the produced bytes are:
	// 'G','P',0,flags,srs[4],env[32]. Then append the real point WKB.
	blob := append(buildGPKGGeom(flags, 4326, env), geometry.WKB(p)...)
	g, err := DecodeGeometry(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := g.(geometry.Point)
	if got.X != 1 || got.Y != 2 {
		t.Fatalf("coords: %+v", got)
	}
}

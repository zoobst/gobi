package geometry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

func (p Point) Distance(pt2 Point, unit string) float64 {
	return haversine(p, pt2, unit)
}

func (p Point) String() string { return fmt.Sprintf("%f %f", p.X, p.Y) }

func (p Point) Type() string { return "Point" }

func (p Point) Name() string { return p.Type() }

func (p Point) CRS() CRS { return p.CoordRefSys }

func (p Point) WKT() string { return fmt.Sprintf("POINT (%f %f)", p.X, p.Y) }

func (p Point) WKB() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write byte order (1 = little endian)
	if err := binary.Write(buf, binary.LittleEndian, byte(1)); err != nil {
		return nil, err
	}

	// Write geometry type (1 = Point)
	if err := binary.Write(buf, binary.LittleEndian, WKB_POINT); err != nil {
		return nil, err
	}

	// Write coordinates
	if err := binary.Write(buf, binary.LittleEndian, p.X); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, p.Y); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// WKBHex returns the WKB encoding of the Point as a hex string.
func (p Point) WKBHex() (string, error) {
	wkb, err := p.WKB()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(wkb), nil
}

func (p Point) Coords() (fList [][2]float64) {
	fList = [][2]float64{{p.X, p.Y}}
	return fList
}

func (p Point) MaxX() float64 { return p.X }

func (p Point) MaxY() float64 { return p.Y }

func (p Point) MinX() float64 { return p.X }

func (p Point) MinY() float64 { return p.Y }

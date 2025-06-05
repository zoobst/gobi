package geometry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

func NewPoint(x, y float64, crs *CRS) (Point, error) {
	p := Point{
		X: x,
		Y: y,
	}
	if crs == nil {
		if ok := p.checkDegrees(); !ok {
			p.CoordRefSys = PSEUDOMERCATOR
		} else {
			p.CoordRefSys = WGS84
		}
	}
	return p, nil
}

func (p Point) Len() int {
	return 1
}

func (p Point) Equal(other Geometry) bool {
	switch t := other.(type) {
	case *Point:
		if t.X == p.X && t.Y == p.Y && t.CRS().EPSG == p.CRS().EPSG {
			return true
		}
		return false
	default:
		return false
	}
}

func (p Point) ToCRS(epsg int) Geometry {
	newP := Point{
		CoordRefSys: CRSbyEPSG[epsg],
	}
	if p.CRS().Projected && newP.CRS().Projected {
		newP.X, newP.Y = p.X, p.Y
	} else if p.CRS().Projected && !newP.CRS().Projected {
		newP.X, newP.Y = MercatorToLL(p.X, p.Y)
	} else if !p.CRS().Projected && newP.CRS().Projected {
		newP.X, newP.Y = LLToMercator(p.X, p.Y)
	}
	return newP
}

func (p Point) EstimateUTMCRS() CRS {
	epsg := estimateUTMEPSG(p)
	return CRSbyEPSG[epsg]
}

func (p Point) Bounds() Box {
	return Box{p.X, p.Y, p.X, p.Y}
}

func (p Point) Distance(pt2 Point, unit string) float64 {
	if p.CRS().Projected {
		return projectedDistance(&p, &pt2, unit)
	}
	return haversine(&p, &pt2, unit)
}

func (p Point) String() string { return fmt.Sprintf("%f %f", p.X, p.Y) }

func (p Point) Type() string { return "Geometry" }

func (p Point) Name() string { return "Point" }

func (p Point) CRS() CRS { return p.CoordRefSys }

func (p Point) WKT() string { return fmt.Sprintf("POINT(%f %f)", p.X, p.Y) }

func (p Point) WKB() []byte {
	buf := new(bytes.Buffer)

	// Write byte order (1 = little endian)
	if err := binary.Write(buf, binary.LittleEndian, byte(1)); err != nil {
		return nil
	}

	// Write geometry type (1 = Point)
	if err := binary.Write(buf, binary.LittleEndian, WKB_POINT); err != nil {
		return nil
	}

	// Write coordinates
	if err := binary.Write(buf, binary.LittleEndian, p.X); err != nil {
		return nil
	}
	if err := binary.Write(buf, binary.LittleEndian, p.Y); err != nil {
		return nil
	}

	return buf.Bytes()
}

// WKBHex returns the WKB encoding of the Point as a hex string.
func (p Point) WKBHex() (string, error) {
	wkb := p.WKB()
	return hex.EncodeToString(wkb), nil
}

func (pt Point) FromWKB(data []byte) (Point, error) {
	var crs CRS
	if pt.CoordRefSys.Name != "" {
		crs = pt.CoordRefSys
	}
	buf := bytes.NewReader(data)

	// 1. Byte order
	var byteOrder byte
	if err := binary.Read(buf, binary.LittleEndian, &byteOrder); err != nil {
		return pt, fmt.Errorf("failed to read byte order: %w", err)
	}
	var bo binary.ByteOrder
	switch byteOrder {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return pt, errors.New("invalid byte order")
	}

	// 2. Geometry type
	var geomType uint32
	if err := binary.Read(buf, bo, &geomType); err != nil {
		return pt, fmt.Errorf("failed to read geometry type: %w", err)
	}
	if geomType != 1 { // WKB Point = 1
		return pt, fmt.Errorf("unexpected geometry type for Point: %d", geomType)
	}

	// 3. Coordinates
	if err := binary.Read(buf, bo, &pt.X); err != nil {
		return pt, fmt.Errorf("failed to read X: %w", err)
	}
	if err := binary.Read(buf, bo, &pt.Y); err != nil {
		return pt, fmt.Errorf("failed to read Y: %w", err)
	}

	pt.CoordRefSys = crs

	if pt.CoordRefSys.Name == "" {
		pt.CoordRefSys = WGS84
	}
	return pt, nil
}

func (p Point) Coords() (fList [][2]float64) {
	fList = [][2]float64{{p.X, p.Y}}
	return fList
}

func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        string     `json:"type"`
		Coordinates [2]float64 `json:"coordinates"`
	}{
		Type:        p.Name(),
		Coordinates: p.Coords()[0],
	})
}

func (p Point) UnmarshalJSON(data []byte) error {
	temp := struct {
		Type        string     `json:"type"`
		Coordinates [2]float64 `json:"coordinates"`
	}{}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	if temp.Type != p.Name() {
		return errors.New("invalid geometry type for Point")
	}
	p.X = temp.Coordinates[0]
	p.Y = temp.Coordinates[1]
	return nil
}

func (p Point) MinX() float64 { return p.X }

func (p Point) MinY() float64 { return p.Y }

func (p Point) MaxX() float64 { return p.X }

func (p Point) MaxY() float64 { return p.Y }

func (p *Point) Copy() Point {
	copyP := *p
	return copyP
}

func (p Point) checkDegrees() bool {
	if p.X >= -90.0 && p.X <= 90.0 && p.Y >= -180.0 && p.Y <= 180.0 {
		return true
	}
	return false
}

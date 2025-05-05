package geometry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Length calculates the distance between two points on the Earth using a haversine
// calculation. The distance is returned in the unit specified.
//
// The unit argument accepts the following values (case-insensitive):
//   - "km"  : kilometers (default if unknown)
//   - "mi"  : miles
//   - "nmi" : nautical miles
//
// Coordinates are assumed to be in WGS84 format.
//
// Example usage:
//
//	stringLength := l.Length("mi")   // Distance in miles
func (l LineString) Length(unit string) float64 {
	dist := 0.0
	for i := range len(l.Points) - 1 {
		dist += haversine(l.Points[i], l.Points[i+1], unit)
	}
	return dist
}

func (l LineString) String() (strList string) {
	if len(l.Points) == 0 {
		return
	}
	for _, LineString := range l.Points {
		strList += ", " + LineString.String()
	}
	return strList[2:]
}

func (l LineString) Type() string { return "LineString" }

func (l LineString) Name() string { return l.Type() }

func (l LineString) CRS() CRS { return l.Points[0].CoordRefSys }

func (l LineString) WKT() (strList string) {
	strList = "LINESTRING ("
	for _, LineString := range l.Points {
		strList += fmt.Sprintf("(%f %f),", LineString.X, LineString.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (l LineString) Coords() (fList [][2]float64) {
	for _, LineString := range l.Points {
		fList = append(fList, [2]float64{LineString.X, LineString.Y})
	}
	return fList
}

func (l LineString) WKB() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Byte order: 1 = little endian
	if err := binary.Write(buf, binary.LittleEndian, byte(1)); err != nil {
		return nil, err
	}

	// Geometry type: 2 = LineString
	if err := binary.Write(buf, binary.LittleEndian, WKB_LINESTRING); err != nil {
		return nil, err
	}

	// Number of points
	numPoints := uint32(l.Len())
	if err := binary.Write(buf, binary.LittleEndian, numPoints); err != nil {
		return nil, err
	}

	// Write all points (X, Y)
	for _, pt := range l.Points {
		if err := binary.Write(buf, binary.LittleEndian, pt.X); err != nil {
			return nil, err
		}
		if err := binary.Write(buf, binary.LittleEndian, pt.Y); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// WKBHex returns the WKB encoding of the LineString as a hex string.
func (l LineString) WKBHex() (string, error) {
	wkb, err := l.WKB()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(wkb), nil
}

func (l LineString) Len() int {
	return len(l.Points)
}

func (l LineString) MaxX() float64 { return maxX(&l.Points) }

func (l LineString) MaxY() float64 { return maxY(&l.Points) }

func (l LineString) MinX() float64 { return minX(&l.Points) }

func (l LineString) MinY() float64 { return minY(&l.Points) }

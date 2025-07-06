package geometry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

func (m MultiPoint) Equal(other Geometry) bool {
	switch t := other.(type) {
	case *Polygon:
		if t.Len() != m.Len() {
			return false
		}
		for i := range m.Len() {
			if !m.PointList[i].Equal(t.Points[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (m MultiPoint) ToCRS(epsg int32) (newMultiPoint MultiPoint, err error) {
	for _, p := range m.PointList {
		newPoint, err := p.ToCRS(epsg)
		if err != nil {
			return newMultiPoint, err
		}
		newMultiPoint.PointList = append(newMultiPoint.PointList, newPoint)
	}
	return newMultiPoint, nil
}

func (m MultiPoint) EstimateUTMCRS() *CRS {
	epsg := estimateUTMEPSG(m)
	newCRS := CRSbyEPSG[epsg]
	return &newCRS
}

func (m MultiPoint) Bounds() Box {
	return [4]float64{m.MinX(), m.MinY(), m.MaxX(), m.MaxY()}
}

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
func (m MultiPoint) Length(unit string) float64 {
	dist := 0.0
	for i := range len(m.PointList) - 1 {
		if m.CRS().Projected {
			dist += projectedDistance(&m.PointList[i], &m.PointList[i+1], unit)
		} else {
			dist += haversine(&m.PointList[i], &m.PointList[i+1], unit)
		}
	}
	return dist
}

func (m MultiPoint) Centroid() Point {
	var (
		n = m.Len()
	)
	// average of points
	var sx, sy float64
	for _, pt := range m.PointList {
		sx += pt.X
		sy += pt.Y
	}
	return Point{
		X: sx / float64(n),
		Y: sy / float64(n),
	}
}

func (m MultiPoint) String() (strList string) {
	if len(m.PointList) == 0 {
		return
	}
	for _, MultiPoint := range m.PointList {
		strList += ", " + MultiPoint.String()
	}
	return strList[2:]
}

func (m MultiPoint) Type() string { return "Geometry" }

func (m MultiPoint) Name() string { return "MultiPoint" }

func (m MultiPoint) CRS() *CRS { return &m.PointList[0].CoordRefSys }

func (m MultiPoint) WKT() (strList string) {
	strList = "MULTIPOINT ("
	for _, MultiPoint := range m.PointList {
		strList += fmt.Sprintf("(%f %f),", MultiPoint.X, MultiPoint.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (m MultiPoint) Coords() (fList [][2]float64) {
	for _, MultiPoint := range m.PointList {
		fList = append(fList, [2]float64{MultiPoint.X, MultiPoint.Y})
	}
	return fList
}

func (m MultiPoint) WKB() []byte {
	buf := new(bytes.Buffer)

	// Byte order: 1 = little endian
	if err := binary.Write(buf, binary.LittleEndian, byte(1)); err != nil {
		return nil
	}

	// Geometry type: 2 = MultiPoint
	if err := binary.Write(buf, binary.LittleEndian, WKB_LINESTRING); err != nil {
		return nil
	}

	numPoints := uint32(m.Len())
	if err := binary.Write(buf, binary.LittleEndian, numPoints); err != nil {
		return nil
	}

	// Write all points (X, Y)
	for _, pt := range m.PointList {
		if err := binary.Write(buf, binary.LittleEndian, pt.X); err != nil {
			return nil
		}
		if err := binary.Write(buf, binary.LittleEndian, pt.Y); err != nil {
			return nil
		}
	}

	return buf.Bytes()
}

// WKBHex returns the WKB encoding of the MultiPoint as a hex string.
func (m MultiPoint) WKBHex() (string, error) {
	wkb := m.WKB()
	return hex.EncodeToString(wkb), nil
}

func (m MultiPoint) FromWKB(data []byte) (MultiPoint, error) {
	crs := m.PointList[0].CoordRefSys
	buf := bytes.NewReader(data)

	// 1. Byte order
	var byteOrder byte
	if err := binary.Read(buf, binary.LittleEndian, &byteOrder); err != nil {
		return m, fmt.Errorf("failed to read byte order: %w", err)
	}
	var bo binary.ByteOrder
	switch byteOrder {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return m, errors.New("invalid byte order")
	}

	// 2. Geometry type
	var geomType uint32
	if err := binary.Read(buf, bo, &geomType); err != nil {
		return m, fmt.Errorf("failed to read geometry type: %w", err)
	}
	if geomType != 2 { // WKB MultiPoint = 2
		return m, fmt.Errorf("unexpected geometry type for MultiPoint: %d", geomType)
	}

	// 3. Number of points
	var numPoints uint32
	if err := binary.Read(buf, bo, &numPoints); err != nil {
		return m, fmt.Errorf("failed to read number of points: %w", err)
	}

	// 4. Read each point
	points := make([]Point, 0, numPoints)
	for i := range int(numPoints) {
		var x, y float64
		if err := binary.Read(buf, bo, &x); err != nil {
			return m, fmt.Errorf("failed to read x at point %d: %w", i, err)
		}
		if err := binary.Read(buf, bo, &y); err != nil {
			return m, fmt.Errorf("failed to read y at point %d: %w", i, err)
		}
		points = append(points, Point{X: x, Y: y, CoordRefSys: crs})
	}

	m.PointList = points
	for _, pts := range m.PointList {
		if pts.CoordRefSys.Name == "" {
			pts.CoordRefSys = WGS84 // Default if not stored in WKB
		}
	}
	return m, nil
}

func (m MultiPoint) Len() int {
	return len(m.PointList)
}

func (m MultiPoint) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        string       `json:"type"`
		Coordinates [][2]float64 `json:"coordinates"`
	}{
		Type:        m.Name(),
		Coordinates: m.Coords(),
	})
}

func (p MultiPoint) UnmarshalJSON(data []byte) error {
	temp := struct {
		Type        string       `json:"type"`
		Coordinates [][2]float64 `json:"coordinates"`
	}{}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	if temp.Type != p.Name() {
		return errors.New("invalid geometry type for Polygon")
	}

	for _, pt := range temp.Coordinates {
		p.PointList = append(p.PointList, Point{X: pt[0], Y: pt[1]})
	}

	return nil
}

func (m MultiPoint) MaxX() float64 { return maxX(&m.PointList) }

func (m MultiPoint) MaxY() float64 { return maxY(&m.PointList) }

func (m MultiPoint) MinX() float64 { return minX(&m.PointList) }

func (m MultiPoint) MinY() float64 { return minY(&m.PointList) }

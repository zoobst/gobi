package geometry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

func (l LineString) Equal(other Geometry) bool {
	switch t := other.(type) {
	case *Polygon:
		if t.Len() != l.Len() {
			return false
		}
		for i := range l.Len() {
			if !l.Points[i].Equal(t.Points[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (l LineString) ToCRS(epsg int32) (newL LineString, err error) {
	for _, p := range l.Points {
		p, err = p.ToCRS(epsg)
		if err != nil {
			return newL, err
		}

		newL.Points = append(newL.Points, p)
	}
	return newL, nil
}

func (l LineString) EstimateUTMCRS() CRS {
	epsg := estimateUTMEPSG(l)
	return CRSbyEPSG[epsg]
}

func (l LineString) Bounds() Box {
	return [4]float64{l.MinX(), l.MinY(), l.MaxX(), l.MaxY()}
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
func (l LineString) Length(unit string) float64 {
	dist := 0.0
	for i := range len(l.Points) - 1 {
		if l.CRS().Projected {
			dist += projectedDistance(&l.Points[i], &l.Points[i+1], unit)
		} else {
			dist += haversine(&l.Points[i], &l.Points[i+1], unit)
		}
	}
	return dist
}

func (l LineString) Centroid() Point {
	var (
		n = l.Len()
	)
	// average of points
	var sx, sy float64
	for _, pt := range l.Points {
		sx += pt.X
		sy += pt.Y
	}
	return Point{
		X: sx / float64(n),
		Y: sy / float64(n),
	}
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

func (l LineString) Type() string { return "Geometry" }

func (l LineString) Name() string { return "LineString" }

func (l LineString) CRS() *CRS { return &l.Points[0].CoordRefSys }

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

func (l LineString) WKB() []byte {
	buf := new(bytes.Buffer)

	// Byte order: 1 = little endian
	if err := binary.Write(buf, binary.LittleEndian, byte(1)); err != nil {
		return nil
	}

	// Geometry type: 2 = LineString
	if err := binary.Write(buf, binary.LittleEndian, WKB_LINESTRING); err != nil {
		return nil
	}

	numPoints := uint32(l.Len())
	if err := binary.Write(buf, binary.LittleEndian, numPoints); err != nil {
		return nil
	}

	// Write all points (X, Y)
	for _, pt := range l.Points {
		if err := binary.Write(buf, binary.LittleEndian, pt.X); err != nil {
			return nil
		}
		if err := binary.Write(buf, binary.LittleEndian, pt.Y); err != nil {
			return nil
		}
	}

	return buf.Bytes()
}

// WKBHex returns the WKB encoding of the LineString as a hex string.
func (l LineString) WKBHex() (string, error) {
	wkb := l.WKB()
	return hex.EncodeToString(wkb), nil
}

func (ls LineString) FromWKB(data []byte) (LineString, error) {
	crs := ls.Points[0].CoordRefSys
	buf := bytes.NewReader(data)

	// 1. Byte order
	var byteOrder byte
	if err := binary.Read(buf, binary.LittleEndian, &byteOrder); err != nil {
		return ls, fmt.Errorf("failed to read byte order: %w", err)
	}
	var bo binary.ByteOrder
	switch byteOrder {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return ls, errors.New("invalid byte order")
	}

	// 2. Geometry type
	var geomType uint32
	if err := binary.Read(buf, bo, &geomType); err != nil {
		return ls, fmt.Errorf("failed to read geometry type: %w", err)
	}
	if geomType != 2 { // WKB LineString = 2
		return ls, fmt.Errorf("unexpected geometry type for LineString: %d", geomType)
	}

	// 3. Number of points
	var numPoints uint32
	if err := binary.Read(buf, bo, &numPoints); err != nil {
		return ls, fmt.Errorf("failed to read number of points: %w", err)
	}

	// 4. Read each point
	points := make([]Point, 0, numPoints)
	for i := range int(numPoints) {
		var x, y float64
		if err := binary.Read(buf, bo, &x); err != nil {
			return ls, fmt.Errorf("failed to read x at point %d: %w", i, err)
		}
		if err := binary.Read(buf, bo, &y); err != nil {
			return ls, fmt.Errorf("failed to read y at point %d: %w", i, err)
		}
		points = append(points, Point{X: x, Y: y, CoordRefSys: crs})
	}

	ls.Points = points
	for _, pts := range ls.Points {
		if pts.CoordRefSys.Name == "" {
			pts.CoordRefSys = WGS84 // Default if not stored in WKB
		}
	}
	return ls, nil
}

func (l LineString) Len() int {
	return len(l.Points)
}

func (p LineString) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        string       `json:"type"`
		Coordinates [][2]float64 `json:"coordinates"`
	}{
		Type:        p.Name(),
		Coordinates: p.Coords(),
	})
}

func (p LineString) UnmarshalJSON(data []byte) error {
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
		p.Points = append(p.Points, Point{X: pt[0], Y: pt[1]})
	}

	return nil
}

func (l LineString) MaxX() float64 { return maxX(&l.Points) }

func (l LineString) MaxY() float64 { return maxY(&l.Points) }

func (l LineString) MinX() float64 { return minX(&l.Points) }

func (l LineString) MinY() float64 { return minY(&l.Points) }

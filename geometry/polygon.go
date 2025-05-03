package geometry

import (
	"fmt"
	"math"
	"strings"
)

// PerimeterLength calculates the distance between all points along the Polygon's
// perimeter using a haversine calculation. The distance is returned in the unit specified.
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
//	polyPerimeterLength := poly.PerimeterLength("mi")   // Distance in miles
func (p Polygon) PerimeterLength(unit string) float64 {
	dist := 0.0
	for i := range len(p.Points) - 1 {
		dist += haversine(p.Points[i], p.Points[i+1], unit)
	}
	return dist
}

// Area calculates the area (in square kilometers) of a polygon
// on the Earth's surface assuming the coordinates are (lon, lat) in degrees.
func (p Polygon) Area(unit string) float64 {
	var R float64
	switch strings.ToLower(unit) {
	case "mi":
		R = 3958.8 // Miles
	case "nmi":
		R = 3440.1 // Nautical miles
	case "km":
		fallthrough
	default:
		R = 6371.0 // Kilometers (default)
	}
	// ensure a closed Polygon
	p.checkClosedPolygon()

	excess := p.sphericalExcess()
	area := math.Abs(excess) * R * R
	return area
}

func (p Polygon) String() (strList string) {
	if len(p.Points) == 0 {
		return
	}
	for _, point := range p.Points {
		strList += ", " + point.String()
	}
	return strList[2:]
}

func (p Polygon) Type() string { return "Polygon" }

func (p Polygon) Name() string { return "Polygon" }

func (p Polygon) WKT() (strList string) {
	strList = "POLYGON ("
	for _, point := range p.Points {
		strList += fmt.Sprintf("(%f %f),", point.X, point.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (p Polygon) Coords() (fList [][2]float64) {
	for _, point := range p.Points {
		fList = append(fList, [2]float64{point.X, point.Y})
	}
	return fList
}

func (p Polygon) MaxX() float64 { return maxX(&p.Points) }

func (p Polygon) MaxY() float64 { return maxY(&p.Points) }

func (p Polygon) MinX() (lVal float64) { return minX(&p.Points) }

func (p Polygon) MinY() float64 { return minY(&p.Points) }

func (p Polygon) Len() int {
	return len(p.Points)
}

func (p *Polygon) checkClosedPolygon() {
	if p.Points[0] != p.Points[len(p.Points)-1] {
		copyPoint := Coord(p.Points[0].Coords()[0])
		p.Points = append(p.Points, copyPoint.ToPoint())
	}
}

func (p Polygon) sphericalExcess() float64 {
	total := 0.0
	for i := range p.Len() - 1 {
		lon1 := degreesToRadians(p.Points[i].X)
		lat1 := degreesToRadians(p.Points[i].Y)
		lon2 := degreesToRadians(p.Points[i+1].X)
		lat2 := degreesToRadians(p.Points[i+1].Y)

		total += (lon2 - lon1) * (math.Sin(lat1) + math.Sin(lat2))
	}

	return total / 2.0
}

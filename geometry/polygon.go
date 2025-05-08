package geometry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

func (p Polygon) Equal(other Geometry) bool {
	switch t := other.(type) {
	case *Polygon:
		if t.Len() != p.Len() {
			return false
		}
		for i := range p.Len() {
			if !p.Points[i].Equal(t.Points[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (p Polygon) ToCRS(epsg int) Geometry {
	newP := Polygon{}
	for _, pt := range p.Points {
		newP.Points = append(newP.Points, pt.ToCRS(epsg).(Point))
	}
	return newP
}

func (p Polygon) EstimateUTMCRS() CRS {
	epsg := estimateUTMEPSG(p)
	return CRSbyEPSG[epsg]
}

func (p Polygon) Bounds() Box {
	return [4]float64{p.MinX(), p.MinY(), p.MaxX(), p.MaxY()}
}

// PerimeterLength calculates the distance between all points along the Polygon's
// perimeter. The distance is returned in the unit specified.
//
// The unit argument accepts the following values (case-insensitive):
//   - "km"  : kilometers (default if unknown)
//   - "mi"  : miles
//   - "nmi" : nautical miles
//
// Example usage:
//
//	polyPerimeterLength := poly.PerimeterLength("mi")   // Distance in miles
func (p Polygon) PerimeterLength(unit string) float64 {
	dist := 0.0
	for i := range len(p.Points) - 1 {
		if p.CRS().Projected {
			dist += projectedDistance(&p.Points[i], &p.Points[i+1], unit)
		} else {
			dist += haversine(&p.Points[i], &p.Points[i+1], unit)
		}
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

// ConvexHull computes the convex hull of the points in the polygon using Graham's scan.
// Returns a new Polygon containing only the convex hull.
func (p Polygon) ConvexHull() Polygon {
	// Find the point with the lowest Y (and leftmost if tie)
	lowest := p.Points[0]
	lowestIdx := 0
	for i, pt := range p.Points {
		if pt.Y < lowest.Y || (pt.Y == lowest.Y && pt.X < lowest.X) {
			lowest = pt
			lowestIdx = i
		}
	}

	// Move the lowest point to the first position
	p.Points[0], p.Points[lowestIdx] = p.Points[lowestIdx], p.Points[0]

	// Sort the rest of the points based on polar angle with respect to lowest point
	pivot := p.Points[0]
	sorted := make([]Point, len(p.Points)-1)
	copy(sorted, p.Points[1:])
	sort.Slice(sorted, func(i, j int) bool {
		cp := crossProduct(pivot, sorted[i], sorted[j])
		if cp == 0 {
			// Collinear points: closer one comes first
			return distanceSquared(pivot, sorted[i]) < distanceSquared(pivot, sorted[j])
		}
		return cp > 0
	})

	// Build the convex hull
	hull := []Point{pivot, sorted[0]}
	for i := 1; i < len(sorted); i++ {
		for len(hull) >= 2 && crossProduct(hull[len(hull)-2], hull[len(hull)-1], sorted[i]) <= 0 {
			hull = hull[:len(hull)-1] // pop
		}
		hull = append(hull, sorted[i])
	}

	return Polygon{Points: hull}
}

func (p Polygon) Centroid() Point {
	var (
		n               = p.Len()
		cx, cy, areaSum float64
	)

	for i := range n {
		j := (i + 1) % n
		x0, y0 := p.Points[i].X, p.Points[i].Y
		x1, y1 := p.Points[j].X, p.Points[j].Y

		// Shoelace formula term
		areaTerm := x0*y1 - x1*y0
		areaSum += areaTerm

		// Centroid terms
		cx += (x0 + x1) * areaTerm
		cy += (y0 + y1) * areaTerm
	}

	area := areaSum / 2.0
	if area == 0 {
		// Degenerate polygon, fallback to average of points
		var sx, sy float64
		for _, pt := range p.Points {
			sx += pt.X
			sy += pt.Y
		}
		return Point{
			X: sx / float64(n),
			Y: sy / float64(n),
		}
	}

	cx /= (6.0 * area)
	cy /= (6.0 * area)

	return Point{
		X: cx,
		Y: cy,
	}
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

func (p Polygon) Type() string { return "geometry" }

func (p Polygon) Name() string { return "Polygon" }

func (p Polygon) CRS() CRS { return p.Points[0].CoordRefSys }

func (p Polygon) WKT() (strList string) {
	strList = "POLYGON ("
	for _, point := range p.Points {
		strList += fmt.Sprintf("(%f %f),", point.X, point.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (p Polygon) WKB() []byte {
	buf := new(bytes.Buffer)

	// Byte order: 1 = little endian
	if err := binary.Write(buf, binary.LittleEndian, byte(1)); err != nil {
		return nil
	}

	// Geometry type: Polygon (3)
	if err := binary.Write(buf, binary.LittleEndian, WKB_POLYGON); err != nil {
		return nil
	}

	// WKB Polygons consist of one or more "linear rings"
	ring := p.Points

	// Ensure the ring is closed (first point == last point)
	if len(ring) > 0 && (ring[0].X != ring[len(ring)-1].X || ring[0].Y != ring[len(ring)-1].Y) {
		ring = append(ring, ring[0])
	}

	// Number of rings: 1
	if err := binary.Write(buf, binary.LittleEndian, uint32(1)); err != nil {
		return nil
	}

	// Number of points in ring
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(ring))); err != nil {
		return nil
	}

	// Write each point (X, Y)
	for _, pt := range ring {
		if err := binary.Write(buf, binary.LittleEndian, pt.X); err != nil {
			return nil
		}
		if err := binary.Write(buf, binary.LittleEndian, pt.Y); err != nil {
			return nil
		}
	}

	return buf.Bytes()
}

// WKBHex returns the WKB encoding of the Polygon as a hex string.
func (p Polygon) WKBHex() (string, error) {
	wkb := p.WKB()
	return hex.EncodeToString(wkb), nil
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
	if p.Points[0] != p.Points[p.Len()-1] {
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

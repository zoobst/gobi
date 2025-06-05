package geometry

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"

	berrors "github.com/zoobst/gobi/bErrors"
)

func ParseWKB(data []byte) (Geometry, error) {
	if len(data) < 5 {
		return nil, errors.New("WKB too short to contain type")
	}

	byteOrder := data[0]
	var bo binary.ByteOrder
	switch byteOrder {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return nil, errors.New("invalid byte order in WKB")
	}

	// Read geometry type (next 4 bytes)
	geomType := bo.Uint32(data[1:5])

	switch geomType {
	case WKB_POINT:
		return Point{}.FromWKB(data)
	case WKB_LINESTRING:
		return LineString{}.FromWKB(data)
	case WKB_POLYGON:
		return Polygon{}.FromWKB(data)
	default:
		return nil, fmt.Errorf("unsupported WKB geometry type: %d", geomType)
	}
}

func ParseStringGeometry(s string) (geom Geometry, err error) {
	// Try to parse using WKT
	if geom, err = ParseWKT(s); err == nil {
		return
	}
	if geom, err = ParseStringCoords(s); err == nil {
		return
	}
	return nil, fmt.Errorf(berrors.ErrUnableToParseStringCoords.Error(), s)
}

// ParsePoint parses WKT string of Points, LineStrings, and Polygons
func ParseWKT(s string) (t Geometry, err error) {
	if (len(s) > 5) && (s[:5] == "POINT") {
		if t, err = ParsePointWKT(s); err == nil {
			return
		}
	} else if (len(s) > 7) && (s[:7] == "POLYGON") {
		if t, err = ParsePolygonWKT(s); err == nil {
			return
		}
	} else if (len(s) > 10) && (s[:10] == "LINESTRING") {
		if t, err = ParseLineStringWKT(s); err == nil {
			return
		}
	} else {
		return nil, fmt.Errorf(berrors.ErrInvalidGeometryType.Error(), t)
	}
	return nil, err
}

// ParsePoint parses a WKT Point string
func ParsePointWKT(s string) (Point, error) {
	var coords Coord
	s = strings.TrimPrefix(s, "POINT(")
	s = strings.TrimPrefix(s, "POINT (")
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ")")
	_, err := fmt.Sscanf(s, "%f %f", &coords[0], &coords[1])
	if err != nil {
		return Point{}, err
	}
	return Point{X: coords[0], Y: coords[1]}, nil
}

// ParseLineString parses a WKT LineString string
func ParseLineStringWKT(s string) (LineString, error) {
	s = strings.TrimPrefix(s, "LINESTRING(")
	s = strings.TrimPrefix(s, "LINESTRING (")
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ")")
	coords := strings.Split(s, ",")
	var points []Point
	for _, coord := range coords {
		coord = strings.TrimSpace(coord)
		p, err := ParsePointWKT(coord)
		if err != nil {
			return LineString{}, err
		}
		points = append(points, p)
	}
	return LineString{Points: points}, nil
}

// ParsePolygon parses a WKT Polygon string
func ParsePolygonWKT(s string) (Polygon, error) {
	var points []Point
	s = strings.TrimPrefix(s, "POLYGON ((")
	s = strings.TrimPrefix(s, "POLYGON((")
	s = strings.TrimSuffix(s, "))")
	rings := strings.SplitSeq(s, "),(")
	for ring := range rings {
		coords := strings.SplitSeq(ring, ",")
		for coord := range coords {
			coord = strings.TrimSpace(coord)
			p, err := ParsePointWKT(coord)
			if err != nil {
				return Polygon{}, err
			}
			points = append(points, p)
		}
	}
	log.Println("Points:", points)
	return Polygon{Points: points}, nil
}

func ParseStringCoords(s string) (Geometry, error) {
	var x, y float64

	// Try common coordinate formats
	formats := []string{
		"%f,%f",
		"%f, %f",
		"(%f,%f)",
		"(%f, %f)",
		"%f %f",
	}

	for _, format := range formats {
		if _, err := fmt.Sscanf(s, format, &x, &y); err == nil {
			return Point{X: x, Y: y, CoordRefSys: WGS84}, nil
		}
	}

	return nil, fmt.Errorf(berrors.ErrUnableToParseStringCoords.Error(), s)
}

func maxY(points *[]Point) (hVal float64) {
	hVal = -10_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.Y > hVal {
			hVal = point.Y
		}
	}
	return hVal
}

func maxX(points *[]Point) (hVal float64) {
	hVal = -10_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.X > hVal {
			hVal = point.X
		}
	}
	return hVal
}

func minY(points *[]Point) (lVal float64) {
	lVal = 10_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.Y < lVal {
			lVal = point.Y
		}
	}
	return lVal
}

func minX(points *[]Point) (lVal float64) {
	lVal = 1_000_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.X < lVal {
			lVal = point.X
		}
	}
	return lVal
}

func (b Box) maxBox(b2 Box) (bigBox Box) {
	return Box{
		minX(&[]Point{Point{X: b[0], Y: b[1]}, Point{X: b2[0], Y: b2[1]}}),
		minY(&[]Point{Point{X: b[0], Y: b[1]}, Point{X: b2[0], Y: b2[1]}}),
		maxX(&[]Point{Point{X: b[2], Y: b[3]}, Point{X: b2[2], Y: b2[3]}}),
		maxY(&[]Point{Point{X: b[2], Y: b[3]}, Point{X: b2[2], Y: b2[3]}}),
	}
}

func estimateUTMEPSG(g Geometry) int {
	if g.CRS().Zone != "" {
		return g.CRS().EPSG
	}
	var p Point
	switch t := g.(type) {
	case *Point:
		p = t.Copy()
	case *Polygon:
		p = t.Centroid()
	case *LineString:
		p = t.Centroid()
	default:
		log.Fatal(fmt.Errorf(berrors.ErrInvalidGeometryType.Error(), g))
	}

	// Handle Psuedo-Mercator; convert to 4326
	if g.CRS().Projected {
		p.X, p.Y = MercatorToLL(p.X, p.Y)
	}

	zone := int((p.X+180)/6) + 1
	if p.Y >= 0 {
		return 32600 + zone // Northern hemisphere
	}
	return 32700 + zone // Southern hemisphere
}

// haversine calculates the great-circle distance between two points on the Earth.
// The two points must be provided as longitude/latitude pairs (in degrees),
// and the distance is returned in the unit specified.
//
// The unit argument accepts the following values (case-insensitive):
//   - "km"  : kilometers (default if unknown)
//   - "mi"  : miles
//   - "nmi" : nautical miles
//
// Coordinates are assumed to be in WGS84 (standard GPS) format.
//
// Example usage:
//
//	p1 := Point{X: -73.9857, Y: 40.7484} // NYC (lon, lat)
//	p2 := Point{X: -0.1276, Y: 51.5074}  // London
//	dist := Haversine(p1, p2, "mi")      // Distance in miles
//
// Note: For small distances (<1km), consider Vincenty's formula for better accuracy.
func haversine(p1, p2 *Point, unit string) float64 {
	// Earth radius by unit
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

	lat1 := degreesToRadians(p1.Y)
	lon1 := degreesToRadians(p1.X)
	lat2 := degreesToRadians(p2.Y)
	lon2 := degreesToRadians(p2.X)

	dlat := lat2 - lat1
	dlon := lon2 - lon1

	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dlon/2)*math.Sin(dlon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}

func projectedDistance(p1, p2 *Point, unit string) float64 {
	dx := p2.X - p1.X
	dy := p2.Y - p1.Y
	metersDist := math.Sqrt(dx*dx + dy*dy)

	if unit == "m" {
		return metersDist
	}
	return metersToUnit(metersDist, unit)
}

func metersToUnit(meters float64, unit string) float64 {
	switch unit {
	case "km":
		return meters / 1000
	case "mi":
		return meters / 1609.344
	case "ft":
		return meters / 0.3048
	case "nmi":
		return meters / 1852
	default:
		log.Fatal(fmt.Errorf(berrors.ErrInvalidUnit.Error(), unit))
	}
	return 0.0
}

func degreesToRadians(deg float64) float64 {
	return deg * math.Pi / 180
}

// crossProduct returns the cross product of vectors AB and AC
func crossProduct(a, b, c Point) float64 {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

// distanceSquared returns the squared distance between two points
func distanceSquared(a, b Point) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return dx*dx + dy*dy
}

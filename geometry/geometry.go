package geometry

import (
	"fmt"
	"math"
	"strings"

	berrors "github.com/zoobst/gobi/bErrors"
)

// ParsePoint parses a WKT Point string
func ParseWKT(s string) (Geometry, error) {
	if (len(s) >= 5) && (s[:5] == "POINT") {
		t, err := ParsePointWKT(s)
		if err != nil {
			return nil, err
		}
		return t, nil
	} else if (len(s) >= 7) && (s[:7] == "POLYGON") {
		t, err := ParsePolygonWKT(s)
		if err != nil {
			return nil, err
		}
		return t, nil
	} else if (len(s) >= 10) && (s[:10] == "LINESTRING") {
		t, err := ParseLineStringWKT(s)
		if err != nil {
			return nil, err
		}
		return t, nil
	} else {
		return nil, berrors.ErrInvalidGeometryType
	}
}

// ParsePoint parses a WKT Point string
func ParsePointWKT(s string) (Point, error) {
	var coords [2]float64
	fmt.Scanf("%f %f", coords[0], coords[1])
	return Point{X: coords[0], Y: coords[1]}, nil
}

// ParseLineString parses a WKT LineString string
func ParseLineStringWKT(s string) (LineString, error) {
	s = strings.TrimPrefix(s, "LINESTRING")
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
	return Polygon{Points: points}, nil
}

func maxY[p *[]Point](points *[]Point) (hVal float64) {
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

func maxX[p *[]Point](points *[]Point) (hVal float64) {
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

func minY[p *[]Point](points *[]Point) (lVal float64) {
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

func minX[p *[]Point](points *[]Point) (lVal float64) {
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
func haversine(p1, p2 Point, unit string) float64 {
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

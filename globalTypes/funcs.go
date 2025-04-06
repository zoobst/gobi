package globalTypes

import (
	"fmt"
	"strings"

	berrors "github.com/zoobst/gobi/bErrors"
)

func CheckGeometry(s string) bool {
	if _, err := ParseWKT(s); err == nil {
		return true
	}
	return false
}

// ParsePoint parses a WKT Point string
func ParseWKT(s string) (Geometry, error) {
	if s[:5] == "POINT" {
		t, err := ParsePointWKT(s)
		if err != nil {
			return nil, err
		}
		return t, nil
	} else if s[:7] == "POLYGON" {
		t, err := ParsePolygonWKT(s)
		if err != nil {
			return nil, err
		}
		return t, nil
	} else if s[:10] == "LINESTRING" {
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
	rings := strings.Split(s, "),(")
	for _, ring := range rings {
		coords := strings.Split(ring, ",")
		for _, coord := range coords {
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

func NewHashSet() *HashSet {
	return &HashSet{
		data: make(map[any]struct{}),
	}
}

func (hs *HashSet) Add(Value any) { hs.data[Value] = struct{}{} }

func (hs *HashSet) Remove(Value any) {
	delete(hs.data, Value)
}

func (hs *HashSet) Contains(Value any) bool {
	_, exists := hs.data[Value]
	return exists
}

func (hs *HashSet) Len() int { return len(hs.data) }

func (hs *HashSet) Clear() {
	hs.data = make(map[any]struct{})
}

func (hs *HashSet) List() []any {
	result := make([]any, 0, len(hs.data))
	for key := range hs.data {
		result = append(result, key)
	}
	return result
}

package globalTypes

import (
	"fmt"
	"log"

	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/geometry"
)

func ParseStringGeometry(s string) (geom Geometry, err error) {
	g, err := geometry.ParseStringGeometry(s)
	if err != nil {
		return nil, err
	}
	switch t := g.(type) {
	case geometry.Point:
		geom = NewPointFromGeometry(&t)
		return
	case geometry.LineString:
		geom = NewLineStringFromGeometry(&t)
		return
	case geometry.Polygon:
		geom = NewPolygonFromGeometry(&t)
		return
	default:
		log.Println(t)
		return nil, fmt.Errorf(berrors.ErrUnableToParseStringCoords.Error(), s)
	}
}

func CheckGeometry(s string) bool {
	if _, err := geometry.ParseWKT(s); err == nil {
		return true
	}
	return false
}

func GenericGeometry() Geometry {
	return Point{}
}

func GenericPoint() Geometry {
	return Point{}
}

func GenericPolygon() Geometry {
	return Polygon{}
}

func GenericLineString() Geometry {
	return LineString{}
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

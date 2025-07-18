package geometry

import (
	"fmt"
	"log"
)

type Geometry interface {
	fmt.Stringer
	Len() int
	CRS() *CRS
	String() string
	Type() string
	WKT() string
	WKB() []byte
	WKBHex() (string, error)
	Coords() [][2]float64
	Bounds() Box
	MarshalJSON() ([]byte, error)
	UnmarshalJSON([]byte) error
	MaxX() float64
	MaxY() float64
	MinX() float64
	MinY() float64
}

type CRS struct {
	BoundBox Box

	Name      string
	AreaOfUse string
	Zone      string

	EPSG int32

	Projected bool
}

// minx, miny, maxx, maxy
type Box [4]float64

// x, y (lon, lat)
type Coord [2]float64

func (c Coord) ToPoint() Point {
	p, err := NewPoint(c[0], c[1], nil)
	if err != nil {
		log.Fatal(err)
	}
	return p
}

// x, y (lon, lat)
type Point struct {
	fmt.Stringer
	CoordRefSys CRS

	X float64 `json:"lon"`
	Y float64 `json:"lat"`
}

type Polygon struct {
	fmt.Stringer
	Points []Point
}

type LineString struct {
	fmt.Stringer
	Points []Point
}

type GeometryCollection struct {
	fmt.Stringer
	Geometries []Geometry `json:"geometries"`
}

type MultiPoint struct {
	fmt.Stringer
	PointList []Point
}

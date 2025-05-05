package geometry

import (
	"fmt"
)

type Geometry interface {
	fmt.Stringer
	CRS() CRS
	String() string
	Type() string
	WKT() string
	WKB() ([]byte, error)
	WKBHex() (string, error)
	Coords() [][2]float64
	MaxX() float64
	MaxY() float64
	MinX() float64
	MinY() float64
}

type CRS struct {
	CRS  string
	EPSG int
}

type Coord [2]float64

func (c Coord) ToPoint() Point {
	return Point{X: c[0], Y: c[1]}
}

type Point struct {
	fmt.Stringer
	Geometry
	CoordRefSys CRS
	X           float64 `json:"lon"`
	Y           float64 `json:"lat"`
}

type Polygon struct {
	fmt.Stringer
	Geometry
	Points []Point
}

type LineString struct {
	fmt.Stringer
	Geometry
	Points []Point
}

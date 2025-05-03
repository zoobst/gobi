package geometry

import (
	"fmt"
)

type Geometry interface {
	fmt.Stringer
	String() string
	Type() string
	WKT() string
	Coords() [][2]float64
	MaxX() float64
	MaxY() float64
	MinX() float64
	MinY() float64
}

type Coord [2]float64

func (c Coord) ToPoint() Point {
	return Point{X: c[0], Y: c[1]}
}

type Point struct {
	fmt.Stringer
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

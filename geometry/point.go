package geometry

import "fmt"

func (p Point) String() string { return fmt.Sprintf("%f %f", p.X, p.Y) }

func (p Point) Type() string { return "Point" }

func (p Point) WKT() string { return fmt.Sprintf("POINT (%f %f)", p.X, p.Y) }

func (p Point) Coords() (fList [][2]float64) {
	fList = [][2]float64{{p.X, p.Y}}
	return fList
}

func (p Point) MaxX() float64 { return p.X }

func (p Point) MaxY() float64 { return p.Y }

func (p Point) MinX() float64 { return p.X }

func (p Point) MinY() float64 { return p.Y }

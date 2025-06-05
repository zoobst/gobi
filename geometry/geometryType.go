package geometry

import (
	"fmt"
	"log"

	berrors "github.com/zoobst/gobi/bErrors"
)

type GeometryType struct {
	T      Geometry
	Points []Point
}

func (g GeometryType) Type() string { return g.T.Type() }

func (g GeometryType) Len() int {
	if g.Points == nil {
		return 0
	}
	return len(g.Points)
}

func (g GeometryType) Equal(other Geometry) bool {
	if other.Len() != g.Len() {
		return false
	}
	switch g.T.(type) {
	case Point:
		if _, ok := other.(Point); !ok {
			return false
		} else {
			if !g.Points[0].Equal(other.(Point)) {
				return false
			}
		}
	case Polygon:
		if _, ok := other.(Polygon); !ok {
			return false
		} else {
			for i := range g.Len() {
				if !g.Points[i].Equal(other.(Polygon).Points[i]) {
					return false
				}
			}
		}
	case LineString:
		if _, ok := other.(LineString); !ok {
			return false
		} else {
			for i := range g.Len() {
				if !g.Points[i].Equal(other.(LineString).Points[i]) {
					return false
				}
			}
		}
	}
	return true
}

func (g GeometryType) ToCRS(epsg int) Geometry {
	newP := Point{
		CoordRefSys: CRSbyEPSG[epsg],
	}
	var newPointList []Point
	for _, p := range g.Points {
		if g.CRS().Projected && newP.CRS().Projected {
			newP.X, newP.Y = p.X, p.Y
		} else if p.CRS().Projected && !newP.CRS().Projected {
			newP.X, newP.Y = MercatorToLL(p.X, p.Y)
		} else if !p.CRS().Projected && newP.CRS().Projected {
			newP.X, newP.Y = LLToMercator(p.X, p.Y)
		}
		newPointList = append(newPointList, newP)
	}
	g.Points = newPointList
	return g
}

func (g GeometryType) EstimateUTMCRS() CRS {
	epsg := estimateUTMEPSG(g)
	return CRSbyEPSG[epsg]
}

func (g GeometryType) Bounds() Box {
	return Box{g.MinX(), g.MinY(), g.MaxX(), g.MaxY()}
}

func (g GeometryType) String() (strList string) {
	switch g.T.(type) {
	case Polygon:
		newPoly := Polygon{Points: g.Points}
		return newPoly.String()
	case Point:
		return g.Points[0].String()
	case LineString:
		newLS := LineString{Points: g.Points}
		return newLS.String()
	default:
		log.Fatal(berrors.ErrInvalidGeometryType)
	}
	return ""
}

func (g GeometryType) CRS() CRS { return g.Points[0].CoordRefSys }

func (g GeometryType) WKT() string {
	switch g.T.(type) {
	case Polygon:
		newPoly := Polygon{Points: g.Points}
		return newPoly.WKT()
	case Point:
		return g.Points[0].WKT()
	case LineString:
		newLS := LineString{Points: g.Points}
		return newLS.WKT()
	default:
		log.Fatal(fmt.Errorf(berrors.ErrInvalidGeometryType.Error(), g.T))
	}
	return ""
}

func (g GeometryType) WKB() []byte {
	switch t := g.T.(type) {
	case Polygon:
		newPoly := Polygon{Points: g.Points}
		return newPoly.WKB()
	case Point:
		return g.Points[0].WKB()
	case LineString:
		newLS := LineString{Points: g.Points}
		return newLS.WKB()
	case GeometryType:
		log.Println("uh oh")
	default:
		log.Fatal(fmt.Errorf(berrors.ErrInvalidGeometryType.Error(), t))
	}
	return nil
}

// WKBHex returns the WKB encoding of the Point as a hex string.
func (g GeometryType) WKBHex() (string, error) {
	switch g.T.(type) {
	case Polygon:
		newPoly := Polygon{Points: g.Points}
		return newPoly.WKBHex()
	case Point:
		return g.Points[0].WKBHex()
	case LineString:
		newLS := LineString{Points: g.Points}
		return newLS.WKBHex()
	default:
		return "", berrors.ErrInvalidGeometryType
	}
}

func (g GeometryType) Coords() (fList [][2]float64) {
	switch g.T.(type) {
	case Polygon:
		newPoly := Polygon{Points: g.Points}
		return newPoly.Coords()
	case Point:
		return g.Points[0].Coords()
	case LineString:
		newLS := LineString{Points: g.Points}
		return newLS.Coords()
	default:
		log.Fatal(berrors.ErrInvalidGeometryType)
	}
	return nil
}

func (gt GeometryType) MarshalJSON() ([]byte, error) {
	return gt.T.MarshalJSON()
}

func (gt GeometryType) UnmarshalJSON(data []byte) error {
	return gt.T.UnmarshalJSON(data)
}

func (g GeometryType) MinX() float64 { return minX(&g.Points) }

func (g GeometryType) MinY() float64 { return minY(&g.Points) }

func (g GeometryType) MaxX() float64 { return maxX(&g.Points) }

func (g GeometryType) MaxY() float64 { return maxY(&g.Points) }

func (g GeometryType) checkDegrees() bool {
	if g.Points[0].X >= -90.0 && g.Points[0].X <= 90.0 && g.Points[0].Y >= -180.0 && g.Points[0].Y <= 180.0 {
		return true
	}
	return false
}

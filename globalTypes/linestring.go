package globalTypes

import (
	"fmt"

	"github.com/zoobst/gobi/geojson"
)

func (l LineString) String() (strList string) {
	if len(l.Points) == 0 {
		return
	}
	for _, point := range l.Points {
		strList += ", " + point.String()
	}
	return strList[2:]
}

func (l LineString) Type() string {
	return "LineString"
}

func (l LineString) WKT() (strList string) {
	strList = "LINESTRING ("
	for _, point := range l.Points {
		strList += fmt.Sprintf("(%f %f),", point.X, point.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (l LineString) Coords() (fList [][2]float64) {
	for _, point := range l.Points {
		fList = append(fList, [2]float64{point.X, point.Y})
	}
	return fList
}

func (l LineString) MaxX() float64 {
	return maxX(&l.Points)
}

func (l LineString) MaxY() float64 {
	return maxY(&l.Points)
}

func (l LineString) MinX() float64 {
	return minX(&l.Points)
}

func (l LineString) MinY() float64 {
	return minY(&l.Points)
}

// GetGeometry returns the GeoJSON geometry representation of the geometry.
func (l LineString) GeoJSONGeometry() geojson.GeoJSONGeometry {
	return geojson.GeoJSONGeometry{
		Type:        "LineString",
		Coordinates: [][][2]float64{l.Coords()},
	}
}

func (l LineString) length(units string) float64 {
	return 0.0
}

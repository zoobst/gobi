package globalTypes

import (
	"fmt"
	"hash/fnv"

	"github.com/zoobst/gobi/geojson"

	"github.com/apache/arrow/go/arrow"
)

func (p Polygon) String() (strList string) {
	if len(p.Points) == 0 {
		return
	}
	for _, point := range p.Points {
		strList += ", " + point.String()
	}
	return strList[2:]
}

func (p Polygon) Type() string {
	return "Polygon"
}

func (p *Polygon) ID() arrow.Type {
	return arrow.EXTENSION
}

func (p *Polygon) Name() string {
	return "Polygon"
}

func (p *Polygon) StorageType(dt arrow.DataType) arrow.DataType {
	return arrow.ListOf(arrow.ListOf(arrow.PrimitiveTypes.Float64)) // Storage as list of list of floats (x,y)
}

func (p *Polygon) Fingerprint() string {
	// Implement the Fingerprint method.

	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p *Polygon) Equal(other arrow.DataType) bool {
	//Compare the fingerprints.
	if other, ok := other.(*Polygon); ok {
		return p.Fingerprint() == other.Fingerprint()
	}
	return false
}

func (p *Polygon) Serialize() string {
	return p.Name()
}

func (p *Polygon) Deserialize(s string) arrow.DataType {
	return &Polygon{}
}

func (p *Polygon) ExtensionName() string {
	return p.Name()
}

func (p *Polygon) ExtensionMetadata() string {
	return ""
}

func (p Polygon) WKT() (strList string) {
	strList = "POLYGON ("
	for _, point := range p.Points {
		strList += fmt.Sprintf("(%f %f),", point.X, point.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (p Polygon) Coords() (fList [][2]float64) {
	for _, point := range p.Points {
		fList = append(fList, [2]float64{point.X, point.Y})
	}
	return fList
}

func (p Polygon) MaxX() float64 {
	return maxX(&p.Points)
}

func (p Polygon) MaxY() float64 {
	return maxY(&p.Points)
}

func (p Polygon) MinX() (lVal float64) {
	return minX(&p.Points)
}

func (p Polygon) MinY() float64 {
	return minY(&p.Points)
}

// GetGeometry returns the GeoJSON geometry representation of the geometry.
func (p Polygon) GeoJSONGeometry() geojson.GeoJSONGeometry {
	return geojson.GeoJSONGeometry{
		Type:        "Polygon",
		Coordinates: [][][2]float64{p.Coords()},
	}
}

func (p Polygon) area(units string) {
}

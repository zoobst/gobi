package globalTypes

import (
	"fmt"
	"hash/fnv"

	"github.com/zoobst/gobi/geojson"

	"github.com/apache/arrow/go/arrow"
	"github.com/apache/arrow/go/arrow/array"
)

func (p Point) String() string { return fmt.Sprintf("X: %f, Y: %f", p.X, p.Y) }

func (p *Point) ID() arrow.Type { return arrow.EXTENSION }

func (p *Point) Name() string { return "Point" }

func (p *Point) StorageType(dt arrow.DataType) arrow.DataType {
	return arrow.ListOf(arrow.PrimitiveTypes.Float64) // Storage as list of floats (x,y)
}

func (p *Point) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p *Point) Equal(other arrow.DataType) bool {
	//Compare the fingerprints.
	if other, ok := other.(*Point); ok {
		return p.Fingerprint() == other.Fingerprint()
	}
	return false
}

func (p *Point) Serialize() string { return p.String() }

func (p *Point) Deserialize(s string) arrow.DataType { return &Point{} }

func (p *Point) ExtensionName() string { return p.Name() }

func (p *Point) ExtensionMetadata() string { return "" }

func (p Point) WKT() string { return fmt.Sprintf("POINT (%f %f)", p.X, p.Y) }

func (p Point) Coords() (fList [][2]float64) {
	fList = [][2]float64{{p.X, p.Y}}
	return fList
}

func (p Point) MaxX() float64 { return p.X }

func (p Point) MaxY() float64 { return p.Y }

func (p Point) MinX() float64 { return p.X }

func (p Point) MinY() float64 { return p.Y }

// GetGeometry returns the GeoJSON geometry representation of the geometry.
func (p Point) GeoJSONGeometry() geojson.GeoJSONGeometry {
	return geojson.GeoJSONGeometry{
		Type:        "Point",
		Coordinates: [][][2]float64{p.Coords()},
	}
}

type PointArray struct {
	array.ExtensionArray
	listArray *array.List
}

func (p *Point) NewArray(data array.Data) array.ExtensionArray {
	return &PointArray{
		listArray: array.NewListData(&data),
	}
}

func (a *PointArray) DataType() arrow.DataType { return &Point{} }

func (a *PointArray) Data() *array.Data { return a.listArray.Data() }

func (a *PointArray) String() string { return fmt.Sprintf("%v", a.listArray) }

func (a *PointArray) GetPoint(i int) []float64 {
	listArray := a.listArray.ListValues()
	return listArray.(*array.Float64).Float64Values()[a.listArray.Offsets()[i]*2 : a.listArray.Offsets()[i]*2+2]
}

func (a *PointArray) ListValues() *array.List { return a.listArray }

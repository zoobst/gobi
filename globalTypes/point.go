package globalTypes

import (
	"fmt"
	"hash/fnv"

	"github.com/zoobst/gobi/geojson"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
)

func (p Point) String() string { return fmt.Sprintf("%f %f", p.X, p.Y) }

func (p Point) ID() arrow.Type { return arrow.EXTENSION }

func (p Point) Type() string { return "Point" }

func (p Point) Name() string { return "Point" }

func (p Point) StorageType(dt arrow.DataType) arrow.DataType {
	return arrow.ListOf(arrow.PrimitiveTypes.Float64) // Storage as list of floats (x,y)
}

func (p Point) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p Point) Equal(other arrow.DataType) bool {
	//Compare the fingerprints.
	if other, ok := other.(*Point); ok {
		return p.Fingerprint() == other.Fingerprint()
	}
	return false
}

func (p Point) Serialize() string { return p.String() }

func (p Point) Deserialize() arrow.DataType {
	return &Point{}
}

func (p Point) ExtensionName() string { return p.Name() }

func (p Point) ExtensionMetadata() string { return "" }

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

func (p Point) NewArray(data array.Data) array.ExtensionArray {
	return &PointArray{
		listArray: array.NewListData(&data),
	}
}

func (a PointArray) DataType() arrow.DataType { return &Point{} }

func (a PointArray) Data() arrow.ArrayData { return a.listArray.Data() }

func (a PointArray) String() string { return fmt.Sprintf("%v", a.listArray) }

func (a PointArray) Iloc(i int) []float64 {
	listArray := a.listArray.ListValues()
	return listArray.(*array.Float64).Float64Values()[a.listArray.Offsets()[i]*2 : a.listArray.Offsets()[i]*2+2]
}

func (a PointArray) ListValues() *array.List { return a.listArray }

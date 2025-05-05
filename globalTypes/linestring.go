package globalTypes

import (
	"fmt"
	"hash/fnv"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/zoobst/gobi/geojson"
)

func (l LineString) String() (strList string) {
	if len(l.Points) == 0 {
		return
	}
	for _, LineString := range l.Points {
		strList += ", " + LineString.String()
	}
	return strList[2:]
}

func (p LineString) ID() arrow.Type { return arrow.EXTENSION }

func (p LineString) Type() string { return "LineString" }

func (p LineString) Name() string { return "LineString" }

func (p LineString) StorageType(dt arrow.DataType) arrow.DataType {
	return arrow.ListOf(arrow.PrimitiveTypes.Float64) // Storage as list of floats (x,y)
}

func (p LineString) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p LineString) Equal(other arrow.DataType) bool {
	//Compare the fingerprints.
	if other, ok := other.(*LineString); ok {
		return p.Fingerprint() == other.Fingerprint()
	}
	return false
}

func (p LineString) Serialize() string { return p.String() }

func (p LineString) Deserialize() arrow.DataType {
	return &LineString{}
}

func (p LineString) ExtensionName() string { return p.Name() }

func (p LineString) ExtensionMetadata() string { return "" }

// GetGeometry returns the GeoJSON geometry representation of the geometry.
func (l LineString) GeoJSONGeometry() geojson.GeoJSONGeometry {
	return geojson.GeoJSONGeometry{
		Type:        "LineString",
		Coordinates: [][][2]float64{l.Coords()},
	}
}

type LineStringArray struct {
	array.ExtensionArray
	listArray *array.List
}

func (p LineString) NewArray(data array.Data) array.ExtensionArray {
	return &LineStringArray{
		listArray: array.NewListData(&data),
	}
}

func (a LineStringArray) DataType() arrow.DataType { return &LineString{} }

func (a LineStringArray) Data() arrow.ArrayData { return a.listArray.Data() }

func (a LineStringArray) String() string { return fmt.Sprintf("%v", a.listArray) }

func (a LineStringArray) Iloc(i int) []float64 {
	listArray := a.listArray.ListValues()
	return listArray.(*array.Float64).Float64Values()[a.listArray.Offsets()[i]*2 : a.listArray.Offsets()[i]*2+2]
}

func (a LineStringArray) ListValues() *array.List { return a.listArray }

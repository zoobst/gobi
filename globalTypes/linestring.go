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

func (l LineString) WKT() (strList string) {
	strList = "LINESTRING ("
	for _, LineString := range l.Points {
		strList += fmt.Sprintf("(%f %f),", LineString.X, LineString.Y)
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (l LineString) Coords() (fList [][2]float64) {
	for _, LineString := range l.Points {
		fList = append(fList, [2]float64{LineString.X, LineString.Y})
	}
	return fList
}

func (l LineString) MaxX() float64 { return maxX(&l.Points) }

func (l LineString) MaxY() float64 { return maxY(&l.Points) }

func (l LineString) MinX() float64 { return minX(&l.Points) }

func (l LineString) MinY() float64 { return minY(&l.Points) }

// GetGeometry returns the GeoJSON geometry representation of the geometry.
func (l LineString) GeoJSONGeometry() geojson.GeoJSONGeometry {
	return geojson.GeoJSONGeometry{
		Type:        "LineString",
		Coordinates: [][][2]float64{l.Coords()},
	}
}

func (l LineString) length(units string) float64 { return 0.0 }

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

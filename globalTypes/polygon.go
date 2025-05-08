package globalTypes

import (
	"fmt"
	"hash/fnv"
	"reflect"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
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

func (p Polygon) Type() string { return "Polygon" }

func (p Polygon) ID() arrow.Type { return arrow.EXTENSION }

func (p Polygon) Name() string { return "Polygon" }

func (p Polygon) StorageType() arrow.DataType {
	return arrow.ListOf(arrow.ListOf(arrow.PrimitiveTypes.Float64)) // Storage as list of list of floats ((x,y), (x,y))
}

func (p Polygon) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p Polygon) Serialize() string { return p.String() }

func (p Polygon) Deserialize(arrow.DataType, string) (arrow.ExtensionType, error) {
	return Polygon{}, nil
}

func (p Polygon) ExtensionName() string { return p.Name() }

func (p Polygon) ExtensionMetadata() string { return "" }

func (p Polygon) ExtensionEquals(other arrow.ExtensionType) bool {
	switch t := other.(type) {
	case Geometry:
		return p.Equal(t)
	default:
		return false
	}
}

func (p Polygon) Layout() arrow.DataTypeLayout {
	return arrow.DataTypeLayout{
		Buffers: []arrow.BufferSpec{
			{Kind: arrow.KindBitmap},   // validity
			{Kind: arrow.KindVarWidth}, // offsets
		},
		HasDict: false,
	}
}

func (p Polygon) ArrayType() reflect.Type {
	return reflect.TypeOf(PolygonArray{})
}

type PolygonArray struct {
	array.ExtensionArray
	listArray *array.List
}

func (p Polygon) NewArray(data array.Data) array.ExtensionArray {
	return &PolygonArray{
		listArray: array.NewListData(&data),
	}
}

func (a PolygonArray) DataType() arrow.DataType { return &Point{} }

func (a PolygonArray) Data() arrow.ArrayData { return a.listArray.Data() }

func (a PolygonArray) String() string { return fmt.Sprintf("%v", a.listArray) }

func (a PolygonArray) Iloc(i int) []float64 {
	listArray := a.listArray.ListValues()
	return listArray.(*array.Float64).Float64Values()[a.listArray.Offsets()[i]*2 : a.listArray.Offsets()[i]*2+2]
}

func (a PolygonArray) ListValues() *array.List { return a.listArray }

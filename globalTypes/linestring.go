package globalTypes

import (
	"fmt"
	"hash/fnv"
	"reflect"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
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

func (LineString) ID() arrow.Type { return arrow.EXTENSION }

func (LineString) Type() string { return "Geometry" }

func (LineString) Name() string { return "LineString" }

func (LineString) StorageType() arrow.DataType {
	return arrow.ListOf(arrow.ListOf(arrow.PrimitiveTypes.Float64)) // Storage as list of list of floats ((x,y), (x,y))
}

func (l LineString) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(l.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (l LineString) Serialize() string { return l.String() }

func (LineString) Deserialize(arrow.DataType, string) (arrow.ExtensionType, error) {
	return LineString{}, nil
}

func (l LineString) ExtensionName() string { return l.Name() }

func (LineString) ExtensionMetadata() string { return "" }

func (l LineString) ExtensionEquals(other arrow.ExtensionType) bool {
	switch t := other.(type) {
	case Geometry:
		return l.Equal(t)
	default:
		return false
	}
}

func (LineString) Layout() arrow.DataTypeLayout {
	return arrow.DataTypeLayout{
		Buffers: []arrow.BufferSpec{
			{Kind: arrow.KindBitmap},   // validity
			{Kind: arrow.KindVarWidth}, // offsets
		},
		HasDict: false,
	}
}

func (LineString) ArrayType() reflect.Type {
	return reflect.TypeOf(LineStringArray{})
}

type LineStringArray struct {
	array.ExtensionArray
	listArray *array.List
}

func (LineString) NewArray(data array.Data) array.ExtensionArray {
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

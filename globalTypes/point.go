package globalTypes

import (
	"fmt"
	"hash/fnv"
	"reflect"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
)

func (p Point) String() string { return fmt.Sprintf("%f %f", p.X, p.Y) }

func (p Point) ID() arrow.Type { return arrow.EXTENSION }

func (p Point) Type() string { return "Point" }

func (p Point) Name() string { return "Point" }

func (p Point) StorageType() arrow.DataType {
	return arrow.ListOf(arrow.PrimitiveTypes.Float64) // Storage as list of floats (x,y)
}

func (p Point) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p Point) Serialize() string { return p.String() }

func (p Point) Deserialize(arrow.DataType, string) (arrow.ExtensionType, error) {
	return Point{}, nil
}

func (p Point) ExtensionName() string { return p.Name() }

func (p Point) ExtensionMetadata() string { return "" }

func (p Point) ExtensionEquals(other arrow.ExtensionType) bool {
	switch t := other.(type) {
	case Geometry:
		return p.Equal(t)
	default:
		return false
	}
}

func (p Point) Layout() arrow.DataTypeLayout {
	return arrow.DataTypeLayout{
		Buffers: []arrow.BufferSpec{
			{
				Kind:      arrow.KindBitmap, // Null bitmap (1 bit per value)
				ByteWidth: 0,                // Arrow handles bitmaps internally
			},
			{
				Kind:      arrow.KindFixedWidth, // Data buffer
				ByteWidth: 16,                   // 2 * float64 = 2 * 8 = 16 bytes
			},
		},
		HasDict: false,
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

func (p Point) ArrayType() reflect.Type {
	return reflect.TypeOf(PointArray{})
}

func (a PointArray) DataType() arrow.DataType { return &Point{} }

func (a PointArray) Data() arrow.ArrayData { return a.listArray.Data() }

func (a PointArray) String() string { return fmt.Sprintf("%v", a.listArray) }

func (a PointArray) Iloc(i int) []float64 {
	listArray := a.listArray.ListValues()
	return listArray.(*array.Float64).Float64Values()[a.listArray.Offsets()[i]*2 : a.listArray.Offsets()[i]*2+2]
}

func (a PointArray) ListValues() *array.List { return a.listArray }

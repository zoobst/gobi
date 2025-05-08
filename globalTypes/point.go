package globalTypes

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/zoobst/gobi/geometry"
)

func NewPointFromGeometry(g *geometry.Point) (p Point) {
	p = Point{*g, arrow.ExtensionBase{Storage: p.StorageType()}}
	return
}

func (p Point) String() string { return fmt.Sprintf("%f %f", p.X, p.Y) }

func (p Point) ID() arrow.Type { return arrow.EXTENSION }

func (p Point) Type() string { return "Geometry" }

func (p Point) Name() string { return "Point" }

func (p Point) Field() arrow.Field {
	return arrow.Field{
		Name:     p.Name(),
		Type:     p.StorageType(),
		Nullable: true,
		Metadata: arrow.Metadata{},
	}
}

func (p Point) StorageType() arrow.DataType {
	return arrow.BinaryTypes.LargeBinary
}

func (p Point) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p Point) Serialize() string { return p.String() }

func (p Point) Deserialize(storage arrow.DataType, data string) (arrow.ExtensionType, error) {
	return p, nil
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
	Point
	array.ExtensionArrayBase
}

func (p Point) NewArray(data array.Data) array.ExtensionArray {
	return &PointArray{}
}

func (Point) ArrayType() reflect.Type {
	return reflect.TypeOf(&PointArray{}).Elem() // ← This is correct
}

func (p *PointArray) Len() int {
	return p.Storage().Len()
}

func (p *PointArray) NullN() int {
	return p.Storage().NullN()
}

func (p *PointArray) IsNull(i int) bool {
	return p.Storage().IsNull(i)
}

func (p *PointArray) IsValid(i int) bool {
	return !p.Storage().IsNull(i)
}

// 5. ValueStr() – Get the value at index i as a string (custom implementation)
func (p *PointArray) ValueStr(i int) string {
	if p.IsNull(i) {
		return "null"
	}
	return string(p.Storage().(*array.Binary).Value(i))
}

func (pa *PointArray) String() string {
	var b strings.Builder
	b.WriteString("[")
	for i := range pa.Storage().Len() {
		if pa.IsNull(i) {
			b.WriteString("null")
		} else {
			b.WriteString(pa.ValueStr(i))
		}
		if i != pa.Storage().Len()-1 {
			b.WriteString(", ")
		}
	}
	b.WriteString("]")
	return b.String()
}

// 6. GetOneForMarshal() – Needed for marshaling the extension array
func (pa *PointArray) GetOneForMarshal(i int) interface{} {
	return pa.Storage().(*array.Binary).Value(i)
}

// 7. MarshalJSON() – Marshal the array to JSON
func (pa *PointArray) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString("[")
	for i := range pa.Storage().Len() {
		if pa.IsNull(i) {
			b.WriteString("null")
		} else {
			b.WriteString(`"`)
			b.WriteString(pa.GetOneForMarshal(i).(string)) // Assuming it's a string (e.g. WKT)
			b.WriteString(`"`)
		}
		if i != pa.Storage().Len()-1 {
			b.WriteString(", ")
		}
	}
	b.WriteString("]")
	return []byte(b.String()), nil
}

// 8. Offset() – The offset of this array in memory (usually 0)
func (a *PointArray) Offset() int {
	return 0
}

func (a *PointArray) Data() arrow.ArrayData {
	return a.Storage().Data()
}

// 12. DataType() – The Arrow data type for this array (e.g., binary)
func (a *PointArray) DataType() arrow.DataType {
	return a.Storage().DataType()
}

// 13. ExtensionType() – Get the extension type for this array
func (*PointArray) ExtensionType() arrow.ExtensionType {
	return Point{}
}

// 14. NullBitmapBytes() – Get the bitmap of null values
func (pa *PointArray) NullBitmapBytes() []byte {
	return pa.Storage().NullBitmapBytes()
}

// 15. Release() – Release memory for the array
func (pa *PointArray) Release() {
	pa.Storage().Release()
}

// 16. Retain() – Retain a reference to the array (increase reference count)
func (pa *PointArray) Retain() {
	pa.Storage().Retain()
}

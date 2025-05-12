package globalTypes

import (
	"fmt"
	"hash/fnv"
	"log"
	"reflect"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/zoobst/gobi/geometry"
)

func NewPolygonFromGeometry(g *geometry.Polygon) (p Polygon) {
	p = Polygon{*g, arrow.ExtensionBase{Storage: p.StorageType()}}
	return
}

func (p Polygon) String() (strList string) {
	if len(p.Points) == 0 {
		return
	}
	for _, point := range p.Points {
		strList += ", " + point.String()
	}
	return strList[2:]
}

func (p Polygon) Type() string { return "Geometry" }

func (p Polygon) ID() arrow.Type { return arrow.EXTENSION }

func (p Polygon) Name() string { return "Polygon" }

func (p Polygon) StorageType() arrow.DataType {
	return arrow.BinaryTypes.Binary
}

func (p Polygon) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(p.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (p Polygon) Serialize() string { return p.String() }

func (p Polygon) Deserialize(storage arrow.DataType, _ string) (arrow.ExtensionType, error) {
	if storage.ID() != arrow.BINARY {
		return nil, fmt.Errorf("invalid storage type for Polygon: %s", storage.Name())
	}
	return Polygon{}, nil
}

func (p Polygon) ExtensionName() string { return p.Name() }

func (p Polygon) ExtensionMetadata() string { return "type:geometry" }

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
			{Kind: arrow.KindBitmap}, // null bitmap
		},
		HasDict: false,
	}
}

func (Polygon) ArrayType() reflect.Type {
	return reflect.TypeOf(&GeometryArray{}).Elem()
}

type PolygonArray struct {
	Polygon
	array.ExtensionArrayBase
}

func (p Polygon) NewArray(data array.Data) array.ExtensionArray {
	return &GeometryArray{}
}

func (p *PolygonArray) Len() int {
	return p.Storage().Len()
}

func (p *PolygonArray) NullN() int {
	return p.Storage().NullN()
}

func (p *PolygonArray) IsNull(i int) bool {
	return p.Storage().IsNull(i)
}

func (p *PolygonArray) IsValid(i int) bool {
	return !p.Storage().IsNull(i)
}

func (p *PolygonArray) Value(i int) geometry.Polygon {
	if p.IsNull(i) {
		return geometry.Polygon{}
	}
	poly, err := p.FromWKB(p.Storage().(*array.Binary).Value(i))
	if err != nil {
		log.Fatal(err)
	}
	return poly
}

func (p *PolygonArray) ValueStr(i int) string {
	if p.IsNull(i) {
		return "null"
	}
	poly, err := p.FromWKB(p.Storage().(*array.Binary).Value(i))
	if err != nil {
		log.Fatal(err)
	}
	return poly.String()
}

func (pa *PolygonArray) String() string {
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

func (pa *PolygonArray) GetOneForMarshal(i int) any {
	return string(pa.Storage().(*array.Binary).Value(i))
}

func (pa *PolygonArray) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString("[")
	for i := range pa.Storage().Len() {
		if pa.IsNull(i) {
			b.WriteString("null")
		} else {
			b.WriteString(`"`)
			if val, ok := pa.GetOneForMarshal(i).(string); ok {
				b.WriteString(`"` + val + `"`)
			} else {
				b.WriteString("null")
			}

			b.WriteString(`"`)
		}
		if i != pa.Storage().Len()-1 {
			b.WriteString(", ")
		}
	}
	b.WriteString("]")
	return []byte(b.String()), nil
}

func (a *PolygonArray) Offset() int {
	return 0
}

func (a *PolygonArray) Data() arrow.ArrayData {
	return a.Storage().Data()
}

func (a *PolygonArray) DataType() arrow.DataType {
	return Polygon{}
}

func (*PolygonArray) ExtensionType() arrow.ExtensionType {
	return Polygon{}
}

func (pa *PolygonArray) NullBitmapBytes() []byte {
	return pa.Storage().NullBitmapBytes()
}

func (pa *PolygonArray) Release() {
	pa.Storage().Release()
}

func (pa *PolygonArray) Retain() {
	pa.Storage().Retain()
}

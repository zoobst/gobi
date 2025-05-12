package globalTypes

import (
	"log"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/geometry"
)

type GeometryArray struct {
	array.ExtensionArrayBase
}

func (g *GeometryArray) FromWKB(b []byte) (Geometry, error) {
	var defaultExtensionBase = arrow.ExtensionBase{
		Storage: arrow.BinaryTypes.Binary,
	}
	t, err := geometry.ParseWKB(b)
	if err != nil {
		return nil, err
	}
	if val, ok := t.(geometry.Point); ok {
		return Point{val, defaultExtensionBase}, nil
	} else if val, ok := t.(geometry.Polygon); ok {
		return Polygon{val, defaultExtensionBase}, nil
	} else if val, ok := t.(geometry.LineString); ok {
		return LineString{val, defaultExtensionBase}, nil
	}
	return nil, berrors.ErrInvalidType
}

func (g *GeometryArray) Len() int {
	return g.Storage().Len()
}

func (g *GeometryArray) NullN() int {
	return g.Storage().NullN()
}

func (g *GeometryArray) IsNull(i int) bool {
	return g.Storage().IsNull(i)
}

func (g *GeometryArray) IsValid(i int) bool {
	return !g.Storage().IsNull(i)
}

func (ga *GeometryArray) Value(i int) Geometry {
	g, err := ga.FromWKB(ga.Storage().(*array.Binary).Value(i))
	if err != nil {
		log.Fatal(err)
	}
	return g
}

func (ga *GeometryArray) Storage() arrow.Array {
	return &array.Binary{}
}

func (g *GeometryArray) ValueStr(i int) string {
	if g.IsNull(i) {
		return "null"
	}
	poly, err := g.FromWKB(g.Storage().(*array.Binary).Value(i))
	if err != nil {
		log.Fatal(err)
	}
	return poly.String()
}

func (ga *GeometryArray) String() string {
	var b strings.Builder
	b.WriteString("[")
	for i := range ga.Storage().Len() {
		if ga.IsNull(i) {
			b.WriteString("null")
		} else {
			b.WriteString(ga.ValueStr(i))
		}
		if i != ga.Storage().Len()-1 {
			b.WriteString(", ")
		}
	}
	b.WriteString("]")
	return b.String()
}

func (ga *GeometryArray) GetOneForMarshal(i int) any {
	if val, err := ga.FromWKB(ga.Storage().(*array.Binary).Value(i)); err == nil {
		return val.String()
	} else {
		return err
	}
}

func (ga *GeometryArray) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	for i := range ga.Storage().Len() {
		if ga.IsNull(i) {
			b.WriteString("null")
		} else {
			b.WriteString(`"`)
			if val, ok := ga.GetOneForMarshal(i).(string); ok {
				b.WriteString(`"` + val + `"`)
			} else {
				b.WriteString("null")
			}

			b.WriteString(`"`)
		}
		if i != ga.Storage().Len()-1 {
			b.WriteString(", ")
		}
	}
	return []byte(b.String()), nil
}

func (*GeometryArray) Offset() int { return 0 }

func (ga *GeometryArray) Data() arrow.ArrayData {
	return ga.Storage().Data()
}

func (ga *GeometryArray) DataType() arrow.DataType {
	return ga.Storage().DataType()
}

func (ga *GeometryArray) ExtensionType() arrow.ExtensionType {
	return &GeometryType{}
}

func (ga *GeometryArray) NullBitmapBytes() []byte {
	return ga.Storage().NullBitmapBytes()
}

func (ga *GeometryArray) Release() {
	ga.Storage().Release()
}

func (ga *GeometryArray) Retain() {
	ga.Storage().Retain()
}

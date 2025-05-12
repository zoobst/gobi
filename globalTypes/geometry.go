package globalTypes

import (
	"fmt"
	"hash/fnv"
	"log"
	"reflect"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/geometry"
)

var defaultExtensionBase arrow.ExtensionBase = arrow.ExtensionBase{
	Storage: arrow.BinaryTypes.Binary,
}

type GeometryType struct {
	geometry.GeometryType
	arrow.ExtensionBase
}

func NewGeometryTypeFromGeometry(g Geometry) *GeometryType {
	switch t := g.(type) {
	case Point:
		return &GeometryType{geometry.GeometryType{
			T:      Point{},
			Points: []geometry.Point{t.Point},
		},
			defaultExtensionBase,
		}
	case Polygon:
		return &GeometryType{geometry.GeometryType{
			T:      Polygon{},
			Points: t.Points,
		},
			defaultExtensionBase,
		}
	case LineString:
		return &GeometryType{geometry.GeometryType{
			T:      LineString{},
			Points: t.Points,
		},
			defaultExtensionBase,
		}
	case *GeometryType:
		return t
	default:
		return t.(*GeometryType)
	}
}

func (g *GeometryType) ExtensionName() string {
	return "Geometry"
}

func (g GeometryType) Serialize() string {
	return "" // or custom metadata
}

func (g GeometryType) Deserialize(storage arrow.DataType, _ string) (arrow.ExtensionType, error) {
	return &GeometryType{
		ExtensionBase: arrow.ExtensionBase{Storage: storage},
	}, nil
}

func (g GeometryType) WKB() []byte {
	switch t := g.T.(type) {
	case Polygon:
		return Polygon{geometry.Polygon{Points: t.Points}, defaultExtensionBase}.WKB()
	case LineString:
		return LineString{geometry.LineString{Points: t.Points}, defaultExtensionBase}.WKB()
	case Point:
		return Point{geometry.Point{X: t.X, Y: t.Y, CoordRefSys: t.CoordRefSys}, defaultExtensionBase}.WKB()
	default:
		log.Fatal(fmt.Errorf(berrors.ErrInvalidGeometryType.Error(), t))
	}
	return nil
}

func (g *GeometryType) String() string {
	return fmt.Sprintf("extension<%s>", g.ExtensionName())
}

func (GeometryType) Type() string { return "Geometry" }

func (GeometryType) Name() string { return "Geometry" }

func (GeometryType) StorageType() arrow.DataType {
	return arrow.BinaryTypes.Binary
}

func (g GeometryType) Fingerprint() string {
	h := fnv.New64a()
	h.Write([]byte(g.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (g GeometryType) ExtensionMetadata() string { return "type:geometry" }

func (g GeometryType) ExtensionEquals(other arrow.ExtensionType) bool {
	switch t := other.(type) {
	case Geometry:
		return g.T.Equal(t)
	default:
		return false
	}
}

func (g GeometryType) Layout() arrow.DataTypeLayout {
	return arrow.DataTypeLayout{
		Buffers: []arrow.BufferSpec{
			{Kind: arrow.KindBitmap}, // null bitmap
		},
		HasDict: false,
	}
}

func (GeometryType) ArrayType() reflect.Type {
	return reflect.TypeOf(&GeometryArray{}).Elem()
}

func (g GeometryType) NewArray(arr array.Data) array.ExtensionArray {
	// Create the storage array from the data
	storageArray := array.MakeFromData(&arr)

	// Use the correct function to create the extension array
	extArray := array.NewExtensionArrayWithStorage(arrow.GetExtensionType(g.Type()), storageArray)

	// Cast to GeometryArray and return
	return extArray.(array.ExtensionArray)
}

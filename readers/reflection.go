package readers

import (
	"reflect"

	"github.com/apache/arrow/go/v18/arrow"
)

var GoPrimitivesToArrowPrimitivesMap = map[reflect.Kind]arrow.DataType{
	reflect.Float64:   arrow.PrimitiveTypes.Float64,
	reflect.Bool:      arrow.FixedWidthTypes.Boolean,
	reflect.Int64:     arrow.PrimitiveTypes.Int64,
	reflect.Int:       arrow.PrimitiveTypes.Int64,
	reflect.String:    arrow.BinaryTypes.String,
	reflect.Interface: arrow.BinaryTypes.String,
	reflect.Struct:    &arrow.ExtensionBase{},
}

var GoStringToArrowPrimitivesMap = map[string]arrow.DataType{
	"float64": arrow.PrimitiveTypes.Float64,
	"float32": arrow.PrimitiveTypes.Float32,
	"float":   arrow.PrimitiveTypes.Float64,

	"point":      arrow.GetExtensionType("Geometry"),
	"linestring": arrow.GetExtensionType("Geometry"),
	"polygon":    arrow.GetExtensionType("Geometry"),
	"geometry":   arrow.GetExtensionType("Geometry"),

	"boolean": arrow.FixedWidthTypes.Boolean,

	"int64": arrow.PrimitiveTypes.Int64,
	"int":   arrow.PrimitiveTypes.Int64,
	"int32": arrow.PrimitiveTypes.Int32,
	"int16": arrow.PrimitiveTypes.Int16,

	"string":    arrow.BinaryTypes.String,
	"interface": arrow.BinaryTypes.String,

	"datetime": &arrow.Date64Type{},
	"time":     &arrow.Date32Type{},
}

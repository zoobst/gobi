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
}

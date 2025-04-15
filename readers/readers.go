package readers

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	berrors "github.com/zoobst/gobi/bErrors"
	gTypes "github.com/zoobst/gobi/globalTypes"
)

type Reader interface {
	io.Reader
	handleCompression()
	parseCompressionType()
}

// Helper to map string to arrow.DataType
func ArrowTypeFromString(typeStr string) (arrow.DataType, error) {
	switch strings.ToLower(typeStr) {
	case "datetime":
		return arrow.FixedWidthTypes.Time32ms, nil
	case "geometry":
		return gTypes.GeometryType{}.DataType(), nil
	case "int":
		fallthrough
	case "int64":
		return arrow.PrimitiveTypes.Int64, nil
	case "int32":
		return arrow.PrimitiveTypes.Int32, nil
	case "float":
		fallthrough
	case "float64":
		return arrow.PrimitiveTypes.Float64, nil
	case "float32":
		return arrow.PrimitiveTypes.Float32, nil
	case "string":
		return arrow.BinaryTypes.String, nil
	default:
		return nil, fmt.Errorf(berrors.ErrUnsupportedType.Error(), typeStr)
	}
}

func ArrowTypeFromGo(inType reflect.Type) (arrow.DataType, error) {
	if t, ok := GoPrimitivesToArrowPrimitivesMap[inType.Kind()]; ok {
		return t, nil
	}
	switch inType.Name() {
	case "int":
		return arrow.PrimitiveTypes.Int32, nil
	case "string":
		return arrow.BinaryTypes.String, nil
	case "float64":
		return arrow.PrimitiveTypes.Float64, nil
	case "Time":
		return &arrow.Date64Type{}, nil
	default:
		return nil, fmt.Errorf(berrors.ErrInvalidType.Error(), inType.Name())
	}
}

package readers

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/apache/arrow/go/arrow/memory"
	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
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
		return &arrow.Date64Type{}, nil
	case "point":
		return gTypes.Point{}, nil
	case "polygon":
		return gTypes.Polygon{}, nil
	case "linestring":
		return gTypes.LineString{}, nil
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
		return arrow.PrimitiveTypes.Int64, nil
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

func BuildersFromTypes(dtype arrow.DataType) (builder array.Builder, err error) {
	switch dtype {
	case arrow.PrimitiveTypes.Float64:
		return array.NewFloat64Builder(memory.DefaultAllocator), nil
	case arrow.PrimitiveTypes.Int64:
		return array.NewInt64Builder(memory.DefaultAllocator), nil
	case arrow.FixedWidthTypes.Boolean:
		return array.NewBooleanBuilder(memory.DefaultAllocator), nil
	case arrow.BinaryTypes.String:
		return array.NewStringBuilder(memory.DefaultAllocator), nil
	case arrow.FixedWidthTypes.Date64:
		return array.NewDate64Builder(memory.DefaultAllocator), nil
	case arrow.BinaryTypes.Binary:
		return array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary), nil
	default:
		switch dtype.Name() {
		case "Point":
			fallthrough
		case "Polygon":
			fallthrough
		case "Geometry":
			fallthrough
		case "LineString":
			err = arrow.RegisterExtensionType(gTypes.GenericGeometry())
			if err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			return array.NewExtensionBuilder(memory.DefaultAllocator, &gTypes.GeometryType{}), nil
		default:
			return nil, fmt.Errorf(berrors.ErrInvalidType.Error(), dtype.Name())
		}
	}
}

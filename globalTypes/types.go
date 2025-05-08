package globalTypes

import (
	"fmt"

	"github.com/zoobst/gobi/geometry"

	"github.com/apache/arrow/go/arrow/memory"
	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
)

type DataFrame struct {
	schema   *arrow.Schema
	Series   []Series
	refCount int64
	fmt.Stringer
}

type Series struct {
	Name      string
	Values    *arrow.Column
	Allocator memory.Allocator
}

type HashSet struct {
	data map[any]struct{}
}

type Geometry interface {
	fmt.Stringer
	arrow.DataType
	arrow.ExtensionType
	geometry.Geometry
	NewArray(array.Data) array.ExtensionArray
}

type Point struct {
	geometry.Point
	arrow.ExtensionBase
}

type Polygon struct {
	geometry.Polygon
	arrow.ExtensionBase
}

type LineString struct {
	geometry.LineString
	arrow.ExtensionBase
}

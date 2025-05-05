package globalTypes

import (
	"fmt"

	"github.com/zoobst/gobi/geojson"
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
	geometry.Geometry
	String() string
	Type() string
	WKT() string
	Coords() [][2]float64
	MaxX() float64
	MaxY() float64
	MinX() float64
	MinY() float64
	GeoJSONGeometry() geojson.GeoJSONGeometry
	StorageType(arrow.DataType) arrow.DataType
	ID() arrow.Type
	Name() string
	Fingerprint() string
	Equal(arrow.DataType) bool
	Serialize() string
	Deserialize() arrow.DataType
	ExtensionName() string
	ExtensionMetadata() string
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

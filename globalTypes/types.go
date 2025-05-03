package globalTypes

import (
	"fmt"

	"github.com/zoobst/gobi/geojson"

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
	fmt.Stringer
	arrow.ExtensionBase
	X float64 `json:"lon"`
	Y float64 `json:"lat"`
}

type Polygon struct {
	fmt.Stringer
	arrow.ExtensionBase
	Points []Point `json:"Polygon"`
}

type LineString struct {
	fmt.Stringer
	arrow.ExtensionBase
	Points []Point `json:"LineString"`
}

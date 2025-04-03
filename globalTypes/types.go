package globalTypes

import (
	"time"

	"github.com/zoobst/gobi/geojson"

	"github.com/apache/arrow/go/arrow"
	"github.com/apache/arrow/go/arrow/array"
)

type DataFrame struct {
	Schema *arrow.Schema
	Table  []array.Column
	Series map[string]Series
}

type Series struct {
	Col   array.Column
	Type  GBType
	Index int
}

type GBType interface {
	String() string
}

type String struct {
	Val string
}

type Int struct {
	Val int
}

type Float struct {
	Val float64
}

type DateTime struct {
	Val time.Time
}

type Bool struct {
	Val bool
}

type HashSet struct {
	data map[any]struct{}
}

type Geometry interface {
	String() string
	Type() string
	WKT() string
	Coords() [][2]float64
	MaxX() float64
	MaxY() float64
	MinX() float64
	MinY() float64
	GeoJSONGeometry() geojson.GeoJSONGeometry
	GeoJSONProperties() map[string]any
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
	X float64 `json:"lon"`
	Y float64 `json:"lat"`
}

type Polygon struct {
	Points []Point `json:"Polygon"`
}

type LineString struct {
	Points []Point `json:"LineString"`
}

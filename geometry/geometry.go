package geometry

import "fmt"

// Type identifies a geometry kind.
type Type uint8

const (
	TypeUnknown Type = iota
	TypePoint
	TypeLineString
	TypePolygon
	TypeMultiPoint
	TypeMultiLineString
	TypeMultiPolygon
	TypeGeometryCollection
)

func (t Type) String() string {
	switch t {
	case TypePoint:
		return "Point"
	case TypeLineString:
		return "LineString"
	case TypePolygon:
		return "Polygon"
	case TypeMultiPoint:
		return "MultiPoint"
	case TypeMultiLineString:
		return "MultiLineString"
	case TypeMultiPolygon:
		return "MultiPolygon"
	case TypeGeometryCollection:
		return "GeometryCollection"
	default:
		return "Unknown"
	}
}

// Geometry is the common interface for all geometry primitives.
type Geometry interface {
	// Type returns the concrete geometry type.
	Type() Type
	// CRS returns the coordinate reference system.
	CRS() CRS
	// Bounds returns the axis-aligned bounding box (minX, minY, maxX, maxY).
	Bounds() Bounds
	// Is3D reports whether the geometry carries Z (XYZ). Bounds and 2D
	// operations ignore Z whether or not the geometry is 3D.
	Is3D() bool
	// Centroid returns the geometry's centroid using its type-specific
	// definition. For Point this is the identity; for lines, polygons,
	// and collections see the concrete implementations.
	Centroid() Point
	// WKT returns the Well-Known Text representation.
	WKT() string
	// AppendWKB appends the little-endian WKB encoding to buf and returns
	// the resulting slice.
	AppendWKB(buf []byte) []byte
}

// WKB returns the Well-Known Binary encoding of g.
func WKB(g Geometry) []byte { return g.AppendWKB(nil) }

// String returns g's WKT representation, prefixed by its CRS if set.
func String(g Geometry) string {
	if g == nil {
		return "<nil>"
	}
	if !g.CRS().Zero() {
		return fmt.Sprintf("%s %s", g.CRS(), g.WKT())
	}
	return g.WKT()
}

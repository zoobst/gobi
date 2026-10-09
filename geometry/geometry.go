package geometry

import (
	"fmt"
)

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

// IsEmpty reports whether g has no coordinates. Matches shapely's
// .is_empty semantics: a Point with the default (0,0) is NOT empty
// (shapely's convention), but a Polygon with no rings, a LineString
// with fewer than 2 points, or a MultiPolygon with no components IS.
func IsEmpty(g Geometry) bool {
	if g == nil {
		return true
	}
	switch t := g.(type) {
	case Point:
		return false
	case MultiPoint:
		return len(t.Points) == 0
	case LineString:
		return len(t.Points) < 2
	case MultiLineString:
		if len(t.Lines) == 0 {
			return true
		}
		for _, l := range t.Lines {
			if len(l.Points) >= 2 {
				return false
			}
		}
		return true
	case Polygon:
		return len(t.Rings) == 0 || len(t.Rings[0]) < 3
	case MultiPolygon:
		if len(t.Polygons) == 0 {
			return true
		}
		for _, p := range t.Polygons {
			if len(p.Rings) > 0 && len(p.Rings[0]) >= 3 {
				return false
			}
		}
		return true
	case GeometryCollection:
		if len(t.Geometries) == 0 {
			return true
		}
		for _, inner := range t.Geometries {
			if !IsEmpty(inner) {
				return false
			}
		}
		return true
	}
	return true
}

// IsValid reports whether g satisfies the OGC Simple Features validity
// rules gobi checks:
//
//   - LineString: >= 2 points, no consecutive duplicate vertices.
//   - Polygon: every ring closed (or auto-closable), >= 3 unique
//     vertices per ring, no ring self-intersection.
//   - Multi types: all components valid.
//   - Point / MultiPoint: always valid.
//
// This is deliberately a subset of GEOS's IsValid — full OGC validity
// includes checks like "holes lie inside exterior" and "rings don't
// touch except at points" which require more machinery than gobi
// currently exposes. Returns false for structurally-broken input;
// callers can rely on IsValid == true meaning "safe to pass to
// Boolean / Buffer without triggering degenerate-input paths."
func IsValid(g Geometry) bool {
	if g == nil {
		return false
	}
	switch t := g.(type) {
	case Point:
		return true
	case MultiPoint:
		return true
	case LineString:
		return validLineString(t.Points)
	case MultiLineString:
		for _, l := range t.Lines {
			if !validLineString(l.Points) {
				return false
			}
		}
		return true
	case Polygon:
		for _, r := range t.Rings {
			if !validRing(r) {
				return false
			}
		}
		return true
	case MultiPolygon:
		for _, p := range t.Polygons {
			if !IsValid(p) {
				return false
			}
		}
		return true
	case GeometryCollection:
		for _, inner := range t.Geometries {
			if !IsValid(inner) {
				return false
			}
		}
		return true
	}
	return false
}

func validLineString(pts []Point) bool {
	if len(pts) < 2 {
		return false
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].X == pts[i-1].X && pts[i].Y == pts[i-1].Y {
			return false // consecutive duplicate
		}
	}
	return true
}

func validRing(ring []Point) bool {
	if len(ring) < 3 {
		return false
	}
	closed := closedRing(ring)
	// Require enough unique vertices for a non-degenerate ring.
	unique := 0
	for i := range len(closed) - 1 {
		if i+1 < len(closed)-1 && closed[i].X == closed[i+1].X && closed[i].Y == closed[i+1].Y {
			continue
		}
		unique++
	}
	if unique < 3 {
		return false
	}
	// Self-intersection: any non-adjacent edge pair that crosses is
	// invalid. O(n²) but only runs when the caller explicitly asks;
	// typical Dissolve/Buffer outputs have simple rings.
	n := len(closed) - 1
	for i := range n {
		a0, a1 := closed[i], closed[i+1]
		// Skip adjacent edges (share a vertex).
		for j := i + 2; j < n; j++ {
			if i == 0 && j == n-1 {
				continue // wrap-around adjacent
			}
			b0, b1 := closed[j], closed[j+1]
			if segmentsProperlyCross(a0, a1, b0, b1) {
				return false
			}
		}
	}
	return true
}

// TypeString returns the OGC-style name for g's concrete type
// ("Point", "MultiPolygon", etc.). Matches shapely's .geom_type
// output.
func TypeString(g Geometry) string {
	if g == nil {
		return ""
	}
	return g.Type().String()
}

// Bounds is an axis-aligned 2D bounding box: (MinX, MinY, MaxX, MaxY).
type Bounds struct {
	MinX, MinY, MaxX, MaxY float64
}

// EmptyBounds returns a bounds value that will always be extended by the first
// point passed to Extend.
func EmptyBounds() Bounds {
	return Bounds{MinX: 1, MinY: 1, MaxX: -1, MaxY: -1} // deliberately inverted
}

// Empty reports whether the bounds are the inverted-sentinel returned
// by EmptyBounds — i.e. no point has ever been added to them.
//
// Empty is NOT the same as "zero-area rectangle." A Point's Bounds is
// {x, y, x, y} (min == max on both axes), which is not Empty — it's a
// legitimate zero-area rectangle carrying the point's location. Only
// the inverted sentinel signals "unset." Use IsZero to test for the
// zero-value Bounds{} shape, which callers using bounds as a
// reference-frame parameter often want to treat as "derive from data."
func (b Bounds) Empty() bool { return b.MinX > b.MaxX || b.MinY > b.MaxY }

// IsZero reports whether the bounds are the Go zero-value Bounds{}
// (all four fields == 0). Distinct from Empty (which catches the
// inverted-sentinel EmptyBounds()). Callers that accept bounds as an
// optional reference frame (see HilbertSortOptions.Bounds) treat
// IsZero as "unspecified — derive from data" so users don't need to
// know about the sentinel form.
func (b Bounds) IsZero() bool { return b == Bounds{} }

// Extend returns a Bounds enlarged to include (x, y).
func (b Bounds) Extend(x, y float64) Bounds {
	if b.Empty() {
		return Bounds{MinX: x, MinY: y, MaxX: x, MaxY: y}
	}
	if x < b.MinX {
		b.MinX = x
	}
	if x > b.MaxX {
		b.MaxX = x
	}
	if y < b.MinY {
		b.MinY = y
	}
	if y > b.MaxY {
		b.MaxY = y
	}
	return b
}

// Union returns the smallest Bounds containing both b and o.
func (b Bounds) Union(o Bounds) Bounds {
	if b.Empty() {
		return o
	}
	if o.Empty() {
		return b
	}
	return Bounds{
		MinX: min(b.MinX, o.MinX),
		MinY: min(b.MinY, o.MinY),
		MaxX: max(b.MaxX, o.MaxX),
		MaxY: max(b.MaxY, o.MaxY),
	}
}

// Contains reports whether the bounds contain (x, y). The upper edges are
// inclusive.
func (b Bounds) Contains(x, y float64) bool {
	return !b.Empty() && x >= b.MinX && x <= b.MaxX && y >= b.MinY && y <= b.MaxY
}

// Intersects reports whether the two bounds overlap.
func (b Bounds) Intersects(o Bounds) bool {
	if b.Empty() || o.Empty() {
		return false
	}
	return !(b.MaxX < o.MinX || b.MinX > o.MaxX || b.MaxY < o.MinY || b.MinY > o.MaxY)
}

// Centroid returns the centroid of g using each concrete type's own
// definition. Kept as a package-level helper for API symmetry with
// Area and Length; internally this is just interface dispatch on
// g.Centroid().
func Centroid(g Geometry) Point {
	if g == nil {
		return Point{}
	}
	return g.Centroid()
}

// Area returns the planar (XY) area of g in u². Non-polygonal geometries
// return 0.
func Area(g Geometry, u Unit) (float64, error) {
	switch t := g.(type) {
	case Polygon:
		return t.Area(u)
	case MultiPolygon:
		return t.Area(u)
	case GeometryCollection:
		var total float64
		for _, inner := range t.Geometries {
			a, err := Area(inner, u)
			if err != nil {
				return 0, err
			}
			total += a
		}
		return total, nil
	case Point, MultiPoint, LineString, MultiLineString:
		return 0, nil
	}
	return 0, fmt.Errorf("area: unsupported type %T", g)
}

// Length returns the planar (XY) length of g in u. Non-linear geometries
// (Point, MultiPoint, Polygon) return 0. Polygons don't return perimeter
// here — use Polygon.Perimeter for that.
func Length(g Geometry, u Unit) (float64, error) {
	switch t := g.(type) {
	case LineString:
		return t.Length(u)
	case MultiLineString:
		return t.Length(u)
	case GeometryCollection:
		var total float64
		for _, inner := range t.Geometries {
			l, err := Length(inner, u)
			if err != nil {
				return 0, err
			}
			total += l
		}
		return total, nil
	case Point, MultiPoint, Polygon, MultiPolygon:
		return 0, nil
	}
	return 0, fmt.Errorf("length: unsupported type %T", g)
}

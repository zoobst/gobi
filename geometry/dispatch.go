package geometry

import "fmt"

// Centroid returns the centroid of g using each concrete type's own
// definition. Unrecognized geometry types return an empty Point.
func Centroid(g Geometry) Point {
	switch t := g.(type) {
	case Point:
		return t
	case LineString:
		return t.Centroid()
	case Polygon:
		return t.Centroid()
	case MultiPoint:
		return t.Centroid()
	case MultiLineString:
		return t.Centroid()
	case MultiPolygon:
		return t.Centroid()
	case GeometryCollection:
		return t.Centroid()
	}
	return Point{}
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

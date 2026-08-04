package gobi

import (
	"github.com/zoobst/gobi/geometry"
)

// GeomCrossesAntimeridian returns a Boolean Series where row i is true
// if row i's geometry has any adjacent-vertex pair with |Δlon| > 180°,
// i.e. the edge between them wraps around the ±180° meridian. Only
// meaningful for geographic-CRS inputs; projected-CRS series always
// return false per row. Null rows produce null.
func (s Series) GeomCrossesAntimeridian() (Series, error) {
	return geomBoolFnOp(s, "_crosses_antimeridian", geometry.CrossesAntimeridian)
}

// GeomSplitAtAntimeridian returns a geometry Series where every
// antimeridian-crossing row is replaced by its split components
// (Polygon → MultiPolygon, LineString → MultiLineString). Non-crossing
// rows pass through unchanged. Points always pass through. Nulls stay
// null. See geometry.SplitAtAntimeridian for the crossing detection
// and interpolation semantics.
func (s Series) GeomSplitAtAntimeridian() (Series, error) {
	return geomTransformOp(s, "_split_antimeridian", func(g geometry.Geometry) (geometry.Geometry, error) {
		return geometry.SplitAtAntimeridian(g)
	})
}

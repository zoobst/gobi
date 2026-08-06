package gobi

import (
	"github.com/zoobst/gobi/geometry"
)

// GeomEllipseContains returns a Boolean Series where row i is true
// if the row's geometry is inside e. Points are tested directly;
// other geometry types are tested via their Centroid. Null rows
// pass through as null. Ellipse coordinates follow
// e.Center.CRSValue — reproject the input (GeomToCRS) to the same
// CRS before calling if they differ.
func (s Series) GeomEllipseContains(e geometry.Ellipse) (Series, error) {
	return geomBoolFnOp(s, "_in_ellipse", func(g geometry.Geometry) bool {
		return e.Contains(representativePoint(g))
	})
}

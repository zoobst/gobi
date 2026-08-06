package gobi

import (
	"fmt"

	"github.com/zoobst/gobi/geometry"
)

// GeomDensifyGeodesic replaces each row's LineString with its
// great-circle densification at ≤ stepMeters spacing (see
// geometry.DensifyGeodesic). Rows carrying non-LineString geometry
// pass through unchanged. Requires the Series' CRS metadata to be
// geographic (or unset — treated as WGS84); a projected CRS returns
// ErrGeodesicRequiresGeographic without inspecting per-row values.
//
// Null rows pass through as null.
func (s Series) GeomDensifyGeodesic(stepMeters float64) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	if !crs.Zero() && crs.Projected {
		return Series{}, fmt.Errorf("%w: got %s",
			geometry.ErrGeodesicRequiresGeographic, crs)
	}
	return geomTransformOp(s, "_densified", func(g geometry.Geometry) (geometry.Geometry, error) {
		l, ok := g.(geometry.LineString)
		if !ok {
			// Only LineStrings have "segments" in the geodesic sense.
			// Point / MultiPoint / Polygon / MultiPolygon pass
			// through untouched — callers wanting polygon-ring
			// densification can extract rings, densify each as a
			// LineString, and rebuild.
			return g, nil
		}
		l.CRSValue = crs
		return geometry.DensifyGeodesic(l, stepMeters)
	})
}

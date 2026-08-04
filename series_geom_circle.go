package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// GeomCircleContains returns a Boolean Series where row i is true if
// the row's geometry is inside c. Points are tested directly; other
// geometry types are tested via their Centroid. Null rows produce
// null. Circle units follow c.Center.CRSValue — reproject the input
// (via GeomToCRS) to the same CRS before calling if they differ.
func (s Series) GeomCircleContains(c geometry.Circle) (Series, error) {
	return geomBoolFnOp(s, "_in_circle", func(g geometry.Geometry) bool {
		p := representativePoint(g)
		return c.Contains(p)
	})
}

// GeomDistanceToCircle returns a Float64 Series with the SIGNED
// distance from each row's geometry (Point directly, otherwise its
// centroid) to c's boundary, in the requested unit. Negative when
// the point is inside the circle, positive outside, zero on the
// boundary. Null rows produce null.
//
// Distance is Euclidean in the coordinate plane's linear unit. For
// geographic-CRS input this is degrees × <unit conversion> —
// meaningless in physical distance terms. Project to a projected CRS
// (GeomToCRS) first for meters.
func (s Series) GeomDistanceToCircle(c geometry.Circle, u geometry.Unit) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	perM, err := geometry.MetersPerUnit(u)
	if err != nil {
		return Series{}, err
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	return geomFloat64Op(s, s.name+"_dist_to_circle", func(g geometry.Geometry) (float64, bool, error) {
		g = attachCRS(g, crs)
		p := representativePoint(g)
		d := c.Distance(p)
		// The signed distance is in the coord plane's unit (meters
		// for UTM, degrees for WGS84). Users pass a Unit assuming
		// meters as the base; conversion divides.
		if u == geometry.UnitMeters || u == "" {
			return d, true, nil
		}
		return d / perM, true, nil
	})
}

// GeomFitCircle fits a Circle across every non-null Point row (or
// centroid of non-Point rows) in s via least squares. Errors if
// fewer than 3 non-null rows are present or the input is
// collinear-degenerate. Uses Taubin by default (see
// geometry.FitCircle).
func (s Series) GeomFitCircle(opts geometry.CircleFitOptions) (geometry.Circle, error) {
	if !s.IsGeometry() {
		return geometry.Circle{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	pts := make([]geometry.Point, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return geometry.Circle{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return geometry.Circle{}, err
			}
			g = attachCRS(g, crs)
			pts = append(pts, representativePoint(g))
		}
	}
	c, _, err := geometry.FitCircle(pts, opts)
	return c, err
}

// representativePoint returns g's Point if g is a Point, otherwise
// its centroid. Used by circle predicates when the caller has a
// geometry column of mixed / non-Point types and we want a
// well-defined "one point per row" for cheap set tests.
func representativePoint(g geometry.Geometry) geometry.Point {
	if p, ok := g.(geometry.Point); ok {
		return p
	}
	return g.Centroid()
}

// The generic-Arrow-Builder plumbing is kept in the same style as
// the other Series geom ops (see series_geom_predicates.go /
// series_geom_metrics.go); this file only adds Circle-specific
// glue. Compile-time reference so the imports don't drift unused
// if this file's helpers are removed later.
var _ = memory.DefaultAllocator
var _ arrow.DataType = arrow.BinaryTypes.String

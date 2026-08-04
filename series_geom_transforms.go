package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// GeomBuffer returns a geometry Series holding a buffered version of
// each row. The distance is in the CRS's linear unit (meters for UTM,
// degrees for WGS84); the opts field controls smoothness (Segments)
// and shape (Style: BufferRound vs BufferSquare). Null rows pass
// through as null.
func (s Series) GeomBuffer(distance float64, opts geometry.BufferOptions) (Series, error) {
	return geomTransformOp(s, "_buffer", func(g geometry.Geometry) (geometry.Geometry, error) {
		return geometry.Buffer(g, distance, opts)
	})
}

// GeomSimplify returns a geometry Series with each row simplified via
// Douglas-Peucker at the given tolerance. Tolerance is in the CRS's
// linear unit — vertices within `tolerance` of a straight line between
// their neighbors are removed. Null rows pass through as null.
func (s Series) GeomSimplify(tolerance float64) (Series, error) {
	return geomTransformOp(s, "_simplify", func(g geometry.Geometry) (geometry.Geometry, error) {
		return geometry.Simplify(g, tolerance)
	})
}

// GeomConvexHull returns a geometry Series where each row is the
// convex hull of the input row's vertices (as a Polygon). Rows with
// fewer than 3 unique vertices produce an empty-ring Polygon. Null
// rows pass through as null.
func (s Series) GeomConvexHull() (Series, error) {
	return geomTransformOp(s, "_convex_hull", func(g geometry.Geometry) (geometry.Geometry, error) {
		return geometry.ConvexHull(g), nil
	})
}

// GeomEnvelope returns a geometry Series where each row is the
// axis-aligned bounding-box polygon of the input row. Matches
// geopandas's GeoSeries.envelope. Different from GeomBounds, which
// returns a 4-column Frame of MinX/MinY/MaxX/MaxY floats.
func (s Series) GeomEnvelope() (Series, error) {
	return geomTransformOp(s, "_envelope", func(g geometry.Geometry) (geometry.Geometry, error) {
		return geometry.Envelope(g), nil
	})
}

// geomTransformOp is the shared driver for row-wise Series → Series
// geometry transforms. Iterates non-null rows, calls fn on each parsed
// geometry, encodes the result back to WKB, and returns a new geometry
// Series with the same CRS metadata as the input.
func geomTransformOp(s Series, nameSuffix string, fn func(geometry.Geometry) (geometry.Geometry, error)) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				b.AppendNull()
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			g = attachCRS(g, crs)
			result, err := fn(g)
			if err != nil {
				return Series{}, err
			}
			b.Append(geometry.WKB(result))
		}
	}
	field := GeometryField(s.name+nameSuffix, epsg)
	return SeriesFromArray(field, b.NewArray()), nil
}

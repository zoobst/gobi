package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// GeomDistance returns a Float64 Series where row i is the minimum
// planar (Euclidean) distance from row i's geometry to other, in the
// requested unit. Returns 0 for intersecting geometries. Null inputs
// produce null outputs.
//
// Coordinates are treated as planar meters; for geographic CRSes
// project via GeomToCRS(WGS84 UTM) first or the result is Euclidean
// on lon/lat degrees, which is meaningless.
func (s Series) GeomDistance(other geometry.Geometry, u geometry.Unit) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	if other == nil {
		return Series{}, fmt.Errorf("geometry: nil `other` in GeomDistance")
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	return geomFloat64Op(s, s.name+"_distance", func(g geometry.Geometry) (float64, bool, error) {
		g = attachCRS(g, crs)
		d, err := geometry.GeomDistance(g, other, u)
		if err != nil {
			return 0, false, err
		}
		return d, true, nil
	})
}

// GeomType returns a String Series where row i is the OGC-style type
// name of row i's geometry ("Point", "MultiPolygon", etc.). Matches
// shapely's .geom_type.
func (s Series) GeomType() (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	pool := memory.DefaultAllocator
	b := array.NewStringBuilder(pool)
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
			b.Append(geometry.TypeString(g))
		}
	}
	field := arrow.Field{Name: s.name + "_geom_type", Type: arrow.BinaryTypes.String, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

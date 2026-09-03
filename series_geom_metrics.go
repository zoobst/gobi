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
//
// # Slice 13 SoA fast path
//
// For projected CRSes with meter-based linear units this dispatches
// to `geometry.PlanarMinDistanceFromWKB` when the row's bbox is
// disjoint from `other`'s bbox (the common case — most rows in a
// distance-scan don't overlap the target). Bbox disjoint means
// **definitely non-intersecting**, so the SoA min-distance kernel
// produces the correct answer without a segment-segment intersects
// check.
//
// Rows whose bboxes DO overlap `other` fall through to the AoS
// `geometry.GeomDistance` — those need the full intersects check
// to correctly return 0 on overlapping geometries. Geographic
// CRSes (haversine required) also fall back to AoS.
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

	// SoA fast path for projected CRSes: read row's bbox via
	// BoundsFromWKB (zero-alloc), bbox-disjoint rows skip the
	// AoS ParseWKB and go straight to WKB-direct min-distance.
	if crs.Projected {
		perM, err := geometry.MetersPerUnit(u)
		if err != nil {
			return Series{}, err
		}
		scale := 1 / perM
		otherBounds := other.Bounds()
		otherWKB := geometry.WKB(other)
		return geomFloat64OpWKB(s, s.name+"_distance", func(wkb []byte) (float64, bool, error) {
			rowBounds, err := geometry.BoundsFromWKB(wkb)
			if err != nil {
				return 0, false, err
			}
			if !rowBounds.Empty() && !otherBounds.Empty() && !rowBounds.Intersects(otherBounds) {
				// Bboxes disjoint → definitely non-intersecting →
				// SoA min-distance is correct.
				d, err := geometry.PlanarMinDistanceFromWKB(wkb, otherWKB)
				if err != nil {
					return 0, false, err
				}
				return d * scale, true, nil
			}
			// Bboxes overlap — fall through to AoS for the full
			// Intersects + min-distance semantics.
			g, err := geometry.ParseWKB(wkb)
			if err != nil {
				return 0, false, err
			}
			g = attachCRS(g, crs)
			d, err := geometry.GeomDistance(g, other, u)
			if err != nil {
				return 0, false, err
			}
			return d, true, nil
		})
	}

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
//
// SoA fast path (Slice 15): peeks the WKB type code via
// `geometry.WKBTypeCode` (zero-alloc, reads only the 5-byte header)
// and maps it to a string via `geometry.Type.String()`. No
// ParseWKB, no `[]Point` materialization.
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
			typ, _, err := geometry.WKBTypeCode(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			b.Append(geometry.Type(typ).String())
		}
	}
	field := arrow.Field{Name: s.name + "_geom_type", Type: arrow.BinaryTypes.String, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

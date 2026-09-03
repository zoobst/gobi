package gobi

import (
	"fmt"
	"math"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// GeomIntersects returns a Boolean Series where row i is true if the
// geometry at s[i] shares any point with other. Null rows produce null
// output. Matches geopandas's GeoSeries.intersects(other) semantics.
//
// Uses geometry.Intersects, which short-circuits on disjoint bboxes,
// so on a Series with many rows disjoint from other the loop is
// effectively linear in row count with O(1) work per row.
func (s Series) GeomIntersects(other geometry.Geometry) (Series, error) {
	return geomPredicateOp(s, other, geometry.PredIntersects, "_intersects", false)
}

// GeomContains returns a Boolean Series where row i is true if the
// geometry at s[i] fully contains other. Null rows produce null output.
// Matches geopandas's GeoSeries.contains(other) semantics.
func (s Series) GeomContains(other geometry.Geometry) (Series, error) {
	return geomPredicateOp(s, other, geometry.PredContains, "_contains", false)
}

// GeomWithin returns a Boolean Series where row i is true if the
// geometry at s[i] lies fully within other. Matches geopandas's
// GeoSeries.within(other) semantics. Equivalent to
// other.Contains(s[i]), so we swap the argument order in the
// per-row call.
func (s Series) GeomWithin(other geometry.Geometry) (Series, error) {
	return geomPredicateOp(s, other, geometry.PredContains, "_within", true)
}

// GeomDisjoint returns a Boolean Series where row i is true if the
// geometry at s[i] shares no point with other. This is !Intersects.
// Matches geopandas's GeoSeries.disjoint(other) semantics.
func (s Series) GeomDisjoint(other geometry.Geometry) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	inter, err := s.GeomIntersects(other)
	if err != nil {
		return Series{}, err
	}
	return invertBoolSeries(inter, s.name+"_disjoint")
}

// GeomTouches returns a Boolean Series where row i is true if row i's
// geometry shares boundary points with other but no interior points.
// Matches geopandas's GeoSeries.touches(other) semantics.
func (s Series) GeomTouches(other geometry.Geometry) (Series, error) {
	return geomBoolFnOp(s, "_touches", func(g geometry.Geometry) bool {
		return geometry.Touches(g, other)
	})
}

// GeomOverlaps returns a Boolean Series where row i is true if row i's
// geometry shares interior points with other but neither contains the
// other, and both are of the same dimension. Matches geopandas's
// GeoSeries.overlaps(other) semantics.
func (s Series) GeomOverlaps(other geometry.Geometry) (Series, error) {
	return geomBoolFnOp(s, "_overlaps", func(g geometry.Geometry) bool {
		return geometry.Overlaps(g, other)
	})
}

// GeomCrosses returns a Boolean Series where row i is true if row i's
// geometry crosses other — typically LineString × Polygon or
// LineString × LineString with mixed dimensions. Matches geopandas's
// GeoSeries.crosses(other) semantics.
func (s Series) GeomCrosses(other geometry.Geometry) (Series, error) {
	return geomBoolFnOp(s, "_crosses", func(g geometry.Geometry) bool {
		return geometry.Crosses(g, other)
	})
}

// GeomIsEmpty returns a Boolean Series where row i is true if the
// geometry at s[i] has no coordinates (empty ring, empty collection,
// LineString with fewer than 2 points). Matches shapely's .is_empty.
func (s Series) GeomIsEmpty() (Series, error) {
	return geomBoolFnOp(s, "_is_empty", geometry.IsEmpty)
}

// GeomIsValid returns a Boolean Series where row i is true if the
// geometry at s[i] passes gobi's structural validity checks
// (see geometry.IsValid).
func (s Series) GeomIsValid() (Series, error) {
	return geomBoolFnOp(s, "_is_valid", geometry.IsValid)
}

// GeomDWithin returns a Boolean Series where row i is true if the
// geometry at s[i] is within `distance` coordinate units of other.
// The killer spatial-join operator — "rows whose geometry is at
// most 5km from this line" or "polygons within 100m of this AOI"
// map straight to this call.
//
// distance is measured in the column's coordinate units — meters
// for projected CRSes (UTM, PseudoMercator), degrees for lon/lat.
// For lon/lat data, project to a suitable CRS first (see GeomToCRS)
// so distance comparisons stay meaningful.
//
// distance = 0 degenerates to GeomIntersects. Negative or NaN
// distance yields all-false (matches the geometry.WithinDistance
// contract). Null rows produce null output.
//
// Under the hood, the bbox short-circuit in WithinDistance skips
// polygons whose bounding rectangle is already farther than
// distance from other's bbox — no WKB decoding of the far rows'
// interiors. Combined with the v0.3.4 row-group pushdown, this is
// what makes a "roads within 100m" query over a million-row parquet
// finish in tens of milliseconds instead of seconds.
func (s Series) GeomDWithin(other geometry.Geometry, distance float64) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	if other == nil {
		return Series{}, fmt.Errorf("geometry: nil `other` in GeomDWithin")
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	otherBounds := other.Bounds()
	// Slice 15 SoA fast path: per-row bbox-min-distance reject.
	// If the row's bbox is already farther than `distance` from
	// other's bbox, no interior point pair can be closer — emit
	// false without ParseWKB. Same shape as Slice 14 predicate
	// wire-in.
	//
	// Skipped when distance is NaN / negative (all-false trivially)
	// or when otherBounds is empty (nothing to compare against).
	bboxReject := !otherBounds.Empty() && !math.IsNaN(distance) && distance >= 0
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
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
			wkb := bin.Value(i)
			if bboxReject {
				rowBounds, err := geometry.BoundsFromWKB(wkb)
				if err != nil {
					return Series{}, err
				}
				if !rowBounds.Empty() &&
					geometry.BoundsMinDistance(rowBounds, otherBounds) > distance {
					b.Append(false)
					continue
				}
			}
			g, err := geometry.ParseWKB(wkb)
			if err != nil {
				return Series{}, err
			}
			g = attachCRS(g, crs)
			b.Append(geometry.WithinDistance(g, other, distance))
		}
	}
	field := arrow.Field{Name: s.name + "_dwithin", Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

// geomBoolFnOp is the shared driver for row-wise Series → Bool ops
// whose predicate needs only the row's own geometry (no scalar
// argument). Null rows pass through as null.
func geomBoolFnOp(s Series, nameSuffix string, fn func(geometry.Geometry) bool) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
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
			b.Append(fn(g))
		}
	}
	field := arrow.Field{Name: s.name + nameSuffix, Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

// geomPredicateOp is the shared driver for row-wise geometry × scalar
// spatial-predicate ops. Emits a Boolean column; null rows pass through
// as null. When swap is true, the predicate is evaluated with the row
// and `other` swapped (used by GeomWithin, which is Contains reversed).
// Follows the release discipline from geomBinaryOp so Arrow refcounts
// stay balanced on every path.
//
// # SoA fast path (Slice 14)
//
// Per row: read the row's bbox via BoundsFromWKB (zero-alloc) and
// call geometry.BoundsCompatible(pred, aBounds, bBounds). If the
// predicate's necessary-condition on bboxes rejects the row →
// emit false without a ParseWKB. If the bboxes are compatible →
// fall through to the AoS ParseWKB + Test path (potentially still
// a false answer, just not shortcuttable by bbox alone).
//
// For filter-shaped workloads where most rows are disjoint from
// `other`, the ParseWKB is skipped on the majority of rows —
// mirrors the Slice 13 GeomDistance pattern.
func geomPredicateOp(s Series, other geometry.Geometry, pred geometry.Predicate, nameSuffix string, swap bool) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	if other == nil {
		return Series{}, fmt.Errorf("geometry: nil `other` in predicate op")
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	otherBounds := other.Bounds()
	// Bbox-reject fast path only fires on positive predicates
	// (Intersects / Contains / Within / Touches / Crosses / Overlaps)
	// — those have the "bboxes-fail → predicate-false" necessary
	// condition. PredDisjoint has the opposite polarity so a bbox
	// mismatch would need to short-circuit true, not false — falls
	// through to the AoS Test path.
	bboxReject := pred != geometry.PredDisjoint
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
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
			wkb := bin.Value(i)
			if bboxReject {
				rowBounds, err := geometry.BoundsFromWKB(wkb)
				if err != nil {
					return Series{}, err
				}
				var ab, bb geometry.Bounds
				if swap {
					ab, bb = otherBounds, rowBounds
				} else {
					ab, bb = rowBounds, otherBounds
				}
				if !geometry.BoundsCompatible(pred, ab, bb) {
					b.Append(false)
					continue
				}
			}
			g, err := geometry.ParseWKB(wkb)
			if err != nil {
				return Series{}, err
			}
			g = attachCRS(g, crs)
			var result bool
			if swap {
				result = geometry.Test(pred, other, g)
			} else {
				result = geometry.Test(pred, g, other)
			}
			b.Append(result)
		}
	}
	field := arrow.Field{Name: s.name + nameSuffix, Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

// invertBoolSeries returns a Boolean Series whose values are !s, preserving
// nulls. Used by GeomDisjoint, which is defined as !Intersects.
func invertBoolSeries(s Series, outName string) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		boolArr, ok := chunk.(*array.Boolean)
		if !ok {
			return Series{}, fmt.Errorf("%w: expected Boolean, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range boolArr.Len() {
			if boolArr.IsNull(i) {
				b.AppendNull()
				continue
			}
			b.Append(!boolArr.Value(i))
		}
	}
	field := arrow.Field{Name: outName, Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

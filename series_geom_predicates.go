package gobi

import (
	"fmt"

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

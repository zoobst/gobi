package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// GeomClip returns a geometry Series holding the intersection of each
// geometry in s with the mask. Null inputs pass through as null. Both s
// and mask must be in a projected CRS (or unset), and mask must be a
// Polygon or MultiPolygon; other geometry types return an error.
func (s Series) GeomClip(mask geometry.Geometry) (Series, error) {
	return geomBinaryOp(s, mask, geometry.OpIntersection, "_clip")
}

// GeomUnion returns a geometry Series holding the union of each row with
// other. See GeomClip for input requirements.
func (s Series) GeomUnion(other geometry.Geometry) (Series, error) {
	return geomBinaryOp(s, other, geometry.OpUnion, "_union")
}

// GeomDifference returns a geometry Series holding s.geom minus other for
// each row. See GeomClip for input requirements.
func (s Series) GeomDifference(other geometry.Geometry) (Series, error) {
	return geomBinaryOp(s, other, geometry.OpDifference, "_diff")
}

// GeomSymDifference returns a geometry Series holding the symmetric
// difference of each row with other. See GeomClip for input requirements.
func (s Series) GeomSymDifference(other geometry.Geometry) (Series, error) {
	return geomBinaryOp(s, other, geometry.OpSymDifference, "_symdiff")
}

// GeomEstimateUTMCRS returns the WGS 84 / UTM zone CRS covering the total
// bounds of every non-null geometry in s. Matches geopandas's
// GeoDataFrame.estimate_utm_crs() by aggregating over the collection
// rather than per-row. Returns ErrEmptyGeometry if every row is null.
func (s Series) GeomEstimateUTMCRS() (geometry.CRS, error) {
	if !s.IsGeometry() {
		return geometry.CRS{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	sourceCRS, _ := geometry.LookupCRS(epsg)
	if sourceCRS.Zero() {
		sourceCRS = geometry.WGS84
	}
	bounds := geometry.EmptyBounds()
	seen := false
	crossesAM := false
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return geometry.CRS{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return geometry.CRS{}, err
			}
			g = attachCRS(g, sourceCRS)
			bounds = bounds.Union(g.Bounds())
			seen = true
			if !crossesAM && geometry.CrossesAntimeridian(g) {
				crossesAM = true
			}
		}
	}
	// Geographic-CRS input that spans the antimeridian → bounds center
	// picks the wrong zone silently. Refuse and let the caller pre-split.
	if crossesAM && !sourceCRS.Projected {
		return geometry.CRS{}, geometry.ErrAntimeridianCrossing
	}
	// Also refuse when the aggregated bounds itself spans >180° (which
	// can happen with disjoint rows on either side of the antimeridian
	// even if no single row crosses).
	if !sourceCRS.Projected && (bounds.MaxX-bounds.MinX) > 180 {
		return geometry.CRS{}, geometry.ErrAntimeridianCrossing
	}
	if !seen {
		return geometry.CRS{}, geometry.ErrEmptyGeometry
	}
	cx := (bounds.MinX + bounds.MaxX) / 2
	cy := (bounds.MinY + bounds.MaxY) / 2
	lon, lat := cx, cy
	if sourceCRS.Projected {
		g := geometry.Point{X: cx, Y: cy, CRSValue: sourceCRS}
		reprojected, err := geometry.Project(g, geometry.WGS84)
		if err != nil {
			return geometry.CRS{}, err
		}
		p := reprojected.(geometry.Point)
		lon, lat = p.X, p.Y
	}
	return geometry.LookupCRS(geometry.UTMEpsgFor(lon, lat))
}

// GeomToCRS reprojects every geometry in s into target, returning a new
// Series whose field is tagged with target's EPSG code. Null rows pass
// through as null. Both this and geometry.Project must know how to bridge
// the source and target CRSes (currently WGS84 ↔ PseudoMercator ↔ UTM).
func (s Series) GeomToCRS(target geometry.CRS) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	sourceCRS, _ := geometry.LookupCRS(epsg)
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
			g = attachCRS(g, sourceCRS)
			reprojected, err := geometry.Project(g, target)
			if err != nil {
				return Series{}, err
			}
			b.Append(geometry.WKB(reprojected))
		}
	}
	field := GeometryField(s.name+"_"+target.String(), target.EPSG)
	return SeriesFromArray(field, b.NewArray()), nil
}

// GeomDissolve returns a single Geometry containing the union of every
// geometry in s. Rows with null geometry are skipped. Returns an empty
// Polygon when the series is empty or all rows are null.
func (s Series) GeomDissolve() (geometry.Geometry, error) {
	if !s.IsGeometry() {
		return nil, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	geoms := make([]geometry.Geometry, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return nil, err
			}
			geoms = append(geoms, attachCRS(g, crs))
		}
	}
	return geometry.Dissolve(geoms)
}

// geomBinaryOp is the shared driver for row-wise geometry × scalar-mask
// boolean operations. Writes results as WKB into a new Binary column that
// inherits s's CRS metadata. Follows the release pattern from
// GeomCentroid to keep Arrow refcounts balanced on every path.
func geomBinaryOp(s Series, other geometry.Geometry, op geometry.BoolOp, nameSuffix string) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
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
			result, err := geometry.Boolean(g, other, op, geometry.ClipOptions{})
			if err != nil {
				return Series{}, err
			}
			b.Append(geometry.WKB(result))
		}
	}
	field := GeometryField(s.name+nameSuffix, epsg)
	// SeriesFromArray takes ownership of the returned array and releases it
	// internally along with the chunked column it wraps.
	return SeriesFromArray(field, b.NewArray()), nil
}

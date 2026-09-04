package gobi

import (
	"encoding/binary"
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

// combineDisjointMultiPolygonWKB builds a MultiPolygon WKB
// containing the polygon components of `a` and `b`. Used by the
// Slice-21 Series-level Union / SymDifference bbox-disjoint fast
// path where the correct algebraic result is a MultiPolygon of
// both operands.
//
// Both inputs must be well-formed Polygon or MultiPolygon WKB
// blobs (checked by the caller via WKBTypeCode == 3 or 6). Any
// nested MultiPolygon has its inner polygons flattened into the
// output.
//
// # Endianness
//
// Post-review fix: the fast path now requires both inputs to
// carry the LE byte-order marker (`data[0] == 1`). The earlier
// body force-normalized the outer header to LE but appended
// inner polygon bytes verbatim — if a source was BE that
// produced a mixed-endian blob. OGC spec allows per-member BOMs,
// but many readers (gobi's own ParseWKB included, historically)
// assume uniform endianness within a blob, so mixed output is
// a foot-gun. Returns (result, ok). ok=false means the caller
// must fall through to the AoS path (ParseWKB + combineDisjoint
// + re-emit) so BE inputs still get correct output.
//
// Also validates the input length is sufficient to safely
// slice the body offset — truncated inputs that pass
// WKBTypeCode's 5-byte gate can still fail the 9-byte
// MultiPolygon count read.
func combineDisjointMultiPolygonWKB(a, b []byte) ([]byte, bool) {
	// Both inputs must be LE. gobi writes LE by convention so
	// this covers the common case; BE inputs (rare — mostly
	// external files) fall through to the correct-but-slow AoS
	// path.
	if len(a) < 5 || a[0] != 1 || len(b) < 5 || b[0] != 1 {
		return nil, false
	}
	nA, aBodyOff, aOK := multiPolygonBodyOffset(a)
	if !aOK {
		return nil, false
	}
	nB, bBodyOff, bOK := multiPolygonBodyOffset(b)
	if !bOK {
		return nil, false
	}
	total := nA + nB
	// Header: 1 byte order + 4 type code + 4 numPolys = 9 bytes.
	out := make([]byte, 9, 9+len(a)+len(b))
	out[0] = 1                                 // LE byte order
	binary.LittleEndian.PutUint32(out[1:5], 6) // MultiPolygon type
	binary.LittleEndian.PutUint32(out[5:9], uint32(total))
	// Append `a`'s polygon members. Both sides confirmed LE
	// above, so appending inner bytes verbatim yields a
	// uniformly-LE output.
	if aIsPolygon(a) {
		out = append(out, a...)
	} else {
		out = append(out, a[aBodyOff:]...)
	}
	if aIsPolygon(b) {
		out = append(out, b...)
	} else {
		out = append(out, b[bBodyOff:]...)
	}
	return out, true
}

// multiPolygonBodyOffset returns (n_polys, offset_of_first_inner_polygon_wkb, ok).
// A Polygon returns (1, 0, true). A MultiPolygon returns
// (n, 9, true). Truncated / unrecognized inputs return (_, _, false)
// so the caller can bail before slicing into short data (the
// pre-review body returned (0, 9, _) on truncated input, which
// caused the caller's `data[9:]` slice to panic).
func multiPolygonBodyOffset(data []byte) (int, int, bool) {
	typ, _, err := geometry.WKBTypeCode(data)
	if err != nil {
		return 0, 0, false
	}
	if typ == 3 {
		return 1, 0, true
	}
	if typ != 6 {
		return 0, 0, false
	}
	// MultiPolygon: header 5 bytes + 4-byte count.
	if len(data) < 9 {
		return 0, 0, false
	}
	// Byte order determines endianness of the count field.
	var n uint32
	if data[0] == 1 {
		n = binary.LittleEndian.Uint32(data[5:9])
	} else {
		n = binary.BigEndian.Uint32(data[5:9])
	}
	return int(n), 9, true
}

// aIsPolygon reports whether wkb encodes a single Polygon (not a
// MultiPolygon). Used to pick the append shape for the Slice-21
// concat helper.
func aIsPolygon(wkb []byte) bool {
	typ, _, err := geometry.WKBTypeCode(wkb)
	return err == nil && typ == 3
}

// geomBinaryOp is the shared driver for row-wise geometry × scalar-mask
// boolean operations. Writes results as WKB into a new Binary column that
// inherits s's CRS metadata. Follows the release pattern from
// GeomCentroid to keep Arrow refcounts balanced on every path.
//
// # Slice 17 + 21 SoA bbox fast paths
//
// For all four ops (Intersection / Union / Difference /
// SymDifference), the geometry package's `trivialReject` shape
// (Slice 17 for Int/Diff, Slice 21 for Union/SymDiff) is applied
// at the Series level via a per-row `BoundsFromWKB` check:
//
//   - OpIntersection + row bbox disjoint → emit pre-computed empty
//     Polygon WKB.
//   - OpDifference + row bbox disjoint → re-emit the row's own
//     WKB bytes unchanged (a - b = a when a and b don't touch).
//   - OpUnion + row bbox disjoint → emit a MultiPolygon WKB
//     built by concatenating row's WKB and other's WKB (both
//     must be Polygon or MultiPolygon — otherwise fall through).
//     Matches AoS `combineDisjoint` shape.
//   - OpSymDifference + row bbox disjoint → same as Union (a △ b
//     when disjoint = a ∪ b = MultiPolygon[a, b]).
func geomBinaryOp(s Series, other geometry.Geometry, op geometry.BoolOp, nameSuffix string) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	other = attachCRS(other, crs)
	otherBounds := other.Bounds()
	// Pre-compute canonical outputs for the bbox-disjoint fast paths.
	var (
		emptyPolygonWKB []byte
		otherWKB        []byte
		otherIsPoly     bool
	)
	fastPathEligible := !otherBounds.Empty()
	if op == geometry.OpIntersection {
		emptyPolygonWKB = geometry.WKB(geometry.Polygon{CRSValue: crs})
	}
	if op == geometry.OpUnion || op == geometry.OpSymDifference {
		otherWKB = geometry.WKB(other)
		if typ, _, err := geometry.WKBTypeCode(otherWKB); err == nil && (typ == 3 || typ == 6) {
			otherIsPoly = true
		} else {
			// Non-polygon `other` — fast path can't build the
			// MultiPolygon inline; fall through to AoS for every row.
			fastPathEligible = false
		}
	}
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
			wkb := bin.Value(i)
			if fastPathEligible {
				rowBounds, err := geometry.BoundsFromWKB(wkb)
				if err != nil {
					return Series{}, err
				}
				if !rowBounds.Empty() && !rowBounds.Intersects(otherBounds) {
					switch op {
					case geometry.OpIntersection:
						b.Append(emptyPolygonWKB)
						continue
					case geometry.OpDifference:
						b.Append(wkb)
						continue
					case geometry.OpUnion, geometry.OpSymDifference:
						if otherIsPoly {
							rowTyp, _, err := geometry.WKBTypeCode(wkb)
							if err == nil && (rowTyp == 3 || rowTyp == 6) {
								if combined, ok := combineDisjointMultiPolygonWKB(wkb, otherWKB); ok {
									b.Append(combined)
									continue
								}
								// BE or truncated WKB — fall through to AoS.
							}
						}
						// Row is a non-polygon type — fall through.
					}
				}
			}
			g, err := geometry.ParseWKB(wkb)
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

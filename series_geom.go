package gobi

import (
	"fmt"
	"math"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi/geometry"
)

// GeomArea returns a Float64 Series holding the planar (XY) area of each
// geometry in s, in u². Non-polygonal geometries contribute 0. Null
// geometries produce null values.
//
// For projected CRSes with a meter-based linear unit (the geoparquet
// default), this dispatches to geometry.PlanarAreaFromWKB — a
// byte-stream shoelace scanner that skips the ParseWKB alloc. Other
// CRSes (geographic, or projected with a non-meter linear unit) fall
// through to the AoS path.
func (s Series) GeomArea(u geometry.Unit) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	if crs.Projected() {
		perM, err := geometry.MetersPerUnit(u)
		if err != nil {
			return Series{}, err
		}
		scale := 1 / (perM * perM)
		return geomFloat64OpWKB(s, s.name+"_area", func(wkb []byte) (float64, bool, error) {
			a, err := geometry.PlanarAreaFromWKB(wkb)
			if err != nil {
				return 0, false, err
			}
			return a * scale, true, nil
		})
	}
	return geomFloat64Op(s, s.name+"_area", func(g geometry.Geometry) (float64, bool, error) {
		g = attachCRS(g, crs)
		a, err := geometry.Area(g, u)
		if err != nil {
			return 0, false, err
		}
		return a, true, nil
	})
}

// GeomLength returns a Float64 Series holding the planar (XY) length of
// each geometry in u. Non-linear geometries contribute 0. Null geometries
// produce null values.
//
// For projected CRSes this dispatches to geometry.PlanarLengthFromWKB —
// a byte-stream Euclidean-segment scanner that skips the ParseWKB
// alloc. Geographic CRSes fall through to the AoS haversine path.
func (s Series) GeomLength(u geometry.Unit) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	if crs.Projected() {
		perM, err := geometry.MetersPerUnit(u)
		if err != nil {
			return Series{}, err
		}
		scale := 1 / perM
		return geomFloat64OpWKB(s, s.name+"_length", func(wkb []byte) (float64, bool, error) {
			l, err := geometry.PlanarLengthFromWKB(wkb)
			if err != nil {
				return 0, false, err
			}
			return l * scale, true, nil
		})
	}
	return geomFloat64Op(s, s.name+"_length", func(g geometry.Geometry) (float64, bool, error) {
		g = attachCRS(g, crs)
		l, err := geometry.Length(g, u)
		if err != nil {
			return 0, false, err
		}
		return l, true, nil
	})
}

// GeomCentroid returns a geometry Series holding the centroid of each
// input geometry as a Point (encoded as WKB). Null inputs produce nulls.
// The output column inherits s's CRS.
//
// # SoA fast path (Slice 14)
//
// For Point / LineString / Polygon / MultiPoint / MultiLineString /
// GeometryCollection rows this dispatches to
// `geometry.CentroidFromWKB` — a byte-stream centroid scanner that
// skips the ParseWKB alloc. MultiPolygon rows fall back to AoS
// ParseWKB → Centroid because `CentroidFromWKB` uses bbox-center
// for MultiPolygons vs the AoS's area-weighted definition.
// Preserving the AoS semantic keeps `GeomCentroid` behavior stable
// for existing users.
func (s Series) GeomCentroid() (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()

	for _, chunk := range s.col.Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				b.AppendNull()
				continue
			}
			wkb := bin.Value(i)
			typ, _, err := geometry.WKBTypeCode(wkb)
			if err != nil {
				return Series{}, err
			}
			// MultiPolygon (type 6) needs the AoS area-weighted
			// centroid — CentroidFromWKB returns bbox-center for
			// this shape, which is a documented but observable
			// divergence.
			if typ == 6 {
				g, err := geometry.ParseWKB(wkb)
				if err != nil {
					return Series{}, err
				}
				c := geometry.Centroid(g)
				b.Append(geometry.WKB(c))
				continue
			}
			c, err := geometry.CentroidFromWKB(wkb)
			if err != nil {
				return Series{}, err
			}
			b.Append(geometry.WKB(c))
		}
	}

	arr := b.NewArray()
	defer arr.Release()
	field := GeometryField(s.name+"_centroid", epsg)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	return Series{name: field.Name, field: field, col: col}, nil
}

// GeomBounds returns a Frame with four Float64 columns — MinX, MinY, MaxX,
// MaxY — one row per input geometry. Null geometries produce four nulls.
//
// SoA fast path (Slice 14): dispatches to `geometry.BoundsFromWKB`
// unconditionally — bbox semantics match `g.Bounds()` exactly for
// every geometry type, so no per-shape dispatch is needed.
func (s Series) GeomBounds() (*Frame, error) {
	if !s.IsGeometry() {
		return nil, ErrNotGeometry
	}
	pool := memory.DefaultAllocator
	mkBuilder := func() *array.Float64Builder { return array.NewFloat64Builder(pool) }
	minX, minY, maxX, maxY := mkBuilder(), mkBuilder(), mkBuilder(), mkBuilder()
	defer minX.Release()
	defer minY.Release()
	defer maxX.Release()
	defer maxY.Release()

	for _, chunk := range s.col.Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				minX.AppendNull()
				minY.AppendNull()
				maxX.AppendNull()
				maxY.AppendNull()
				continue
			}
			b, err := geometry.BoundsFromWKB(bin.Value(i))
			if err != nil {
				return nil, err
			}
			minX.Append(b.MinX)
			minY.Append(b.MinY)
			maxX.Append(b.MaxX)
			maxY.Append(b.MaxY)
		}
	}

	fields := []arrow.Field{
		{Name: "minx", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "miny", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "maxx", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "maxy", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	arrs := []arrow.Array{minX.NewArray(), minY.NewArray(), maxX.NewArray(), maxY.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	schema := arrow.NewSchema(fields, nil)
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	return NewFrame(schema, cols)
}

// geomFloat64OpWKB is the SoA-scanner variant of geomFloat64Op:
// hands raw WKB bytes to fn instead of decoding to a Geometry. Used
// by GeomArea / GeomLength on projected CRSes where the byte-stream
// planar scanners (PlanarAreaFromWKB / PlanarLengthFromWKB) skip
// the per-row ParseWKB alloc.
func geomFloat64OpWKB(s Series, outName string, fn func([]byte) (float64, bool, error)) (Series, error) {
	b := array.NewFloat64Builder(memory.DefaultAllocator)
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
			v, ok, err := fn(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			if !ok {
				b.AppendNull()
				continue
			}
			b.Append(v)
		}
	}
	return newSeriesFromArray(outName, b.NewArray()), nil
}

// geomFloat64Op is a shared driver for row-by-row geometry → float64
// series ops. Callers supply a function returning (value, valid, error).
func geomFloat64Op(s Series, outName string, fn func(geometry.Geometry) (float64, bool, error)) (Series, error) {
	b := array.NewFloat64Builder(memory.DefaultAllocator)
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
			v, ok, err := fn(g)
			if err != nil {
				return Series{}, err
			}
			if !ok {
				b.AppendNull()
				continue
			}
			b.Append(v)
		}
	}
	return newSeriesFromArray(outName, b.NewArray()), nil
}

// attachCRS mutates g in place to carry the given CRS. Only the concrete
// geometry types with a CRSValue field are supported.
func attachCRS(g geometry.Geometry, crs geometry.CRS) geometry.Geometry {
	if crs.Zero() {
		return g
	}
	// The concrete types are value receivers so mutation-in-place doesn't
	// affect the caller's copy — this helper returns the value that has the
	// CRS attached; callers that need it should use the return value.
	switch t := g.(type) {
	case geometry.Point:
		t.CRSValue = crs
		return t
	case geometry.LineString:
		t.CRSValue = crs
		return t
	case geometry.Polygon:
		t.CRSValue = crs
		return t
	case geometry.MultiPoint:
		t.CRSValue = crs
		return t
	case geometry.MultiLineString:
		t.CRSValue = crs
		return t
	case geometry.MultiPolygon:
		t.CRSValue = crs
		return t
	case geometry.GeometryCollection:
		t.CRSValue = crs
		return t
	}
	return g
}

// GeomBuffer returns a geometry Series holding a buffered version of
// each row. The distance is in the CRS's linear unit (meters for UTM,
// degrees for WGS84); the opts field controls smoothness (Segments)
// and shape (Style: BufferRound vs BufferSquare). Null rows pass
// through as null.
//
// Point rows take a fast path for round buffers: the circle is written
// straight to WKB (same bytes as the general path) without building a
// Polygon. Together with the pre-sized output buffer, that took 1M
// points from 6.5 GB allocated / 1.87 GB peak / 1.2 s to 0.58 GB /
// 0.63 GB / 0.23 s.
func (s Series) GeomBuffer(distance float64, opts geometry.BufferOptions) (Series, error) {
	var fast geomFastWKB
	if enc := geometry.NewPointBufferEncoder(distance, opts); enc != nil {
		fast = func(dst, wkb []byte) ([]byte, bool, error) {
			x, y, _, ok, err := geometry.PointXYZFromWKB(wkb)
			if err != nil || !ok || math.IsNaN(x) || math.IsNaN(y) {
				// Not a point, empty, or unreadable: the general path
				// handles (and reports) it exactly as before.
				return dst, false, nil
			}
			return enc.AppendWKB(dst, x, y), true, nil
		}
	}
	return geomTransformOpFast(s, "_buffer", fast, func(g geometry.Geometry) (geometry.Geometry, error) {
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
	return geomTransformOpFast(s, nameSuffix, nil, fn)
}

// geomFastWKB is an optional WKB → WKB shortcut for geomTransformOp:
// it appends row's output to dst and returns ok=true, or returns
// ok=false to send the row through the general parse → fn → encode
// path.
type geomFastWKB func(dst, wkb []byte) (out []byte, ok bool, err error)

// geomReserveSample is how many rows geomTransformOpFast encodes before
// sizing the output buffer from their average length.
const geomReserveSample = 256

// geomTransformOpFast is geomTransformOp with an optional fast path.
//
// Memory: each row is encoded into one reused scratch buffer, and the
// output's value buffer is reserved once — sized from the average of
// the first geomReserveSample rows, plus 1/16 headroom — instead of
// growing by doubling. Doubling re-copies the whole buffer at every
// step and can leave up to half of it as slack in the final array.
func geomTransformOpFast(s Series, nameSuffix string, fast geomFastWKB, fn func(geometry.Geometry) (geometry.Geometry, error)) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()
	n := s.Len()
	b.Reserve(n)

	var (
		scratch      []byte
		row          int
		sampleBytes  int
		sampleRows   int
		reservedData bool
	)
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			row++
			if bin.IsNull(i) {
				b.AppendNull()
				continue
			}
			wkb := bin.Value(i)
			var done bool
			if fast != nil {
				out, ok, err := fast(scratch[:0], wkb)
				if err != nil {
					return Series{}, err
				}
				if ok {
					scratch, done = out, true
				}
			}
			if !done {
				g, err := geometry.ParseWKB(wkb)
				if err != nil {
					return Series{}, err
				}
				g = attachCRS(g, crs)
				result, err := fn(g)
				if err != nil {
					return Series{}, err
				}
				scratch = result.AppendWKB(scratch[:0])
			}
			b.Append(scratch)

			if !reservedData {
				sampleBytes += len(scratch)
				sampleRows++
				if sampleRows == geomReserveSample {
					est := sampleBytes / sampleRows * (n - row)
					b.ReserveData(est + est/16)
					reservedData = true
				}
			}
		}
	}
	field := GeometryField(s.name+nameSuffix, epsg)
	return SeriesFromArray(field, b.NewArray()), nil
}

// PointsFromXY builds a geometry Series of 2D WKB Points from two
// coordinate columns. x and y must be numeric (Float64, Float32,
// Int64, or Int32) and the same length. Mixed-type inputs are
// promoted to Float64. Null values on either side emit a null
// geometry for that row.
//
// The returned Series is a WKB Binary column tagged with geometry
// metadata + the given EPSG code, so it plugs directly into
// Frame.WithColumn, Frame.SJoin, GeoParquet write paths, and other
// geometry-aware operations.
//
// Modeled on geopandas.points_from_xy — the intended flow is to build
// a geometry column from two attribute columns without hand-rolling
// the WKB encoding:
//
//	lat, _ := df.Column("lat")
//	lng, _ := df.Column("lng")
//	geom, _ := gobi.PointsFromXY(lng, lat, 4326)   // x=lng, y=lat
//	df, _ = df.WithColumn("geometry", geom)
//
// Note the argument order: x first, y second. In geographic
// coordinates that means longitude first, latitude second — matching
// GeoJSON / WKB / shapefile conventions (and geopandas).
func PointsFromXY(x, y Series, crs int32) (Series, error) {
	n := x.Len()
	if y.Len() != n {
		return Series{}, fmt.Errorf("%w: x has %d rows, y has %d",
			ErrColumnLenMismatch, n, y.Len())
	}
	if !x.isNumeric() {
		return Series{}, fmt.Errorf("PointsFromXY: x column: %w", ErrNotNumeric)
	}
	if !y.isNumeric() {
		return Series{}, fmt.Errorf("PointsFromXY: y column: %w", ErrNotNumeric)
	}

	crsVal, _ := geometry.LookupCRS(crs)

	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()

	for i := range n {
		xv, xValid, err := x.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		yv, yValid, err := y.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !xValid || !yValid {
			b.AppendNull()
			continue
		}
		b.Append(geometry.WKB(geometry.Point{X: xv, Y: yv, CRSValue: crsVal}))
	}

	arr := b.NewArray()
	defer arr.Release()
	field := GeometryField("geometry", crs)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	return NewSeries(col), nil
}

// PointsFromXYZ is the 3D variant of PointsFromXY. z must be numeric
// and the same length as x and y; rows with a null z produce null
// geometries even if x and y are valid.
//
// The resulting Point geometries carry HasZ=true so downstream WKB
// encoding emits XYZ type codes (1001..) rather than 2D (1..).
func PointsFromXYZ(x, y, z Series, crs int32) (Series, error) {
	n := x.Len()
	if y.Len() != n || z.Len() != n {
		return Series{}, fmt.Errorf("%w: x=%d y=%d z=%d",
			ErrColumnLenMismatch, n, y.Len(), z.Len())
	}
	if !x.isNumeric() || !y.isNumeric() || !z.isNumeric() {
		return Series{}, fmt.Errorf("PointsFromXYZ: %w (all of x, y, z must be numeric)",
			ErrNotNumeric)
	}

	crsVal, _ := geometry.LookupCRS(crs)

	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()

	for i := range n {
		xv, xValid, err := x.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		yv, yValid, err := y.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		zv, zValid, err := z.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !xValid || !yValid || !zValid {
			b.AppendNull()
			continue
		}
		b.Append(geometry.WKB(geometry.Point{
			X: xv, Y: yv, Z: zv, HasZ: true, CRSValue: crsVal,
		}))
	}

	arr := b.NewArray()
	defer arr.Release()
	field := GeometryField("geometry", crs)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	return NewSeries(col), nil
}

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
	if !crs.Zero() && crs.Projected() {
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

// GeomCrossesAntimeridian returns a Boolean Series where row i is true
// if row i's geometry has any adjacent-vertex pair with |Δlon| > 180°,
// i.e. the edge between them wraps around the ±180° meridian. Only
// meaningful for geographic-CRS inputs; projected-CRS series always
// return false per row. Null rows produce null.
func (s Series) GeomCrossesAntimeridian() (Series, error) {
	return geomBoolFnOp(s, "_crosses_antimeridian", geometry.CrossesAntimeridian)
}

// GeomSplitAtAntimeridian returns a geometry Series where every
// antimeridian-crossing row is replaced by its split components
// (Polygon → MultiPolygon, LineString → MultiLineString). Non-crossing
// rows pass through unchanged. Points always pass through. Nulls stay
// null. See geometry.SplitAtAntimeridian for the crossing detection
// and interpolation semantics.
func (s Series) GeomSplitAtAntimeridian() (Series, error) {
	return geomTransformOp(s, "_split_antimeridian", func(g geometry.Geometry) (geometry.Geometry, error) {
		return geometry.SplitAtAntimeridian(g)
	})
}

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

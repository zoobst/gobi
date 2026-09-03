package gobi

import (
	"fmt"

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
	if crs.Projected {
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
	if crs.Projected {
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

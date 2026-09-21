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

// GeomDistance3D returns a Float64 Series where row i is the 3D
// distance from row i's geometry to other, in the requested unit.
// Null inputs produce null outputs.
//
// Dispatches on CRS to match the shape of Point.Distance3D:
//
//   - Projected CRS: 3D Cartesian Euclidean in the CRS's linear
//     unit.
//   - Geographic CRS: ECEF Euclidean via the WGS84 ellipsoid
//     (through-Earth chord distance) in meters.
//   - Zero CRS: 3D Cartesian in the caller's units.
//
// `other` MUST be a Point / PointZ; any other type returns an
// error at the top of the call. Column-side rows that aren't
// Points (or are 2D Points when the column is CRS-tagged
// geographic — no Z coordinate to work with) produce null output
// rows rather than an error, matching the per-row null-propagation
// contract of the surrounding Series geom family. A full 3D
// min-distance kernel (Point-to-LineString, Point-to-Polygon in
// 3D) is future work.
//
// # SoA fast path
//
// The column is walked once; per row the (x, y, z) coordinates are
// read directly from the WKB header + coord bytes (no ParseWKB,
// no []Point materialization) into pre-allocated slabs. `other`'s
// coordinates broadcast into a companion slab, and a single
// slab-kernel call produces the output column. Zero per-row heap
// allocation for the math itself; the only per-column alloc is
// the fixed set of scratch slabs (pooled would be a follow-up).
func (s Series) GeomDistance3D(other geometry.Geometry, u geometry.Unit) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	if other == nil {
		return Series{}, fmt.Errorf("geometry: nil `other` in GeomDistance3D")
	}
	otherPt, ok := other.(geometry.Point)
	if !ok {
		return Series{}, fmt.Errorf("%w: GeomDistance3D requires a Point `other`, got %T",
			geometry.ErrTypeMismatch, other)
	}
	if !otherPt.HasZ {
		return Series{}, fmt.Errorf("%w: GeomDistance3D requires a 3D `other` Point",
			geometry.ErrTypeMismatch)
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	// Comma-ok on the type assertion: attachCRS always returns
	// the same concrete type in current code, but the assertion
	// would panic silently if that ever changed. Fall back to
	// the caller-provided `otherPt` (no CRS attached) on failure —
	// the downstream slab kernel doesn't consult the CRS after
	// this point.
	if p, ok := attachCRS(otherPt, crs).(geometry.Point); ok {
		otherPt = p
	}

	geographic := !crs.Projected && !crs.Zero()
	perM, err := geometry.MetersPerUnit(u)
	if err != nil {
		return Series{}, err
	}

	// Column pass — collect (lon/x, lat/y, alt/z) into slabs, then
	// invoke the slab kernel once. Symmetric between projected and
	// geographic modes; only the kernel choice differs.
	n := s.Len()
	xs := make([]float64, 0, n)
	ys := make([]float64, 0, n)
	zs := make([]float64, 0, n)
	validMask := make([]bool, 0, n) // parallel: false for null / non-Point rows.
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				xs = append(xs, 0)
				ys = append(ys, 0)
				zs = append(zs, 0)
				validMask = append(validMask, false)
				continue
			}
			x, y, z, valid, err := geometry.PointXYZFromWKB(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			xs = append(xs, x)
			ys = append(ys, y)
			zs = append(zs, z)
			validMask = append(validMask, valid)
		}
	}
	nRows := len(xs)
	out := make([]float64, nRows)
	if geographic {
		// Scalar-target kernel — target's ECEF triple computed once
		// on the stack; no 3 * N broadcast-slab alloc.
		var scratch geometry.ECEFScratch
		geometry.Distance3DGeodesicFromSlabsToPoint(xs, ys, zs, otherPt.X, otherPt.Y, otherPt.Z, out, &scratch)
	} else {
		// Projected / Zero CRS — Cartesian scalar-target kernel.
		geometry.Distance3DProjectedFromSlabsToPoint(xs, ys, zs, otherPt.X, otherPt.Y, otherPt.Z, out)
	}

	b := array.NewFloat64Builder(memory.DefaultAllocator)
	defer b.Release()
	for i, v := range out {
		if !validMask[i] {
			b.AppendNull()
			continue
		}
		b.Append(v / perM)
	}
	return newSeriesFromArray(s.name+"_distance_3d", b.NewArray()), nil
}

// GeomLength3D returns a Float64 Series where row i is the 3D arc
// length of row i's LineString / LineStringZ (or MultiLineString),
// in the requested unit. Null / non-LineString rows produce null.
//
// Projected CRS: sum of segment Cartesian 3D distances.
// Geographic CRS: sum of ECEF Euclidean segment distances (meters).
// Zero CRS: Cartesian in caller's units.
//
// Same shape as GeomLength (2D) with the Z axis mixed in per the
// CRS-appropriate flavor.
func (s Series) GeomLength3D(u geometry.Unit) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	epsg := geometryCRSFromField(s.field)
	crs, _ := geometry.LookupCRS(epsg)
	geographic := !crs.Projected && !crs.Zero()
	perM, err := geometry.MetersPerUnit(u)
	if err != nil {
		return Series{}, err
	}
	// Hoisted scratch buffer — one alloc for the whole column
	// rather than per-row. `reset` inside the geographic path
	// grows the slabs to fit the longest vertex slab we see;
	// subsequent rows reuse the backing array.
	var scratch geometry.ECEFScratch
	return geomFloat64Op(s, s.name+"_length_3d", func(g geometry.Geometry) (float64, bool, error) {
		total, ok, err := lengthOfLineString3D(g, geographic, &scratch)
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return 0, false, nil
		}
		return total / perM, true, nil
	})
}

// GeomZ returns a Float64 Series where row i is the Z (altitude)
// coordinate of row i's Point. Null / non-Point / 2D-only rows
// produce null.
func (s Series) GeomZ() (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	return geomFloat64OpWKB(s, s.name+"_z", func(wkb []byte) (float64, bool, error) {
		typ, hasZ, err := geometry.WKBTypeCode(wkb)
		if err != nil {
			return 0, false, err
		}
		if geometry.Type(typ) != geometry.TypePoint || !hasZ {
			return 0, false, nil
		}
		_, _, z, valid, err := geometry.PointXYZFromWKB(wkb)
		if err != nil {
			return 0, false, err
		}
		if !valid {
			return 0, false, nil
		}
		return z, true, nil
	})
}

// GeomForce2D returns a geometry Series with the Z coordinate
// dropped and HasZ cleared on every row. Idempotent on already-2D
// columns. Mirrors PostGIS ST_Force2D. Null rows pass through as
// null.
//
// Implemented via ParseWKB → force-2D → WKB rewrite per row.
// SoA byte-stream shortcut is a follow-up; the ParseWKB path is
// correct and small.
func (s Series) GeomForce2D() (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	return geomGeomOp(s, "_force2d", func(g geometry.Geometry) (geometry.Geometry, error) {
		return forceGeometry2D(g), nil
	})
}

// GeomForceZ returns a geometry Series with Z set to alt and
// HasZ = true on every row. Overwrites any existing Z value.
// Mirrors PostGIS ST_Force3D / ST_Force3DZ.
func (s Series) GeomForceZ(alt float64) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	return geomGeomOp(s, "_forcez", func(g geometry.Geometry) (geometry.Geometry, error) {
		return forceGeometryZ(g, alt), nil
	})
}

// -----------------------------------------------------------------------------
// Helpers (unexported).
// -----------------------------------------------------------------------------

// lengthOfLineString3D returns the 3D length of a LineString or
// MultiLineString, respecting the CRS-appropriate math. Non-line
// geometries return ok=false. `scratch` is caller-hoisted so a
// full column pass shares one ECEF scratch across every row; nil
// falls back to per-call allocation.
func lengthOfLineString3D(g geometry.Geometry, geographic bool, scratch *geometry.ECEFScratch) (float64, bool, error) {
	switch t := g.(type) {
	case geometry.LineString:
		xs, ys, zs := xyzFromPoints(t.Points, t.HasZ)
		if geographic {
			return geometry.LineString3DGeodesicLengthFromXYZ(xs, ys, zs, scratch), true, nil
		}
		return geometry.LineString3DProjectedLengthFromXYZ(xs, ys, zs), true, nil
	case geometry.MultiLineString:
		var total float64
		for _, l := range t.Lines {
			xs, ys, zs := xyzFromPoints(l.Points, l.HasZ || t.HasZ)
			if geographic {
				total += geometry.LineString3DGeodesicLengthFromXYZ(xs, ys, zs, scratch)
			} else {
				total += geometry.LineString3DProjectedLengthFromXYZ(xs, ys, zs)
			}
		}
		return total, true, nil
	default:
		return 0, false, nil
	}
}

// xyzFromPoints materializes parallel Xs/Ys/Zs slabs from an AoS
// []Point. Called on the AoS side of the GeomLength3D path; the
// slab kernel doesn't accept []Point directly on purpose (SoA
// discipline). When hasZ is false, zs is a same-length slab of
// zeros — the length kernel then produces a pure 2D length.
func xyzFromPoints(pts []geometry.Point, hasZ bool) (xs, ys, zs []float64) {
	n := len(pts)
	xs = make([]float64, n)
	ys = make([]float64, n)
	zs = make([]float64, n)
	for i, p := range pts {
		xs[i] = p.X
		ys[i] = p.Y
		if hasZ {
			zs[i] = p.Z
		}
	}
	return xs, ys, zs
}

// forceGeometry2D returns g with Z dropped from every coordinate.
// Container HasZ flags cleared. Types not carrying coordinates
// (empty geometries, unrecognized) pass through unchanged.
//
// Every branch defensively copies its point slabs before mutation
// so the returned geometry never aliases the caller's backing
// arrays. Today ParseWKB always hands the Series driver a
// freshly-allocated geometry, so an in-place mutate wouldn't be
// user-visible, but that invariant would break the moment WKB
// parsing gained a cache or interning — so the defensive copy is
// permanent, not an optimization to skip once we're "sure it's
// safe."
func forceGeometry2D(g geometry.Geometry) geometry.Geometry {
	switch t := g.(type) {
	case geometry.Point:
		return t.Force2D()
	case geometry.MultiPoint:
		t.Points = force2DPoints(t.Points)
		t.HasZ = false
		return t
	case geometry.LineString:
		t.Points = force2DPoints(t.Points)
		t.HasZ = false
		return t
	case geometry.MultiLineString:
		lines := make([]geometry.LineString, len(t.Lines))
		for i, l := range t.Lines {
			l.Points = force2DPoints(l.Points)
			l.HasZ = false
			lines[i] = l
		}
		t.Lines = lines
		t.HasZ = false
		return t
	case geometry.Polygon:
		t.Rings = force2DRings(t.Rings)
		t.HasZ = false
		return t
	case geometry.MultiPolygon:
		polys := make([]geometry.Polygon, len(t.Polygons))
		for i, poly := range t.Polygons {
			poly.Rings = force2DRings(poly.Rings)
			poly.HasZ = false
			polys[i] = poly
		}
		t.Polygons = polys
		t.HasZ = false
		return t
	default:
		return g
	}
}

// forceGeometryZ returns g with Z = alt and HasZ = true on every
// coordinate. Overwrites any existing Z. Defensively copies every
// point slab — see forceGeometry2D for the aliasing rationale.
func forceGeometryZ(g geometry.Geometry, alt float64) geometry.Geometry {
	switch t := g.(type) {
	case geometry.Point:
		return t.ForceZ(alt)
	case geometry.MultiPoint:
		t.Points = forceZPoints(t.Points, alt)
		t.HasZ = true
		return t
	case geometry.LineString:
		t.Points = forceZPoints(t.Points, alt)
		t.HasZ = true
		return t
	case geometry.MultiLineString:
		lines := make([]geometry.LineString, len(t.Lines))
		for i, l := range t.Lines {
			l.Points = forceZPoints(l.Points, alt)
			l.HasZ = true
			lines[i] = l
		}
		t.Lines = lines
		t.HasZ = true
		return t
	case geometry.Polygon:
		t.Rings = forceZRings(t.Rings, alt)
		t.HasZ = true
		return t
	case geometry.MultiPolygon:
		polys := make([]geometry.Polygon, len(t.Polygons))
		for i, poly := range t.Polygons {
			poly.Rings = forceZRings(poly.Rings, alt)
			poly.HasZ = true
			polys[i] = poly
		}
		t.Polygons = polys
		t.HasZ = true
		return t
	default:
		return g
	}
}

// force2DPoints / forceZPoints / force2DRings / forceZRings —
// slab-level helpers that return fresh []Point / [][]Point so
// the caller's backing arrays are never touched.
func force2DPoints(in []geometry.Point) []geometry.Point {
	out := make([]geometry.Point, len(in))
	for i, p := range in {
		out[i] = p.Force2D()
	}
	return out
}

func forceZPoints(in []geometry.Point, alt float64) []geometry.Point {
	out := make([]geometry.Point, len(in))
	for i, p := range in {
		out[i] = p.ForceZ(alt)
	}
	return out
}

func force2DRings(in [][]geometry.Point) [][]geometry.Point {
	out := make([][]geometry.Point, len(in))
	for i, ring := range in {
		out[i] = force2DPoints(ring)
	}
	return out
}

func forceZRings(in [][]geometry.Point, alt float64) [][]geometry.Point {
	out := make([][]geometry.Point, len(in))
	for i, ring := range in {
		out[i] = forceZPoints(ring, alt)
	}
	return out
}

// geomGeomOp is a shared driver for row-by-row geometry → geometry
// series ops. Emits a Binary column of WKB, preserving null rows.
func geomGeomOp(s Series, nameSuffix string, fn func(geometry.Geometry) (geometry.Geometry, error)) (Series, error) {
	epsg := geometryCRSFromField(s.field)
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
			out, err := fn(g)
			if err != nil {
				return Series{}, err
			}
			b.Append(geometry.WKB(out))
		}
	}
	field := GeometryField(s.name+nameSuffix, epsg)
	return SeriesFromArray(field, b.NewArray()), nil
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

// GeomIntersects3D returns a Boolean Series where row i is true
// when row i's Point lies inside (or on the boundary of) the given
// prism. Null / non-Point rows produce null.
//
// The scalar RHS is a geometry.ExtrudedPolygon (2D footprint × Z
// range), not a generic Geometry — day-one 3D predicates are
// Point × Prism only. Other combinations (LineString-3D vs prism,
// prism vs prism, arbitrary polyhedra) are follow-up scope pending
// a 3D shape codec.
//
// # SoA fast path
//
// The column is walked once; per row (x, y, z) is read directly
// from the WKB header + coord bytes into slabs, then a single
// PointsInPrismFromXYZ call runs the whole batch (2D PIP on cached
// ring views + vectorized Z band-pass). No []Point materialization.
//
// Column-level 3D buffer output (Series.GeomBuffer3D) is not
// wired at the Series level yet — 3D shapes (Sphere, Capsule,
// buffered ExtrudedPolygon) don't have a stable Arrow encoding.
// Callers who need column-level 3D buffering should compose the
// scalar Buffer3DPoint / Buffer3DLineString primitives outside
// the Series API.
func (s Series) GeomIntersects3D(prism geometry.ExtrudedPolygon) (Series, error) {
	if !s.IsGeometry() {
		return Series{}, ErrNotGeometry
	}
	// Column pass — collect (x, y, z) slabs, then a single
	// slab-kernel call. Matches the GeomDistance3D shape.
	n := s.Len()
	xs := make([]float64, 0, n)
	ys := make([]float64, 0, n)
	zs := make([]float64, 0, n)
	validMask := make([]bool, 0, n)
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return Series{}, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				xs = append(xs, 0)
				ys = append(ys, 0)
				zs = append(zs, 0)
				validMask = append(validMask, false)
				continue
			}
			x, y, z, valid, err := geometry.PointXYZFromWKB(bin.Value(i))
			if err != nil {
				return Series{}, err
			}
			xs = append(xs, x)
			ys = append(ys, y)
			zs = append(zs, z)
			validMask = append(validMask, valid)
		}
	}
	nRows := len(xs)
	hits := make([]bool, nRows)
	geometry.PointsInPrismFromXYZ(xs, ys, zs, prism, hits)

	pool := memory.DefaultAllocator
	bb := array.NewBooleanBuilder(pool)
	defer bb.Release()
	for i, v := range hits {
		if !validMask[i] {
			bb.AppendNull()
			continue
		}
		bb.Append(v)
	}
	field := arrow.Field{Name: s.name + "_intersects_3d", Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, bb.NewArray()), nil
}

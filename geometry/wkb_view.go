package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// LineStringViewFromWKB parses a LineString or LineStringZ WKB
// directly into a PointsView, skipping the intermediate []Point
// slice that ParseWKB(data).View() would allocate. Type codes
// other than LineString / LineStringZ return ErrTypeMismatch.
//
// This is the WKB-facing sibling of LineString.View() for callers
// materializing SoA views from bytes (Hilbert index over stored
// WKB, spatial-predicate refine loops, streamed-parquet consumers).
// Two allocations: the Xs and Ys slabs sized to the vertex count.
// One extra Zs slab when the WKB carries Z.
//
// CRS is left unset on the returned view — the WKB blob doesn't
// carry CRS. Callers embedding CRS via schema/annotation must
// attach it themselves.
func LineStringViewFromWKB(data []byte) (PointsView, error) {
	if len(data) < 5 {
		return PointsView{}, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return PointsView{}, err
	}
	typ := bo.Uint32(data[1:5])
	switch typ {
	case wkbLineString:
		return lineStringViewBody(data[5:], bo, false)
	case wkbLineStringZ:
		return lineStringViewBody(data[5:], bo, true)
	default:
		return PointsView{}, fmt.Errorf("%w: expected LineString, got %d",
			ErrTypeMismatch, typ)
	}
}

// PolygonRingViewsFromWKB parses a Polygon or PolygonZ WKB
// directly into a []PointsView with one view per ring
// (exterior at index 0, then holes). Skips the ParseWKB
// intermediate [][]Point allocation.
//
// This is the primary Slice-10 entry point for PreparedGeometry
// callers — collapses ParseWKB + Polygon.RingViews() into a
// single byte-stream pass. Wired into PrepareFromWKB.
func PolygonRingViewsFromWKB(data []byte) ([]PointsView, error) {
	if len(data) < 5 {
		return nil, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return nil, err
	}
	typ := bo.Uint32(data[1:5])
	switch typ {
	case wkbPolygon:
		v, _, err := polygonRingViewsBody(data[5:], bo, false)
		return v, err
	case wkbPolygonZ:
		v, _, err := polygonRingViewsBody(data[5:], bo, true)
		return v, err
	default:
		return nil, fmt.Errorf("%w: expected Polygon, got %d",
			ErrTypeMismatch, typ)
	}
}

// MultiPolygonRingViewsFromWKB parses a MultiPolygon or
// MultiPolygonZ WKB directly into a [][]PointsView with one
// []PointsView per sub-polygon. Same layout as
// MultiPolygon.PolygonRingViews() but no []Point intermediate.
func MultiPolygonRingViewsFromWKB(data []byte) ([][]PointsView, error) {
	if len(data) < 5 {
		return nil, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return nil, err
	}
	typ := bo.Uint32(data[1:5])
	var hasZ bool
	switch typ {
	case wkbMultiPolygon:
	case wkbMultiPolygonZ:
		hasZ = true
	default:
		return nil, fmt.Errorf("%w: expected MultiPolygon, got %d",
			ErrTypeMismatch, typ)
	}
	body := data[5:]
	if len(body) < 4 {
		return nil, ErrShortWKB
	}
	n := int(bo.Uint32(body[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	out := make([][]PointsView, n)
	for i := range n {
		if len(body) < off+5 {
			return nil, ErrShortWKB
		}
		innerBO, err := byteOrder(body[off])
		if err != nil {
			return nil, err
		}
		if innerBO.Uint32(body[off+1:off+5]) != innerType {
			return nil, fmt.Errorf("%w: expected Polygon inside MultiPolygon",
				ErrTypeMismatch)
		}
		rings, sz, err := polygonRingViewsBody(body[off+5:], innerBO, hasZ)
		if err != nil {
			return nil, err
		}
		out[i] = rings
		off += 5 + sz
	}
	return out, nil
}

// lineStringViewBody parses the coord run of a LineString body
// (data starts at the 4-byte vertex count). Allocates fresh
// Xs / Ys (and Zs if hasZ) slabs sized to the vertex count.
func lineStringViewBody(data []byte, bo binary.ByteOrder, hasZ bool) (PointsView, error) {
	if len(data) < 4 {
		return PointsView{}, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return PointsView{}, ErrShortWKB
	}
	v := PointsView{
		Xs:   make([]float64, n),
		Ys:   make([]float64, n),
		HasZ: hasZ,
	}
	if hasZ {
		v.Zs = make([]float64, n)
	}
	base := data[4:]
	if hasZ {
		for i := range n {
			off := i * cs
			v.Xs[i] = math.Float64frombits(bo.Uint64(base[off : off+8]))
			v.Ys[i] = math.Float64frombits(bo.Uint64(base[off+8 : off+16]))
			v.Zs[i] = math.Float64frombits(bo.Uint64(base[off+16 : off+24]))
		}
	} else {
		for i := range n {
			off := i * cs
			v.Xs[i] = math.Float64frombits(bo.Uint64(base[off : off+8]))
			v.Ys[i] = math.Float64frombits(bo.Uint64(base[off+8 : off+16]))
		}
	}
	return v, nil
}

// polygonRingViewsBody parses the numRings + ring coords portion
// of a Polygon body. Returns the ring views, bytes consumed, and
// any error. Used by both PolygonRingViewsFromWKB and
// MultiPolygonRingViewsFromWKB's inner loop.
func polygonRingViewsBody(data []byte, bo binary.ByteOrder, hasZ bool) ([]PointsView, int, error) {
	if len(data) < 4 {
		return nil, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	rings := make([]PointsView, numRings)
	for r := range numRings {
		if len(data) < off+4 {
			return nil, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return nil, 0, ErrShortWKB
		}
		v := PointsView{
			Xs:   make([]float64, nPts),
			Ys:   make([]float64, nPts),
			HasZ: hasZ,
		}
		if hasZ {
			v.Zs = make([]float64, nPts)
			for i := range nPts {
				base := off + i*cs
				v.Xs[i] = math.Float64frombits(bo.Uint64(data[base : base+8]))
				v.Ys[i] = math.Float64frombits(bo.Uint64(data[base+8 : base+16]))
				v.Zs[i] = math.Float64frombits(bo.Uint64(data[base+16 : base+24]))
			}
		} else {
			for i := range nPts {
				base := off + i*cs
				v.Xs[i] = math.Float64frombits(bo.Uint64(data[base : base+8]))
				v.Ys[i] = math.Float64frombits(bo.Uint64(data[base+8 : base+16]))
			}
		}
		rings[r] = v
		off += nPts * cs
	}
	return rings, off, nil
}

// PrepareFromWKB builds a PreparedGeometry from a WKB blob using
// the byte-stream direct-parse (Slice 10). Single WKB walk: the
// slabs are populated first, then the AoS Polygon / MultiPolygon
// needed for TestPrepared's non-fast-path fall-through is
// materialized from the slabs (a cheap float64→Point copy, not a
// second WKB parse). Non-polygon geometry types fall through to
// `Prepare(ParseWKB(data))` — they don't have cached slabs today,
// so there's no direct-parse win to capture.
//
// Bounds are derived from the slabs (BoundsFromXY per ring)
// rather than a third byte-stream pass. Matches g.Bounds() exactly.
func PrepareFromWKB(data []byte) (PreparedGeometry, error) {
	if len(data) < 5 {
		return PreparedGeometry{}, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return PreparedGeometry{}, err
	}
	typ := bo.Uint32(data[1:5])
	switch typ {
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		rings, err := PolygonRingViewsFromWKB(data)
		if err != nil {
			return PreparedGeometry{}, err
		}
		poly := polygonFromRingViews(rings, hasZ)
		return PreparedGeometry{
			G:         poly,
			Bounds:    boundsFromRingViews(rings),
			polyRings: rings,
		}, nil
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		polys, err := MultiPolygonRingViewsFromWKB(data)
		if err != nil {
			return PreparedGeometry{}, err
		}
		mp := multiPolygonFromRingViews(polys, hasZ)
		return PreparedGeometry{
			G:              mp,
			Bounds:         boundsFromMultiPolygonViews(polys),
			multiPolyRings: polys,
		}, nil
	default:
		g, err := ParseWKB(data)
		if err != nil {
			return PreparedGeometry{}, err
		}
		return Prepare(g), nil
	}
}

// polygonFromRingViews materializes an AoS Polygon from
// pre-parsed ring views. Used by PrepareFromWKB to keep the
// TestPrepared fall-through's Test(a.G, b.G) contract without
// paying a second WKB byte walk. Copy cost is O(total-vertices)
// — same shape as `PointsView.View()` in reverse.
func polygonFromRingViews(views []PointsView, hasZ bool) Polygon {
	rings := make([][]Point, len(views))
	for i, v := range views {
		pts := make([]Point, v.Len())
		if hasZ {
			for j := range pts {
				pts[j] = Point{X: v.Xs[j], Y: v.Ys[j], Z: v.Zs[j], HasZ: true}
			}
		} else {
			for j := range pts {
				pts[j] = Point{X: v.Xs[j], Y: v.Ys[j]}
			}
		}
		rings[i] = pts
	}
	return Polygon{Rings: rings, HasZ: hasZ}
}

// multiPolygonFromRingViews materializes an AoS MultiPolygon from
// pre-parsed per-polygon ring views.
func multiPolygonFromRingViews(polys [][]PointsView, hasZ bool) MultiPolygon {
	out := make([]Polygon, len(polys))
	for i, rings := range polys {
		out[i] = polygonFromRingViews(rings, hasZ)
	}
	return MultiPolygon{Polygons: out, HasZ: hasZ}
}

// boundsFromRingViews returns the union bbox of every ring view.
// Matches Polygon.Bounds() (which delegates to the exterior +
// each hole — but hole bounds are always ⊂ exterior bounds so
// exterior-only is equivalent; walk all rings for parity).
func boundsFromRingViews(views []PointsView) Bounds {
	if len(views) == 0 {
		return EmptyBounds()
	}
	b := views[0].Bounds()
	for _, v := range views[1:] {
		b = b.Union(v.Bounds())
	}
	return b
}

// boundsFromMultiPolygonViews returns the union bbox across every
// sub-polygon's rings.
func boundsFromMultiPolygonViews(polys [][]PointsView) Bounds {
	b := EmptyBounds()
	for _, rings := range polys {
		b = b.Union(boundsFromRingViews(rings))
	}
	return b
}

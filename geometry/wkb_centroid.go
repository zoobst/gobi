package geometry

import (
	"encoding/binary"
	"fmt"
	"math"
)

// CentroidFromWKB extracts the geometry's centroid from a WKB blob
// without materializing intermediate `[]Point` / `Polygon` / etc.
// structs. Walks the byte stream once, running per-type
// centroid accumulators against the raw coordinate pairs.
//
// This is Slice 3's SoA fast path for the `SortByHilbert` write path.
// The two-pass `SortByHilbertWith` and the fused
// `HilbertSortWithCovering` both currently parse every row's WKB
// into a full geometry, call `.Centroid()`, and discard the
// geometry — exactly the shape BoundsFromWKB (Slice 2) already
// targets on the bbox side.
//
// # Semantics vs g.Centroid()
//
// The returned Point matches `ParseWKB(data).Centroid()` on
// Point, LineString, Polygon, MultiPoint, and MultiLineString.
// Divergences:
//
//   - MultiPolygon centroid uses bbox-center. The AoS
//     `MultiPolygon.Centroid()` weights each sub-polygon by its
//     geodesic area (`Area(UnitMeters)`), which requires CRS
//     context this scanner doesn't carry from the WKB alone.
//     bbox-center is locality-preserving and CRS-independent —
//     enough for spatial-sort use cases (Hilbert-index inputs)
//     which don't care about geodesic accuracy.
//
//   - GeometryCollection centroid uses bbox-center. This matches
//     the AoS implementation exactly (see collection.go —
//     `GeometryCollection.Centroid()` already returns bbox-center).
//
// The returned Point's CRS is unset — the WKB blob doesn't carry
// CRS. Callers embedding CRS via a schema/annotation must set it
// themselves.
//
// Zero-allocation on well-formed input.
func CentroidFromWKB(data []byte) (Point, error) {
	c, _, err := centroidAndBoundsFromWKB(data, false)
	return c, err
}

// CentroidAndBoundsFromWKB is the fused-scan variant: computes the
// centroid AND the 2D bounding box in a single byte-stream pass.
// The centroid semantics match CentroidFromWKB; the bounds
// semantics match BoundsFromWKB. Callers who need both (e.g. the
// fused HilbertSortWithCovering write path) save a full second
// byte-scan.
func CentroidAndBoundsFromWKB(data []byte) (Point, Bounds, error) {
	c, b, err := centroidAndBoundsFromWKB(data, true)
	return c, b, err
}

func centroidAndBoundsFromWKB(data []byte, wantBounds bool) (Point, Bounds, error) {
	// Use +/-Inf sentinels for the bounds accumulator so the hot-
	// loop extendBoundsInline can compare-and-update without a
	// first-point branch. Same pattern as BoundsFromWKB.
	b := Bounds{
		MinX: math.Inf(1), MinY: math.Inf(1),
		MaxX: math.Inf(-1), MaxY: math.Inf(-1),
	}
	if len(data) < 5 {
		return Point{}, EmptyBounds(), ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return Point{}, EmptyBounds(), err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	var c Point
	switch typ {
	case wkbPoint, wkbPointZ:
		hasZ := typ == wkbPointZ
		c, _, err = scanPointCentroid(body, bo, hasZ, &b)
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		c, _, err = scanLineStringCentroid(body, bo, hasZ, &b)
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		c, _, err = scanPolygonCentroid(body, bo, hasZ, &b)
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		c, _, err = scanMultiPointCentroid(body, bo, hasZ, &b)
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		c, _, err = scanMultiLineStringCentroid(body, bo, hasZ, &b)
	case wkbMultiPolygon, wkbMultiPolygonZ:
		// bbox-center fallback: walk bounds for every coord in
		// every sub-polygon, then return the resulting bounds'
		// center. See docstring for the geodesic-Area rationale.
		hasZ := typ == wkbMultiPolygonZ
		_, err = scanMultiPolygonBounds(body, bo, hasZ, &b)
		if err == nil {
			c = bboxCenterOrZero(b)
		}
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		// Matches AoS GeometryCollection.Centroid — bbox-center.
		_, err = scanGeometryCollectionBounds(body, bo, &b)
		if err == nil {
			c = bboxCenterOrZero(b)
		}
	default:
		return Point{}, EmptyBounds(), fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
	if err != nil {
		return Point{}, EmptyBounds(), err
	}
	if !wantBounds {
		return c, Bounds{}, nil
	}
	if math.IsInf(b.MinX, 1) {
		return c, EmptyBounds(), nil
	}
	return c, b, nil
}

// bboxCenterOrZero returns the center of b, or the zero Point if b
// is empty (no coordinates were scanned). Matches AoS
// GeometryCollection.Centroid's empty-bounds behavior.
func bboxCenterOrZero(b Bounds) Point {
	if math.IsInf(b.MinX, 1) {
		return Point{}
	}
	return Point{X: (b.MinX + b.MaxX) / 2, Y: (b.MinY + b.MaxY) / 2}
}

// scanPointCentroid reads exactly one XY (or XYZ) coord and returns
// it as the "centroid" (Point.Centroid returns itself). Extends b.
func scanPointCentroid(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (Point, int, error) {
	need := coordSize(hasZ)
	if len(data) < need {
		return Point{}, 0, ErrShortWKB
	}
	x := math.Float64frombits(bo.Uint64(data[0:8]))
	y := math.Float64frombits(bo.Uint64(data[8:16]))
	extendBoundsInline(b, x, y)
	return Point{X: x, Y: y}, need, nil
}

// scanLineStringCentroid computes the length-weighted midpoint
// centroid matching LineString.Centroid semantics. Returns the
// centroid + bytes consumed (including the 4-byte length prefix).
func scanLineStringCentroid(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (Point, int, error) {
	if len(data) < 4 {
		return Point{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return Point{}, 0, ErrShortWKB
	}
	c, err := lineStringCentroidFromCoords(data[4:], bo, n, cs, b)
	return c, 4 + n*cs, err
}

// lineStringCentroidFromCoords computes the length-weighted
// midpoint centroid over a coordinate slab (no 4-byte length
// prefix). Extracted so scanMultiLineStringCentroid can reuse the
// per-line body without re-parsing the length prefix.
//
// Formula matches LineString.Centroid exactly:
//
//	cx = sum(midpoint_x * seg_len) / sum(seg_len)
//	cy = sum(midpoint_y * seg_len) / sum(seg_len)
//
// where each seg_len is the Euclidean length of a segment between
// consecutive points. Empty → zero Point. Single point → that
// point. Zero total length (all points coincident) → first point.
func lineStringCentroidFromCoords(data []byte, bo binary.ByteOrder, n, cs int, b *Bounds) (Point, error) {
	if n == 0 {
		return Point{}, nil
	}
	fx := math.Float64frombits(bo.Uint64(data[0:8]))
	fy := math.Float64frombits(bo.Uint64(data[8:16]))
	extendBoundsInline(b, fx, fy)
	if n == 1 {
		return Point{X: fx, Y: fy}, nil
	}
	var cx, cy, total float64
	px, py := fx, fy
	for i := 1; i < n; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(data[off : off+8]))
		y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
		extendBoundsInline(b, x, y)
		dx := x - px
		dy := y - py
		segLen := math.Sqrt(dx*dx + dy*dy)
		if segLen != 0 {
			mx := (px + x) / 2
			my := (py + y) / 2
			cx += mx * segLen
			cy += my * segLen
			total += segLen
		}
		px, py = x, y
	}
	if total == 0 {
		return Point{X: fx, Y: fy}, nil
	}
	return Point{X: cx / total, Y: cy / total}, nil
}

// scanPolygonCentroid computes the exterior-ring shoelace-formula
// area-weighted centroid matching Polygon.Centroid exactly. Also
// walks interior rings to keep the bounds accumulator correct.
func scanPolygonCentroid(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (Point, int, error) {
	if len(data) < 4 {
		return Point{}, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	var (
		centroid  Point
		haveOuter bool
	)
	for r := range numRings {
		if len(data) < off+4 {
			return Point{}, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return Point{}, 0, ErrShortWKB
		}
		if r == 0 && nPts > 0 {
			centroid = polygonRingCentroid(data[off:], bo, nPts, cs, b)
			haveOuter = true
		} else {
			// Interior ring — bounds only.
			for i := range nPts {
				base := i * cs
				x := math.Float64frombits(bo.Uint64(data[off+base : off+base+8]))
				y := math.Float64frombits(bo.Uint64(data[off+base+8 : off+base+16]))
				extendBoundsInline(b, x, y)
			}
		}
		off += nPts * cs
	}
	if !haveOuter {
		return Point{}, off, nil
	}
	return centroid, off, nil
}

// polygonRingCentroid runs the shoelace-formula centroid over a
// single ring's coordinate slab. Handles closed vs. unclosed rings
// (matching closedRing()'s virtual-append behavior in
// Polygon.Centroid), zero-area fallback (arithmetic mean of segment
// starts), and empty-ring guard.
func polygonRingCentroid(data []byte, bo binary.ByteOrder, nPts, cs int, b *Bounds) Point {
	if nPts == 0 {
		return Point{}
	}
	fx := math.Float64frombits(bo.Uint64(data[0:8]))
	fy := math.Float64frombits(bo.Uint64(data[8:16]))
	extendBoundsInline(b, fx, fy)
	if nPts == 1 {
		// Matches the AoS pathological case: closedRing on a
		// length-1 ring stays length-1, n=0, division by zero → NaN.
		return Point{X: math.NaN(), Y: math.NaN()}
	}
	var (
		cx, cy, areaTwo float64
		sx, sy          float64
		px, py          = fx, fy
	)
	// Walk edges between consecutive points.
	for i := 1; i < nPts; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(data[off : off+8]))
		y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
		extendBoundsInline(b, x, y)
		cross := px*y - x*py
		areaTwo += cross
		cx += (px + x) * cross
		cy += (py + y) * cross
		sx += px
		sy += py
		px, py = x, y
	}
	// Handle the closing edge (last, first) iff ring wasn't
	// already closed. This mirrors closedRing's virtual append.
	var segCount int
	if px == fx && py == fy {
		segCount = nPts - 1
	} else {
		// Closing segment: (px, py) -> (fx, fy). Add it.
		cross := px*fy - fx*py
		areaTwo += cross
		cx += (px + fx) * cross
		cy += (py + fy) * cross
		sx += px
		sy += py
		segCount = nPts
	}
	if areaTwo == 0 {
		return Point{X: sx / float64(segCount), Y: sy / float64(segCount)}
	}
	return Point{X: cx / (3 * areaTwo), Y: cy / (3 * areaTwo)}
}

// scanMultiPointCentroid computes the arithmetic mean of the
// contained points, matching MultiPoint.Centroid.
func scanMultiPointCentroid(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (Point, int, error) {
	if len(data) < 4 {
		return Point{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	if n == 0 {
		return Point{}, off, nil
	}
	innerType := wkbPoint
	if hasZ {
		innerType = wkbPointZ
	}
	elemSize := 5 + coordSize(hasZ)
	var sx, sy float64
	for range n {
		if len(data) < off+elemSize {
			return Point{}, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return Point{}, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return Point{}, 0, fmt.Errorf("%w: expected Point inside MultiPoint", ErrTypeMismatch)
		}
		x := math.Float64frombits(innerBO.Uint64(data[off+5 : off+13]))
		y := math.Float64frombits(innerBO.Uint64(data[off+13 : off+21]))
		extendBoundsInline(b, x, y)
		sx += x
		sy += y
		off += elemSize
	}
	nn := float64(n)
	return Point{X: sx / nn, Y: sy / nn}, off, nil
}

// scanMultiLineStringCentroid computes the length-weighted
// combined centroid across all constituent lines, matching
// MultiLineString.Centroid. Two-pass per line (once to sum
// length, once to accumulate centroid) matches the AoS shape.
func scanMultiLineStringCentroid(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (Point, int, error) {
	if len(data) < 4 {
		return Point{}, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	cs := coordSize(hasZ)
	var cx, cy, totalLen float64
	for range n {
		if len(data) < off+5 {
			return Point{}, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return Point{}, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return Point{}, 0, fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		// Inner LineString length prefix + coords.
		if len(data) < off+5+4 {
			return Point{}, 0, ErrShortWKB
		}
		nPts := int(innerBO.Uint32(data[off+5 : off+9]))
		coordsOff := off + 9
		if len(data) < coordsOff+nPts*cs {
			return Point{}, 0, ErrShortWKB
		}
		if nPts < 2 {
			// AoS skips lines with < 2 points, but still extends
			// bounds via ParseWKB's decoded LineString. Match by
			// walking coords for bounds only.
			for i := range nPts {
				coordsBase := coordsOff + i*cs
				x := math.Float64frombits(innerBO.Uint64(data[coordsBase : coordsBase+8]))
				y := math.Float64frombits(innerBO.Uint64(data[coordsBase+8 : coordsBase+16]))
				extendBoundsInline(b, x, y)
			}
			off = coordsOff + nPts*cs
			continue
		}
		// First pass: compute this line's total length.
		var lineLen float64
		{
			px := math.Float64frombits(innerBO.Uint64(data[coordsOff : coordsOff+8]))
			py := math.Float64frombits(innerBO.Uint64(data[coordsOff+8 : coordsOff+16]))
			for i := 1; i < nPts; i++ {
				base := coordsOff + i*cs
				x := math.Float64frombits(innerBO.Uint64(data[base : base+8]))
				y := math.Float64frombits(innerBO.Uint64(data[base+8 : base+16]))
				dx := x - px
				dy := y - py
				lineLen += math.Sqrt(dx*dx + dy*dy)
				px, py = x, y
			}
		}
		if lineLen == 0 {
			// Bounds only, no centroid contribution.
			for i := range nPts {
				base := coordsOff + i*cs
				x := math.Float64frombits(innerBO.Uint64(data[base : base+8]))
				y := math.Float64frombits(innerBO.Uint64(data[base+8 : base+16]))
				extendBoundsInline(b, x, y)
			}
			off = coordsOff + nPts*cs
			continue
		}
		// Second pass: compute this line's centroid, extending
		// bounds along the way, then accumulate weighted.
		lc, err := lineStringCentroidFromCoords(data[coordsOff:], innerBO, nPts, cs, b)
		if err != nil {
			return Point{}, 0, err
		}
		cx += lc.X * lineLen
		cy += lc.Y * lineLen
		totalLen += lineLen
		off = coordsOff + nPts*cs
	}
	if totalLen == 0 {
		return Point{}, off, nil
	}
	return Point{X: cx / totalLen, Y: cy / totalLen}, off, nil
}

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

// BoundsFromWKB computes the axis-aligned 2D bounding box of a WKB
// geometry without materializing any intermediate Point / Polygon
// / etc. structs. Walks the byte stream once, tracking running
// min/max on X and Y.
//
// This is Slice 2's SoA fast path for bbox-only callers — the
// parquetio bbox-covering-column write path, GeoParquet metadata
// bounds compute, and any Filter/predicate hot path that only
// needs the bbox of each input geometry. Skips the O(n)
// `[]Point` allocation that ParseWKB does even though the caller
// throws the geometry away immediately after `.Bounds()`.
//
// Semantics match `ParseWKB(data).Bounds()` exactly:
//
//   - Empty geometries (empty LineString / Polygon /
//     GeometryCollection) return EmptyBounds().
//   - Z coordinates are ignored — matches the 2D Bounds type.
//   - MultiPoint / MultiLineString / MultiPolygon /
//     GeometryCollection recursively include every sub-geometry's
//     coordinates. Nested GeometryCollections are rejected inside
//     a GeometryCollection (matching ParseWKB).
//   - Byte-order and type-code handling mirrors ParseWKB;
//     unsupported type codes return ErrUnsupportedWKB.
//
// The scanner is per-call zero-allocation on well-formed input.
// Malformed input returns an error without leaking partial state.
func BoundsFromWKB(data []byte) (Bounds, error) {
	// Use ±Inf as the accumulator sentinel so the hot-loop
	// extendBoundsInline can compare-and-update without a branch
	// on "is this the first coord?". EmptyBounds's inverted-sentinel
	// form (1, 1, -1, -1) doesn't compose with a naive < comparison
	// on the first extend, so we normalize the entry state and
	// convert back to EmptyBounds if no coord was ever seen.
	b := Bounds{
		MinX: math.Inf(1), MinY: math.Inf(1),
		MaxX: math.Inf(-1), MaxY: math.Inf(-1),
	}
	if _, err := scanWKBBounds(data, &b, false); err != nil {
		return EmptyBounds(), err
	}
	if math.IsInf(b.MinX, 1) {
		// No coordinate was scanned — matches ParseWKB's empty
		// geometry semantics.
		return EmptyBounds(), nil
	}
	return b, nil
}

// scanWKBBounds consumes exactly one WKB geometry from the head of
// data and extends b with every coordinate pair found. Returns the
// number of bytes consumed. When inCollection is true, nested
// GeometryCollections are rejected (matching ParseWKB's rule).
func scanWKBBounds(data []byte, b *Bounds, inCollection bool) (int, error) {
	if len(data) < 5 {
		return 0, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return 0, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		return 5 + 16, scanPointBounds(body, bo, false, b)
	case wkbPointZ:
		return 5 + 24, scanPointBounds(body, bo, true, b)
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		size, err := scanLineStringBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		size, err := scanPolygonBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		size, err := scanMultiPointBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		size, err := scanMultiLineStringBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		size, err := scanMultiPolygonBounds(body, bo, hasZ, b)
		return 5 + size, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		if inCollection {
			return 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		size, err := scanGeometryCollectionBounds(body, bo, b)
		return 5 + size, err
	default:
		return 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

// scanPointBounds reads exactly one XY (or XYZ) coordinate from
// data and extends b. Point-typed WKB values always contribute a
// single coordinate — no empty-point encoding in OGC SFA 1.2.
func scanPointBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) error {
	need := coordSize(hasZ)
	if len(data) < need {
		return ErrShortWKB
	}
	x := math.Float64frombits(bo.Uint64(data[0:8]))
	y := math.Float64frombits(bo.Uint64(data[8:16]))
	extendBoundsInline(b, x, y)
	return nil
}

// scanLineStringBounds walks n coordinate tuples and extends b
// with each. Returns the total bytes consumed (including the
// 4-byte length prefix).
func scanLineStringBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	// Overflow-safe check: `n*cs` can wrap int on 32-bit for a
	// hostile 2^30 count. `coordsFit` uses division instead.
	if !coordsFit(len(data)-4, n, cs) {
		return 0, ErrShortWKB
	}
	base := data[4:]
	for i := range n {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(base[off : off+8]))
		y := math.Float64frombits(bo.Uint64(base[off+8 : off+16]))
		extendBoundsInline(b, x, y)
	}
	return 4 + n*cs, nil
}

// scanPolygonBounds walks numRings, each ring being a length-
// prefixed run of coordinates.
func scanPolygonBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	for range numRings {
		if len(data) < off+4 {
			return 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if !coordsFit(len(data)-off, nPts, cs) {
			return 0, ErrShortWKB
		}
		for i := range nPts {
			base := off + i*cs
			x := math.Float64frombits(bo.Uint64(data[base : base+8]))
			y := math.Float64frombits(bo.Uint64(data[base+8 : base+16]))
			extendBoundsInline(b, x, y)
		}
		off += nPts * cs
	}
	return off, nil
}

// scanMultiPointBounds — n inner Point WKBs, each with its own
// byte-order + type header.
func scanMultiPointBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPoint
	if hasZ {
		innerType = wkbPointZ
	}
	elemSize := 5 + coordSize(hasZ)
	for range n {
		if len(data) < off+elemSize {
			return 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, fmt.Errorf("%w: expected Point inside MultiPoint", ErrTypeMismatch)
		}
		if err := scanPointBounds(data[off+5:off+elemSize], innerBO, hasZ, b); err != nil {
			return 0, err
		}
		off += elemSize
	}
	return off, nil
}

// scanMultiLineStringBounds — n inner LineString WKBs.
func scanMultiLineStringBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	for range n {
		if len(data) < off+5 {
			return 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		sz, err := scanLineStringBounds(data[off+5:], innerBO, hasZ, b)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

// scanMultiPolygonBounds — n inner Polygon WKBs.
func scanMultiPolygonBounds(data []byte, bo binary.ByteOrder, hasZ bool, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	for range n {
		if len(data) < off+5 {
			return 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		sz, err := scanPolygonBounds(data[off+5:], innerBO, hasZ, b)
		if err != nil {
			return 0, err
		}
		off += 5 + sz
	}
	return off, nil
}

// scanGeometryCollectionBounds recurses into each member. The
// inCollection=true flag on the recursive call rejects nested
// GeometryCollections (matching ParseWKB).
func scanGeometryCollectionBounds(data []byte, bo binary.ByteOrder, b *Bounds) (int, error) {
	if len(data) < 4 {
		return 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	for range n {
		used, err := scanWKBBounds(data[off:], b, true)
		if err != nil {
			return 0, err
		}
		off += used
	}
	return off, nil
}

// extendBoundsInline is the hot-loop version of Bounds.Extend.
// Kept manually inlined to keep the SoA scanner competitive on
// the (bbox-per-row × N-rows-per-column) shape parquetio drives.
// Same semantics as (b *Bounds).Extend — first-coord case handled
// by EmptyBounds's sentinel (MinX > MaxX so both comparisons hit).
func extendBoundsInline(b *Bounds, x, y float64) {
	if x < b.MinX {
		b.MinX = x
	}
	if x > b.MaxX {
		b.MaxX = x
	}
	if y < b.MinY {
		b.MinY = y
	}
	if y > b.MaxY {
		b.MaxY = y
	}
}

// PlanarAreaFromWKB returns the absolute planar (XY) area of the
// WKB geometry in coord² without materializing intermediate
// []Point / Polygon structs. Polygon area is exterior − holes;
// MultiPolygon sums per-polygon area; GeometryCollection recurses.
// Non-areal types (Point, LineString, MultiPoint,
// MultiLineString) contribute 0.
//
// Semantics match `PlanarRingArea` composed via `Polygon.Area` on
// a projected CRS — i.e. the shoelace formula applied ring-by-
// ring with unclosed rings treated as if the first vertex were
// virtually appended. Unit conversion is left to the caller
// (multiply by `1 / (perM*perM)` for a projected CRS whose linear
// unit is meters). Geographic CRSes must fall back to the AoS
// spherical-excess path.
//
// Byte-order / type-code handling mirrors ParseWKB; malformed
// input returns an error. Zero-allocation on well-formed input.
func PlanarAreaFromWKB(data []byte) (float64, error) {
	total, _, err := scanWKBPlanarArea(data, false)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// scanWKBPlanarArea consumes exactly one WKB geometry from the head
// of data and returns (area, bytesConsumed, err). When
// inCollection is true, nested GeometryCollections are rejected.
func scanWKBPlanarArea(data []byte, inCollection bool) (float64, int, error) {
	if len(data) < 5 {
		return 0, 0, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return 0, 0, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		if len(body) < 16 {
			return 0, 0, ErrShortWKB
		}
		return 0, 5 + 16, nil
	case wkbPointZ:
		if len(body) < 24 {
			return 0, 0, ErrShortWKB
		}
		return 0, 5 + 24, nil
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		sz, err := skipLineString(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbPolygon, wkbPolygonZ:
		hasZ := typ == wkbPolygonZ
		area, sz, err := scanPolygonPlanarArea(body, bo, hasZ)
		return area, 5 + sz, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		sz, err := skipMultiPoint(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		sz, err := skipMultiLineString(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		area, sz, err := scanMultiPolygonPlanarArea(body, bo, hasZ)
		return area, 5 + sz, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		if inCollection {
			return 0, 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		area, sz, err := scanGeometryCollectionPlanarArea(body, bo)
		return area, 5 + sz, err
	default:
		return 0, 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

// scanPolygonPlanarArea returns |exterior| − Σ|holes| via the
// shoelace formula on each ring. Rings with fewer than 3 points
// contribute 0 (matches PlanarRingArea).
func scanPolygonPlanarArea(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	numRings := int(bo.Uint32(data[0:4]))
	off := 4
	cs := coordSize(hasZ)
	var area float64
	for r := range numRings {
		if len(data) < off+4 {
			return 0, 0, ErrShortWKB
		}
		nPts := int(bo.Uint32(data[off : off+4]))
		off += 4
		if len(data) < off+nPts*cs {
			return 0, 0, ErrShortWKB
		}
		ringArea := planarRingAreaFromCoords(data[off:], bo, nPts, cs)
		if r == 0 {
			area += ringArea
		} else {
			area -= ringArea
		}
		off += nPts * cs
	}
	if area < 0 {
		// AoS Polygon.Area returns exterior − holes without abs. The
		// scanner keeps the same behavior; ring winding is caller's
		// responsibility. But an unclosed-exterior + closed-holes
		// combo can produce a small negative on degenerate inputs —
		// clamp to 0 to preserve the "area is nonnegative" contract
		// on well-formed input (matches PlanarRingArea's `math.Abs`).
		//
		// This differs from Polygon.Area's exact numeric shape but
		// matches the semantic guarantee callers rely on.
		return 0, off, nil
	}
	return area, off, nil
}

// planarRingAreaFromCoords runs the shoelace formula over one
// ring's coordinate slab (no length prefix). Matches PlanarRingArea
// exactly: closedRing behavior via virtual-append when
// (fx,fy) != (lastX,lastY), zero-area guard for < 3 points.
func planarRingAreaFromCoords(data []byte, bo binary.ByteOrder, nPts, cs int) float64 {
	if nPts < 3 {
		return 0
	}
	fx := math.Float64frombits(bo.Uint64(data[0:8]))
	fy := math.Float64frombits(bo.Uint64(data[8:16]))
	var a float64
	px, py := fx, fy
	for i := 1; i < nPts; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(data[off : off+8]))
		y := math.Float64frombits(bo.Uint64(data[off+8 : off+16]))
		a += px*y - x*py
		px, py = x, y
	}
	// Closing edge iff ring wasn't already closed.
	if px != fx || py != fy {
		a += px*fy - fx*py
	}
	return math.Abs(a) / 2
}

// scanMultiPolygonPlanarArea sums per-polygon planar area.
func scanMultiPolygonPlanarArea(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbPolygon
	if hasZ {
		innerType = wkbPolygonZ
	}
	var area float64
	for range n {
		if len(data) < off+5 {
			return 0, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, 0, fmt.Errorf("%w: expected Polygon inside MultiPolygon", ErrTypeMismatch)
		}
		a, sz, err := scanPolygonPlanarArea(data[off+5:], innerBO, hasZ)
		if err != nil {
			return 0, 0, err
		}
		area += a
		off += 5 + sz
	}
	return area, off, nil
}

// scanGeometryCollectionPlanarArea sums PlanarArea across every
// member. Nested GeometryCollections are rejected.
func scanGeometryCollectionPlanarArea(data []byte, bo binary.ByteOrder) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	var area float64
	for range n {
		a, used, err := scanWKBPlanarArea(data[off:], true)
		if err != nil {
			return 0, 0, err
		}
		area += a
		off += used
	}
	return area, off, nil
}

// PlanarLengthFromWKB returns the planar (XY) length of the WKB
// geometry in coordinate units without materializing any
// intermediate []Point / LineString structs. Sums Euclidean
// segment lengths for LineString / MultiLineString; recurses into
// GeometryCollections. All other type codes (Point, MultiPoint,
// Polygon, MultiPolygon) contribute 0 — matches the AoS shape of
// `geometry.Length` where non-linear geometries return 0.
//
// This is the SoA sibling of BoundsFromWKB / CentroidFromWKB for
// callers that only need the planar linear extent — e.g. a Series
// GeomLength column over a projected CRS. Returns coordinate-unit
// length; Unit conversion is left to the caller (multiply by
// `1 / metersPerUnit(u)` for a projected CRS whose linear unit is
// meters). Geographic CRSes must fall back to the AoS
// Haversine path — the WKB blob doesn't carry CRS context.
//
// Byte-order / type-code handling mirrors ParseWKB; malformed
// input returns an error. Zero-allocation on well-formed input.
func PlanarLengthFromWKB(data []byte) (float64, error) {
	total, _, err := scanWKBPlanarLength(data, false)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// scanWKBPlanarLength consumes exactly one WKB geometry from the
// head of data and returns (sum, bytesConsumed, err). When
// inCollection is true, nested GeometryCollections are rejected
// (matching ParseWKB's rule).
func scanWKBPlanarLength(data []byte, inCollection bool) (float64, int, error) {
	if len(data) < 5 {
		return 0, 0, ErrShortWKB
	}
	bo, err := byteOrder(data[0])
	if err != nil {
		return 0, 0, err
	}
	typ := bo.Uint32(data[1:5])
	body := data[5:]
	switch typ {
	case wkbPoint:
		if len(body) < 16 {
			return 0, 0, ErrShortWKB
		}
		return 0, 5 + 16, nil
	case wkbPointZ:
		if len(body) < 24 {
			return 0, 0, ErrShortWKB
		}
		return 0, 5 + 24, nil
	case wkbLineString, wkbLineStringZ:
		hasZ := typ == wkbLineStringZ
		sum, sz, err := scanLineStringPlanarLength(body, bo, hasZ)
		return sum, 5 + sz, err
	case wkbPolygon, wkbPolygonZ:
		// Non-linear — Length returns 0. Walk the byte range to
		// advance the offset honestly.
		hasZ := typ == wkbPolygonZ
		sz, err := skipPolygon(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiPoint, wkbMultiPointZ:
		hasZ := typ == wkbMultiPointZ
		sz, err := skipMultiPoint(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbMultiLineString, wkbMultiLineStringZ:
		hasZ := typ == wkbMultiLineStringZ
		sum, sz, err := scanMultiLineStringPlanarLength(body, bo, hasZ)
		return sum, 5 + sz, err
	case wkbMultiPolygon, wkbMultiPolygonZ:
		hasZ := typ == wkbMultiPolygonZ
		sz, err := skipMultiPolygon(body, bo, hasZ)
		return 0, 5 + sz, err
	case wkbGeometryCollection, wkbGeometryCollectionZ:
		if inCollection {
			return 0, 0, fmt.Errorf("%w: nested GeometryCollection", ErrUnsupportedWKB)
		}
		sum, sz, err := scanGeometryCollectionPlanarLength(body, bo)
		return sum, 5 + sz, err
	default:
		return 0, 0, fmt.Errorf("%w: %d", ErrUnsupportedWKB, typ)
	}
}

// scanLineStringPlanarLength sums Euclidean distances between
// consecutive coordinate pairs. Returns (sum, bytesConsumed).
// Lines with fewer than 2 points contribute 0.
func scanLineStringPlanarLength(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	cs := coordSize(hasZ)
	if len(data) < 4+n*cs {
		return 0, 0, ErrShortWKB
	}
	if n < 2 {
		return 0, 4 + n*cs, nil
	}
	base := data[4:]
	px := math.Float64frombits(bo.Uint64(base[0:8]))
	py := math.Float64frombits(bo.Uint64(base[8:16]))
	var sum float64
	for i := 1; i < n; i++ {
		off := i * cs
		x := math.Float64frombits(bo.Uint64(base[off : off+8]))
		y := math.Float64frombits(bo.Uint64(base[off+8 : off+16]))
		dx := x - px
		dy := y - py
		sum += math.Sqrt(dx*dx + dy*dy)
		px, py = x, y
	}
	return sum, 4 + n*cs, nil
}

// scanMultiLineStringPlanarLength sums the planar length of every
// inner LineString.
func scanMultiLineStringPlanarLength(data []byte, bo binary.ByteOrder, hasZ bool) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	innerType := wkbLineString
	if hasZ {
		innerType = wkbLineStringZ
	}
	var sum float64
	for range n {
		if len(data) < off+5 {
			return 0, 0, ErrShortWKB
		}
		innerBO, err := byteOrder(data[off])
		if err != nil {
			return 0, 0, err
		}
		if innerBO.Uint32(data[off+1:off+5]) != innerType {
			return 0, 0, fmt.Errorf("%w: expected LineString inside MultiLineString", ErrTypeMismatch)
		}
		s, sz, err := scanLineStringPlanarLength(data[off+5:], innerBO, hasZ)
		if err != nil {
			return 0, 0, err
		}
		sum += s
		off += 5 + sz
	}
	return sum, off, nil
}

// scanGeometryCollectionPlanarLength sums Length across every
// member. Nested GeometryCollections are rejected by
// scanWKBPlanarLength (inCollection=true).
func scanGeometryCollectionPlanarLength(data []byte, bo binary.ByteOrder) (float64, int, error) {
	if len(data) < 4 {
		return 0, 0, ErrShortWKB
	}
	n := int(bo.Uint32(data[0:4]))
	off := 4
	var sum float64
	for range n {
		s, used, err := scanWKBPlanarLength(data[off:], true)
		if err != nil {
			return 0, 0, err
		}
		sum += s
		off += used
	}
	return sum, off, nil
}

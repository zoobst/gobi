package gobi

import (
	"fmt"
	"math"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

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

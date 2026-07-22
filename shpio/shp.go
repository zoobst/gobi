// Package shpio reads and writes ESRI Shapefiles as gobi Frames.
//
// A Shapefile is a set of side-by-side files sharing a base name:
//
//   - <base>.shp — geometry records
//   - <base>.shx — offset index (produced/consumed automatically)
//   - <base>.dbf — dBase III attribute table
//   - <base>.prj — optional WKT projection sidecar
//
// This package implements the 1998 ESRI Shapefile Technical Description
// end-to-end for the geometry types most real-world data uses: Null (0),
// Point (1), PolyLine (3), Polygon (5), MultiPoint (8), and their XYZ
// variants (11, 13, 15, 18). M ("measure") variants (types 21-28) are not
// currently supported.
//
// Reader: ReadFile(base) returns a Frame with the attributes as leading
// columns and a WKB "geometry" column at the end, tagged with the CRS
// from <base>.prj when present.
//
// Writer: WriteFile(f, base) walks the geometry column to pick the
// Shapefile geometry type (all rows must share compatible type), emits
// the .shp / .shx / .dbf, and writes a .prj sidecar when the geometry
// column carries a known EPSG code.
package shpio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// Shapefile shape type codes per the ESRI 1998 technical description.
const (
	ShapeNull        = 0
	ShapePoint       = 1
	ShapePolyLine    = 3
	ShapePolygon     = 5
	ShapeMultiPoint  = 8
	ShapePointZ      = 11
	ShapePolyLineZ   = 13
	ShapePolygonZ    = 15
	ShapeMultiPointZ = 18
)

// Shapefile magic: header file code = 9994 at big-endian offset 0.
const shpFileCode = 9994

// ErrInvalidShapefile is returned for malformed or unsupported shapefile
// input.
var ErrInvalidShapefile = errors.New("shpio: invalid shapefile")

// ReadOptions reserves a slot for future read-time configuration
// (e.g., .prj override, dbf encoding hint, M-variant handling).
// Currently empty — pass nil.
//
// The struct exists so future options can be added without breaking
// the ReadFile signature. Matches the naming pattern used by
// parquetio, csvio, gpkgio, geojsonio, kmlio, and pgio.
type ReadOptions struct{}

// WriteOptions reserves a slot for future write-time configuration
// (e.g., forced geometry-type override, .prj injection, custom
// .dbf field spec). Currently empty — pass nil.
type WriteOptions struct{}

// -----------------------------------------------------------------------------
// Reader
// -----------------------------------------------------------------------------

// ReadFile reads the shapefile whose base name is `base` (no `.shp`
// suffix) and returns a Frame with attribute columns from the .dbf plus
// a geometry column. Pass nil opts for defaults.
func ReadFile(base string, opts *ReadOptions) (*gobi.Frame, error) {
	_ = opts // reserved
	base = strings.TrimSuffix(base, ".shp")

	shpBytes, err := os.ReadFile(base + ".shp")
	if err != nil {
		return nil, err
	}
	geoms, err := parseSHP(shpBytes)
	if err != nil {
		return nil, err
	}

	// DBF is optional — if absent we still emit the geometry column.
	var (
		attrFields []arrow.Field
		attrArrs   []arrow.Array
	)
	if dbfBytes, err := os.ReadFile(base + ".dbf"); err == nil {
		attrFields, attrArrs, err = parseDBF(dbfBytes, len(geoms))
		if err != nil {
			return nil, err
		}
	}

	// PRJ is optional — we don't parse WKT, we just note the file's
	// existence and default to EPSG:4326 tagging when it's a WGS 84 prj.
	// (Proper WKT parsing is a project of its own; users who care can
	// override via a follow-up ToCRS call.)
	epsg := int32(4326)
	if prjBytes, err := os.ReadFile(base + ".prj"); err == nil {
		if code := guessEPSGFromPRJ(string(prjBytes)); code != 0 {
			epsg = code
		}
	}

	// Assemble the Frame: attribute columns first, then geometry.
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, g := range geoms {
		if g == nil {
			geomB.AppendNull()
			continue
		}
		geomB.Append(geometry.WKB(g))
	}

	fields := append([]arrow.Field(nil), attrFields...)
	fields = append(fields, gobi.GeometryField("geometry", epsg))

	arrs := append([]arrow.Array(nil), attrArrs...)
	arrs = append(arrs, geomB.NewArray())
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
	}
	return gobi.NewFrame(schema, cols)
}

// parseSHP walks the .shp bytes and returns one gobi Geometry per record
// (nil for Null shapes). The Shapefile file header is 100 bytes; each
// record then has an 8-byte header (record number + content length, both
// big-endian 32-bit) followed by the shape body.
func parseSHP(data []byte) ([]geometry.Geometry, error) {
	if len(data) < 100 {
		return nil, fmt.Errorf("%w: file too short for header", ErrInvalidShapefile)
	}
	if binary.BigEndian.Uint32(data[0:4]) != shpFileCode {
		return nil, fmt.Errorf("%w: bad magic (want 9994)", ErrInvalidShapefile)
	}

	var geoms []geometry.Geometry
	off := 100
	for off < len(data) {
		if off+8 > len(data) {
			return nil, fmt.Errorf("%w: truncated record header at offset %d", ErrInvalidShapefile, off)
		}
		// contentLength is expressed in 16-bit words (per spec).
		contentWords := binary.BigEndian.Uint32(data[off+4 : off+8])
		contentBytes := int(contentWords) * 2
		bodyStart := off + 8
		bodyEnd := bodyStart + contentBytes
		if bodyEnd > len(data) {
			return nil, fmt.Errorf("%w: record extends past EOF", ErrInvalidShapefile)
		}
		body := data[bodyStart:bodyEnd]
		g, err := parseRecord(body)
		if err != nil {
			return nil, err
		}
		geoms = append(geoms, g)
		off = bodyEnd
	}
	return geoms, nil
}

// parseRecord decodes one shape body (starting with the 4-byte shape
// type, little-endian). Returns nil for a Null shape.
func parseRecord(body []byte) (geometry.Geometry, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("%w: record body too short", ErrInvalidShapefile)
	}
	shapeType := binary.LittleEndian.Uint32(body[0:4])
	rest := body[4:]
	switch shapeType {
	case ShapeNull:
		return nil, nil
	case ShapePoint:
		return parsePointRecord(rest, false)
	case ShapePointZ:
		return parsePointRecord(rest, true)
	case ShapeMultiPoint:
		return parseMultiPointRecord(rest, false)
	case ShapeMultiPointZ:
		return parseMultiPointRecord(rest, true)
	case ShapePolyLine:
		return parsePolyLineRecord(rest, false)
	case ShapePolyLineZ:
		return parsePolyLineRecord(rest, true)
	case ShapePolygon:
		return parsePolygonRecord(rest, false)
	case ShapePolygonZ:
		return parsePolygonRecord(rest, true)
	}
	return nil, fmt.Errorf("%w: unsupported shape type %d", ErrInvalidShapefile, shapeType)
}

func parsePointRecord(body []byte, hasZ bool) (geometry.Geometry, error) {
	if len(body) < 16 {
		return nil, fmt.Errorf("%w: Point body too short", ErrInvalidShapefile)
	}
	x := math.Float64frombits(binary.LittleEndian.Uint64(body[0:8]))
	y := math.Float64frombits(binary.LittleEndian.Uint64(body[8:16]))
	p := geometry.Point{X: x, Y: y, HasZ: hasZ}
	if hasZ && len(body) >= 24 {
		p.Z = math.Float64frombits(binary.LittleEndian.Uint64(body[16:24]))
	}
	return p, nil
}

func parseMultiPointRecord(body []byte, hasZ bool) (geometry.Geometry, error) {
	// bbox(32) + numPoints(4) + points(16*n) [+ zRange(16) + z(8*n) if Z]
	if len(body) < 36 {
		return nil, fmt.Errorf("%w: MultiPoint body too short", ErrInvalidShapefile)
	}
	n := int(binary.LittleEndian.Uint32(body[32:36]))
	xyBytes := n * 16
	if len(body) < 36+xyBytes {
		return nil, fmt.Errorf("%w: MultiPoint truncated", ErrInvalidShapefile)
	}
	pts := make([]geometry.Point, n)
	for i := range n {
		off := 36 + i*16
		pts[i] = geometry.Point{
			X:    math.Float64frombits(binary.LittleEndian.Uint64(body[off : off+8])),
			Y:    math.Float64frombits(binary.LittleEndian.Uint64(body[off+8 : off+16])),
			HasZ: hasZ,
		}
	}
	if hasZ {
		zStart := 36 + xyBytes + 16 // skip zRange
		if len(body) < zStart+n*8 {
			return nil, fmt.Errorf("%w: MultiPointZ truncated", ErrInvalidShapefile)
		}
		for i := range n {
			pts[i].Z = math.Float64frombits(binary.LittleEndian.Uint64(body[zStart+i*8 : zStart+i*8+8]))
		}
	}
	return geometry.MultiPoint{Points: pts, HasZ: hasZ}, nil
}

// parsePolyLineRecord decodes a PolyLine (or PolyLineZ) record. A
// PolyLine has one or more "parts," each a linear string of points.
// Single-part records become gobi LineString; multi-part become
// MultiLineString.
func parsePolyLineRecord(body []byte, hasZ bool) (geometry.Geometry, error) {
	parts, points, err := parsePartsAndPoints(body, hasZ)
	if err != nil {
		return nil, err
	}
	if len(parts) == 1 {
		return geometry.LineString{Points: points[parts[0]:], HasZ: hasZ}, nil
	}
	lines := make([]geometry.LineString, len(parts))
	for i, start := range parts {
		end := len(points)
		if i+1 < len(parts) {
			end = parts[i+1]
		}
		lines[i] = geometry.LineString{Points: points[start:end], HasZ: hasZ}
	}
	return geometry.MultiLineString{Lines: lines, HasZ: hasZ}, nil
}

// parsePolygonRecord decodes a Polygon (or PolygonZ). A Polygon has one
// or more parts, each a closed ring. Exterior rings are clockwise;
// interior rings (holes) are counter-clockwise. We use the sign of the
// signed area to distinguish, then group holes with their enclosing
// exterior ring in file order (matches the common shapefile convention).
func parsePolygonRecord(body []byte, hasZ bool) (geometry.Geometry, error) {
	parts, points, err := parsePartsAndPoints(body, hasZ)
	if err != nil {
		return nil, err
	}
	rings := make([][]geometry.Point, len(parts))
	for i, start := range parts {
		end := len(points)
		if i+1 < len(parts) {
			end = parts[i+1]
		}
		rings[i] = points[start:end]
	}
	// Group rings into polygons. Any exterior (CW in shapefile convention)
	// ring starts a new polygon; interior rings attach to the most recent
	// exterior.
	var polys []geometry.Polygon
	for _, ring := range rings {
		if ringIsCW(ring) {
			polys = append(polys, geometry.Polygon{Rings: [][]geometry.Point{ring}, HasZ: hasZ})
			continue
		}
		if len(polys) == 0 {
			// Hole with no exterior — treat as an exterior anyway.
			polys = append(polys, geometry.Polygon{Rings: [][]geometry.Point{ring}, HasZ: hasZ})
			continue
		}
		polys[len(polys)-1].Rings = append(polys[len(polys)-1].Rings, ring)
	}
	if len(polys) == 1 {
		polys[0].HasZ = hasZ
		return polys[0], nil
	}
	return geometry.MultiPolygon{Polygons: polys, HasZ: hasZ}, nil
}

// parsePartsAndPoints decodes the common PolyLine/Polygon record shape:
// bbox(32) + numParts(4) + numPoints(4) + parts(4*np) + points(16*n)
// [ + zRange(16) + z(8*n) if hasZ ].
func parsePartsAndPoints(body []byte, hasZ bool) (parts []int, points []geometry.Point, err error) {
	if len(body) < 40 {
		return nil, nil, fmt.Errorf("%w: record too short for header", ErrInvalidShapefile)
	}
	numParts := int(binary.LittleEndian.Uint32(body[32:36]))
	numPoints := int(binary.LittleEndian.Uint32(body[36:40]))
	partsStart := 40
	pointsStart := partsStart + numParts*4
	xyBytes := numPoints * 16
	if len(body) < pointsStart+xyBytes {
		return nil, nil, fmt.Errorf("%w: record truncated", ErrInvalidShapefile)
	}
	parts = make([]int, numParts)
	for i := range numParts {
		parts[i] = int(binary.LittleEndian.Uint32(body[partsStart+i*4 : partsStart+i*4+4]))
	}
	points = make([]geometry.Point, numPoints)
	for i := range numPoints {
		off := pointsStart + i*16
		points[i] = geometry.Point{
			X:    math.Float64frombits(binary.LittleEndian.Uint64(body[off : off+8])),
			Y:    math.Float64frombits(binary.LittleEndian.Uint64(body[off+8 : off+16])),
			HasZ: hasZ,
		}
	}
	if hasZ {
		zStart := pointsStart + xyBytes + 16 // skip zRange
		if len(body) < zStart+numPoints*8 {
			return nil, nil, fmt.Errorf("%w: Z block truncated", ErrInvalidShapefile)
		}
		for i := range numPoints {
			points[i].Z = math.Float64frombits(binary.LittleEndian.Uint64(body[zStart+i*8 : zStart+i*8+8]))
		}
	}
	return parts, points, nil
}

// ringIsCW returns true if a closed ring is clockwise (signed area < 0
// under Cartesian orientation, which is Shapefile's exterior-ring
// convention).
func ringIsCW(ring []geometry.Point) bool {
	var a float64
	for i := 0; i < len(ring)-1; i++ {
		a += ring[i].X*ring[i+1].Y - ring[i+1].X*ring[i].Y
	}
	return a < 0
}

// guessEPSGFromPRJ scans a PRJ (WKT) blob for a recognizable EPSG code.
// This is intentionally simple — it looks for common projection names and
// the WGS 84 marker. Producing a full WKT parser is out of scope.
func guessEPSGFromPRJ(prj string) int32 {
	s := strings.ToUpper(prj)
	switch {
	case strings.Contains(s, "WGS_1984") || strings.Contains(s, "WGS 84"):
		return 4326
	case strings.Contains(s, "PSEUDO_MERCATOR"), strings.Contains(s, "PSEUDO-MERCATOR"),
		strings.Contains(s, "WEB_MERCATOR"):
		return 3857
	}
	return 0
}

// writeUint32BE is a helper used by both the reader (rarely) and the
// writer for big-endian header fields.
func writeUint32BE(w io.Writer, v uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func writeUint32LE(w io.Writer, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func writeFloat64LE(w io.Writer, v float64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	_, err := w.Write(b[:])
	return err
}

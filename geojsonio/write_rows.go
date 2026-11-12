package geojsonio

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"math"
	"strconv"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// This file is the row loop behind Write. Each Feature is appended to
// one reused buffer: property values straight from the Arrow arrays,
// and the geometry straight from its WKB bytes. Nothing is boxed into
// a map[string]any or decoded into a geometry.Geometry on the way,
// which is where the time used to go (about 22 allocations per row).
//
// The JSON is the same as json.Marshal of the old Feature value,
// except that properties are written in column order. They used to
// come from a map, whose order json/v2 doesn't fix.

// rowColumn walks one column's chunks in row order.
type rowColumn struct {
	key    []byte // `"name":`, quoted once
	name   string
	chunks []arrow.Array
	chunk  int // index of the chunk holding the current row
	local  int // the current row's index within that chunk
}

func newRowColumn(s gobi.Series) rowColumn {
	return rowColumn{name: s.Name(), chunks: s.Column().Data().Chunks(), local: -1}
}

// next moves to the following row and returns its chunk and index.
// Calling it more times than the column has rows is a caller bug.
func (c *rowColumn) next() (arrow.Array, int) {
	c.local++
	for c.local >= c.chunks[c.chunk].Len() {
		c.chunk++
		c.local = 0
	}
	return c.chunks[c.chunk], c.local
}

// forEachFeature builds a Feature JSON object per row of df and
// invokes fn with it. The slice passed to fn is reused for the next
// row, so fn must not keep it. Kept as a helper so the
// FeatureCollection and line writers share one row loop.
func forEachFeature(df *gobi.Frame, geomIdx int, fn func(row int, featJSON []byte) error) error {
	props := make([]rowColumn, 0, df.NumCols())
	var geom *rowColumn
	for i := range df.NumCols() {
		s, err := df.ColumnAt(i)
		if err != nil {
			return err
		}
		c := newRowColumn(s)
		if i == geomIdx {
			geom = &c
			continue
		}
		key, err := jsontext.AppendQuote(nil, c.name)
		if err != nil {
			return fmt.Errorf("geojsonio: column name %q: %w", c.name, err)
		}
		c.key = append(key, ':')
		props = append(props, c)
	}

	var buf []byte
	for row := range df.NumRows() {
		buf = append(buf[:0], `{"type":"Feature","geometry":`...)
		if geom == nil {
			buf = append(buf, "null"...)
		} else {
			chunk, i := geom.next()
			var err error
			if buf, err = appendGeometryCell(buf, chunk, i); err != nil {
				return fmt.Errorf("geojsonio: geometry row %d: %w", row, err)
			}
		}
		// A frame with no property columns omits the member, as the
		// Feature struct's omitempty does.
		if len(props) > 0 {
			buf = append(buf, `,"properties":{`...)
			for j := range props {
				c := &props[j]
				if j > 0 {
					buf = append(buf, ',')
				}
				buf = append(buf, c.key...)
				chunk, i := c.next()
				var err error
				if buf, err = appendPropertyCell(buf, chunk, i); err != nil {
					return fmt.Errorf("geojsonio: %s row %d: %w", c.name, row, err)
				}
			}
			buf = append(buf, '}')
		}
		buf = append(buf, '}')
		if err := fn(row, buf); err != nil {
			return err
		}
	}
	return nil
}

// appendPropertyCell appends row i of chunk as a JSON value. The
// types and their JSON forms are those of scalarAt followed by
// json.Marshal.
func appendPropertyCell(dst []byte, chunk arrow.Array, i int) ([]byte, error) {
	if chunk.IsNull(i) {
		return append(dst, "null"...), nil
	}
	switch a := chunk.(type) {
	case *array.Int64:
		return strconv.AppendInt(dst, a.Value(i), 10), nil
	case *array.Int32:
		return strconv.AppendInt(dst, int64(a.Value(i)), 10), nil
	case *array.Uint64:
		return strconv.AppendUint(dst, a.Value(i), 10), nil
	case *array.Uint32:
		return strconv.AppendUint(dst, uint64(a.Value(i)), 10), nil
	case *array.Float64:
		return appendJSONFloat(dst, a.Value(i))
	case *array.Float32:
		return appendJSONFloat(dst, float64(a.Value(i)))
	case *array.Boolean:
		return strconv.AppendBool(dst, a.Value(i)), nil
	case *array.String:
		return jsontext.AppendQuote(dst, a.Value(i))
	case *array.LargeString:
		return jsontext.AppendQuote(dst, a.Value(i))
	case *array.Binary:
		dst = append(dst, '"')
		dst = base64.StdEncoding.AppendEncode(dst, a.Value(i))
		return append(dst, '"'), nil
	case *array.Timestamp:
		// RFC 3339 for round-trip-friendly JSON. The unit comes from
		// the array's type.
		unit := a.DataType().(*arrow.TimestampType).Unit
		return jsontext.AppendQuote(dst, timestampToRFC3339(int64(a.Value(i)), unit))
	}
	return dst, fmt.Errorf("unsupported chunk type %T", chunk)
}

// appendJSONFloat appends f the way encoding/json does: the shortest
// decimal that round-trips, in exponent form only below 1e-6 or from
// 1e21 up. NaN and ±Inf have no JSON form; they return json.Marshal's
// own error.
func appendJSONFloat(dst []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		_, err := json.Marshal(f)
		return dst, err
	}
	return appendFiniteFloat(dst, f), nil
}

func appendFiniteFloat(dst []byte, f float64) []byte {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// Clean up e-09 to e-9.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

// appendGeometryCell appends row i of a geometry column as a GeoJSON
// geometry object, or null for a null row.
func appendGeometryCell(dst []byte, chunk arrow.Array, i int) ([]byte, error) {
	if chunk.IsNull(i) {
		return append(dst, "null"...), nil
	}
	ba, ok := chunk.(*array.Binary)
	if !ok {
		return dst, fmt.Errorf("geometry column not Binary, got %T", chunk)
	}
	wkb := ba.Value(i)
	// The common shapes are transcoded from WKB directly. Anything the
	// transcoder declines (see wkbToJSON) goes through the geometry
	// types, which also produce the error for input that is invalid.
	if out, ok := appendWKBAsGeoJSON(dst, wkb); ok {
		return out, nil
	}
	g, err := geometry.ParseWKB(wkb)
	if err != nil {
		return dst, fmt.Errorf("parse: %w", err)
	}
	b, err := marshalGeom(g)
	if err != nil {
		return dst, err
	}
	return append(dst, b...), nil
}

// appendWKBAsGeoJSON appends the GeoJSON geometry object for wkb. ok
// is false, and dst is returned unchanged, when wkb isn't something
// the transcoder handles; the caller then takes the slower path
// through geometry.ParseWKB. That covers truncated or trailing bytes,
// type codes other than the seven ISO geometry types and their Z
// variants, a coordinate that is NaN or ±Inf (an empty Point), and a
// Multi* whose members disagree with it about Z.
func appendWKBAsGeoJSON(dst, wkb []byte) (out []byte, ok bool) {
	t := wkbToJSON{dst: dst, src: wkb}
	t.geometry()
	if t.bad || t.pos != len(wkb) {
		return dst, false
	}
	return t.dst, true
}

// wkbToJSON reads WKB from src and appends GeoJSON to dst. Any
// problem sets bad, after which the output is abandoned.
type wkbToJSON struct {
	dst []byte
	src []byte
	pos int
	bad bool
}

// WKB geometry type codes; the Z variants add 1000.
const (
	wkbPoint = iota + 1
	wkbLineString
	wkbPolygon
	wkbMultiPoint
	wkbMultiLineString
	wkbMultiPolygon
	wkbGeometryCollection
)

// header reads a byte-order mark and type code.
func (t *wkbToJSON) header() (code uint32, hasZ bool, order binary.ByteOrder) {
	if t.bad || len(t.src)-t.pos < 5 {
		t.bad = true
		return 0, false, nil
	}
	switch t.src[t.pos] {
	case 0:
		order = binary.BigEndian
	case 1:
		order = binary.LittleEndian
	default:
		t.bad = true
		return 0, false, nil
	}
	code = order.Uint32(t.src[t.pos+1:])
	t.pos += 5
	if code > 1000 && code <= 1000+wkbGeometryCollection {
		return code - 1000, true, order
	}
	if code < wkbPoint || code > wkbGeometryCollection {
		t.bad = true
	}
	return code, false, order
}

// count reads a uint32 element count. Each element takes at least
// minSize bytes, so a count the remaining input can't hold is bad.
func (t *wkbToJSON) count(order binary.ByteOrder, minSize int) int {
	if t.bad || len(t.src)-t.pos < 4 {
		t.bad = true
		return 0
	}
	n := int(order.Uint32(t.src[t.pos:]))
	t.pos += 4
	if n > (len(t.src)-t.pos)/minSize {
		t.bad = true
		return 0
	}
	return n
}

// position appends one [x,y] or [x,y,z].
func (t *wkbToJSON) position(order binary.ByteOrder, hasZ bool) {
	dims := 2
	if hasZ {
		dims = 3
	}
	if t.bad || len(t.src)-t.pos < 8*dims {
		t.bad = true
		return
	}
	t.dst = append(t.dst, '[')
	for d := range dims {
		f := math.Float64frombits(order.Uint64(t.src[t.pos:]))
		t.pos += 8
		if math.IsNaN(f) || math.IsInf(f, 0) {
			t.bad = true
			return
		}
		if d > 0 {
			t.dst = append(t.dst, ',')
		}
		t.dst = appendFiniteFloat(t.dst, f)
	}
	t.dst = append(t.dst, ']')
}

// positions appends a counted list of positions: [[x,y],...].
func (t *wkbToJSON) positions(order binary.ByteOrder, hasZ bool) {
	size := 16
	if hasZ {
		size = 24
	}
	n := t.count(order, size)
	t.dst = append(t.dst, '[')
	for i := range n {
		if i > 0 {
			t.dst = append(t.dst, ',')
		}
		t.position(order, hasZ)
	}
	t.dst = append(t.dst, ']')
}

// rings appends a counted list of rings: [[[x,y],...],...].
func (t *wkbToJSON) rings(order binary.ByteOrder, hasZ bool) {
	n := t.count(order, 4)
	t.dst = append(t.dst, '[')
	for i := range n {
		if i > 0 {
			t.dst = append(t.dst, ',')
		}
		t.positions(order, hasZ)
	}
	t.dst = append(t.dst, ']')
}

// members appends the coordinates of a Multi*'s counted members,
// each a complete WKB geometry of type want.
func (t *wkbToJSON) members(order binary.ByteOrder, want uint32, hasZ bool) {
	n := t.count(order, 5)
	t.dst = append(t.dst, '[')
	for i := range n {
		if i > 0 {
			t.dst = append(t.dst, ',')
		}
		code, z, memberOrder := t.header()
		if t.bad || code != want || z != hasZ {
			t.bad = true
			return
		}
		t.coordinates(code, z, memberOrder)
	}
	t.dst = append(t.dst, ']')
}

// coordinates appends the "coordinates" value for a geometry whose
// header has been read.
func (t *wkbToJSON) coordinates(code uint32, hasZ bool, order binary.ByteOrder) {
	switch code {
	case wkbPoint:
		t.position(order, hasZ)
	case wkbLineString:
		t.positions(order, hasZ)
	case wkbPolygon:
		t.rings(order, hasZ)
	case wkbMultiPoint:
		t.members(order, wkbPoint, hasZ)
	case wkbMultiLineString:
		t.members(order, wkbLineString, hasZ)
	case wkbMultiPolygon:
		t.members(order, wkbPolygon, hasZ)
	default:
		t.bad = true
	}
}

var geoJSONTypeNames = [...]string{
	wkbPoint:              "Point",
	wkbLineString:         "LineString",
	wkbPolygon:            "Polygon",
	wkbMultiPoint:         "MultiPoint",
	wkbMultiLineString:    "MultiLineString",
	wkbMultiPolygon:       "MultiPolygon",
	wkbGeometryCollection: "GeometryCollection",
}

// geometry appends one complete geometry object.
func (t *wkbToJSON) geometry() {
	code, hasZ, order := t.header()
	if t.bad {
		return
	}
	t.dst = append(t.dst, `{"type":"`...)
	t.dst = append(t.dst, geoJSONTypeNames[code]...)
	if code == wkbGeometryCollection {
		// RFC 7946 doesn't allow nesting, so only a flat collection
		// is transcoded here.
		n := t.count(order, 5)
		t.dst = append(t.dst, `","geometries":[`...)
		for i := range n {
			if i > 0 {
				t.dst = append(t.dst, ',')
			}
			if len(t.src)-t.pos >= 5 {
				// Peek: a nested collection is left to the slow path.
				inner := t.src[t.pos+1 : t.pos+5]
				if c := binary.LittleEndian.Uint32(inner); c == wkbGeometryCollection || c == 1000+wkbGeometryCollection {
					t.bad = true
				}
				if c := binary.BigEndian.Uint32(inner); c == wkbGeometryCollection || c == 1000+wkbGeometryCollection {
					t.bad = true
				}
			}
			t.geometry()
			if t.bad {
				return
			}
		}
		t.dst = append(t.dst, "]}"...)
		return
	}
	t.dst = append(t.dst, `","coordinates":`...)
	t.coordinates(code, hasZ, order)
	t.dst = append(t.dst, '}')
}

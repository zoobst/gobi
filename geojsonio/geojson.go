// Package geojsonio reads and writes gobi Frames as GeoJSON per
// RFC 7946.
//
// Supports every geometry type in RFC 7946 §3.1 — Point,
// LineString, Polygon, MultiPoint, MultiLineString, MultiPolygon,
// GeometryCollection — with optional Z coordinate values (RFC 7946
// §3.1.1 allows a third position value for XYZ; higher dimensions
// are dropped on read per the RFC's "SHOULD NOT extend positions
// beyond three elements" guidance).
//
// Coordinate reference systems are always WGS 84 (EPSG:4326) per
// the RFC. Non-WGS84 CRS specifications in the input's `crs` field
// are ignored on read — the RFC deprecated that field in 2016.
//
// Entry points:
//
//   - Marshal / Unmarshal — codec for a single geometry object.
//     Used when embedding GeoJSON in another format or hand-parsing
//     a specific feature.
//
//   - MarshalFeature / UnmarshalFeature — Feature wrapper codec
//     (geometry + properties + id).
//
//   - ReadFile / ReadFileChunksFunc / WriteFile — Frame-level I/O.
//     Reads a FeatureCollection (or line-delimited GeoJSON) into a
//     Frame with a `geometry` column plus one column per unique
//     property key. Writes a Frame back out as FeatureCollection.
//
//   - ScanFile — LazyFrame entry point matching parquetio /
//     gpkgio / csvio's shape. No predicate pushdown (JSON is
//     sequential) but streaming + projection above the scan work.
package geojsonio

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zoobst/gobi/geometry"
)

// ErrInvalidGeoJSON is returned when the input is malformed or
// references an unsupported geometry type. Wrap-friendly:
// errors.Is(err, ErrInvalidGeoJSON) is true for every parse failure
// this package produces.
var ErrInvalidGeoJSON = errors.New("geojson: invalid input")

// Feature is a GeoJSON Feature wrapper (RFC 7946 §3.2).
//
// ID is left as an `any` because the RFC allows both string and
// number IDs; callers should type-switch on the concrete Go type.
// Properties is a free-form map — RFC 7946 doesn't constrain the
// value types, so JSON's usual (float64 / string / bool / nil / []any
// / map[string]any) shapes apply.
type Feature struct {
	Type       string          `json:"type"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties map[string]any  `json:"properties,omitempty"`
	ID         any             `json:"id,omitempty"`
}

// FeatureCollection is the top-level container GeoJSON files
// usually carry (RFC 7946 §3.3).
type FeatureCollection struct {
	Type     string    `json:"type"`
	Features []Feature `json:"features"`
}

// Marshal encodes a Geometry to its GeoJSON representation. The
// output is deterministic — keys appear in the order "type",
// "coordinates" (or "geometries" for GeometryCollection).
//
// XYZ points emit three-element coordinate arrays; XY points emit
// two. Callers can inspect the input's Is3D() to know which they'll
// get.
func Marshal(g geometry.Geometry) ([]byte, error) {
	buf, err := marshalGeom(g)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// Unmarshal decodes a GeoJSON geometry object. The returned
// geometry has its CRS set to WGS 84 per RFC 7946. Coordinate
// arrays with three elements populate Z + set HasZ; two-element
// arrays yield 2D geometries.
func Unmarshal(data []byte) (geometry.Geometry, error) {
	var head struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
		Geometries  json.RawMessage `json:"geometries"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
	}
	return decodeGeom(head.Type, head.Coordinates, head.Geometries)
}

// MarshalFeature encodes a geometry + properties bag as a GeoJSON
// Feature (RFC 7946 §3.2). Passing a nil geometry emits a Feature
// with `"geometry": null`, which is valid per the RFC.
func MarshalFeature(g geometry.Geometry, properties map[string]any) ([]byte, error) {
	var geomRaw json.RawMessage
	if g != nil {
		buf, err := marshalGeom(g)
		if err != nil {
			return nil, err
		}
		geomRaw = buf
	} else {
		geomRaw = json.RawMessage("null")
	}
	f := Feature{Type: "Feature", Geometry: geomRaw, Properties: properties}
	return json.Marshal(f)
}

// UnmarshalFeature decodes a GeoJSON Feature into its geometry and
// property bag. Returns nil geometry (with nil error) when the
// feature carries `"geometry": null`.
func UnmarshalFeature(data []byte) (geometry.Geometry, map[string]any, error) {
	var f Feature
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
	}
	if len(f.Geometry) == 0 || string(f.Geometry) == "null" {
		return nil, f.Properties, nil
	}
	g, err := Unmarshal(f.Geometry)
	if err != nil {
		return nil, nil, err
	}
	return g, f.Properties, nil
}

// -----------------------------------------------------------------------------
// Decoders
// -----------------------------------------------------------------------------

// decodeGeom dispatches on the RFC 7946 geometry-type name. Handles
// every type in §3.1, including GeometryCollection which uses
// "geometries" instead of "coordinates".
func decodeGeom(typ string, coords, geoms json.RawMessage) (geometry.Geometry, error) {
	switch typ {
	case "Point":
		return decodePoint(coords)
	case "LineString":
		return decodeLineString(coords)
	case "Polygon":
		return decodePolygon(coords)
	case "MultiPoint":
		return decodeMultiPoint(coords)
	case "MultiLineString":
		return decodeMultiLineString(coords)
	case "MultiPolygon":
		return decodeMultiPolygon(coords)
	case "GeometryCollection":
		return decodeGeometryCollection(geoms)
	}
	return nil, fmt.Errorf("%w: unsupported type %q", ErrInvalidGeoJSON, typ)
}

// decodePoint parses a Position — a 2- or 3-element numeric array
// per RFC 7946 §3.1.1. A 3-element array populates Z + sets HasZ.
// Positions with more than 3 elements silently drop the extras
// (RFC guidance).
func decodePoint(coords json.RawMessage) (geometry.Point, error) {
	pos, err := decodePosition(coords)
	if err != nil {
		return geometry.Point{}, fmt.Errorf("%w: point coords: %v", ErrInvalidGeoJSON, err)
	}
	return pos.toPoint(), nil
}

func decodeLineString(coords json.RawMessage) (geometry.LineString, error) {
	pts, hasZ, err := decodePositionList(coords)
	if err != nil {
		return geometry.LineString{}, fmt.Errorf("%w: linestring coords: %v", ErrInvalidGeoJSON, err)
	}
	return geometry.LineString{Points: pts, CRSValue: geometry.WGS84, HasZ: hasZ}, nil
}

func decodePolygon(coords json.RawMessage) (geometry.Polygon, error) {
	rings, hasZ, err := decodeRings(coords)
	if err != nil {
		return geometry.Polygon{}, fmt.Errorf("%w: polygon coords: %v", ErrInvalidGeoJSON, err)
	}
	return geometry.Polygon{Rings: rings, CRSValue: geometry.WGS84, HasZ: hasZ}, nil
}

func decodeMultiPoint(coords json.RawMessage) (geometry.MultiPoint, error) {
	pts, hasZ, err := decodePositionList(coords)
	if err != nil {
		return geometry.MultiPoint{}, fmt.Errorf("%w: multipoint coords: %v", ErrInvalidGeoJSON, err)
	}
	return geometry.MultiPoint{Points: pts, CRSValue: geometry.WGS84, HasZ: hasZ}, nil
}

func decodeMultiLineString(coords json.RawMessage) (geometry.MultiLineString, error) {
	rings, hasZ, err := decodeRings(coords)
	if err != nil {
		return geometry.MultiLineString{}, fmt.Errorf("%w: multilinestring coords: %v", ErrInvalidGeoJSON, err)
	}
	lines := make([]geometry.LineString, len(rings))
	for i, r := range rings {
		lines[i] = geometry.LineString{Points: r, CRSValue: geometry.WGS84, HasZ: hasZ}
	}
	return geometry.MultiLineString{Lines: lines, CRSValue: geometry.WGS84, HasZ: hasZ}, nil
}

func decodeMultiPolygon(coords json.RawMessage) (geometry.MultiPolygon, error) {
	// A MultiPolygon's coordinates are `[polygon, polygon, ...]`,
	// where each polygon is `[ring, ring, ...]`, where each ring is
	// `[position, position, ...]`. Three levels of nesting.
	var polyCoords []json.RawMessage
	if err := json.Unmarshal(coords, &polyCoords); err != nil {
		return geometry.MultiPolygon{}, fmt.Errorf("%w: multipolygon coords: %v", ErrInvalidGeoJSON, err)
	}
	polygons := make([]geometry.Polygon, len(polyCoords))
	anyZ := false
	for i, pc := range polyCoords {
		p, err := decodePolygon(pc)
		if err != nil {
			return geometry.MultiPolygon{}, err
		}
		polygons[i] = p
		if p.HasZ {
			anyZ = true
		}
	}
	return geometry.MultiPolygon{Polygons: polygons, CRSValue: geometry.WGS84, HasZ: anyZ}, nil
}

func decodeGeometryCollection(geoms json.RawMessage) (geometry.GeometryCollection, error) {
	// RFC 7946 §3.1.8: `geometries` is an array of geometry objects
	// (each with its own "type" + coords). Recursively decode each,
	// then wrap.
	var raw []json.RawMessage
	if err := json.Unmarshal(geoms, &raw); err != nil {
		return geometry.GeometryCollection{}, fmt.Errorf("%w: geometrycollection: %v", ErrInvalidGeoJSON, err)
	}
	out := make([]geometry.Geometry, len(raw))
	anyZ := false
	for i, r := range raw {
		g, err := Unmarshal(r)
		if err != nil {
			return geometry.GeometryCollection{}, err
		}
		out[i] = g
		if g.Is3D() {
			anyZ = true
		}
	}
	return geometry.GeometryCollection{Geometries: out, CRSValue: geometry.WGS84, HasZ: anyZ}, nil
}

// position is an intermediate representation for one coordinate
// value — 2D or 3D. Kept separate so decodePositionList / decodeRings
// can share the "was any position 3D?" bookkeeping across siblings.
type position struct {
	x, y, z float64
	hasZ    bool
}

func (p position) toPoint() geometry.Point {
	return geometry.Point{
		X: p.x, Y: p.y, Z: p.z,
		HasZ:     p.hasZ,
		CRSValue: geometry.WGS84,
	}
}

// decodePosition parses one coordinate array. Accepts 2 or 3 elements
// (RFC 7946 §3.1.1 requires at least 2; MAY have 3 for elevation;
// SHOULD NOT extend beyond 3). Silently truncates 4+ element arrays.
func decodePosition(data json.RawMessage) (position, error) {
	var arr []float64
	if err := json.Unmarshal(data, &arr); err != nil {
		return position{}, err
	}
	if len(arr) < 2 {
		return position{}, fmt.Errorf("position needs at least 2 coordinates, got %d", len(arr))
	}
	pos := position{x: arr[0], y: arr[1]}
	if len(arr) >= 3 {
		pos.z = arr[2]
		pos.hasZ = true
	}
	return pos, nil
}

// decodePositionList parses `[[x,y], [x,y], ...]` — the LineString /
// MultiPoint shape. Returns the point slice + a flag indicating
// whether ANY position had a Z element. A mix of 2D and 3D positions
// on a single line is technically undefined in the RFC; we consume
// it by treating the line as 3D and defaulting Z=0 for the 2D
// positions.
func decodePositionList(data json.RawMessage) ([]geometry.Point, bool, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, err
	}
	pts := make([]geometry.Point, len(raw))
	anyZ := false
	for i, r := range raw {
		p, err := decodePosition(r)
		if err != nil {
			return nil, false, err
		}
		if p.hasZ {
			anyZ = true
		}
		pts[i] = p.toPoint()
	}
	if anyZ {
		// Backfill HasZ on every point so callers reading Point.Z
		// don't accidentally get an uninitialized zero for the 2D
		// positions.
		for i := range pts {
			pts[i].HasZ = true
		}
	}
	return pts, anyZ, nil
}

// decodeRings parses `[[[x,y], ...], [[x,y], ...], ...]` — the
// Polygon / MultiLineString shape (list of position-lists).
func decodeRings(data json.RawMessage) ([][]geometry.Point, bool, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, err
	}
	rings := make([][]geometry.Point, len(raw))
	anyZ := false
	for i, r := range raw {
		pts, hasZ, err := decodePositionList(r)
		if err != nil {
			return nil, false, err
		}
		rings[i] = pts
		if hasZ {
			anyZ = true
		}
	}
	return rings, anyZ, nil
}

// -----------------------------------------------------------------------------
// Encoders
// -----------------------------------------------------------------------------

// geomObject is a struct with fixed key order so the emitted JSON
// looks the way tools expect. `Geometries` is used only for
// GeometryCollection; `Coordinates` for everything else. Both are
// tagged omitempty so the two branches emit their proper JSON.
type geomObject struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates,omitempty"`
	Geometries  json.RawMessage `json:"geometries,omitempty"`
}

// marshalGeom is the encoder dispatcher — one branch per geometry
// type. Each branch builds the coordinate JSON directly (without an
// intermediate map[string]any) so the encoder makes only one small
// []byte allocation per feature. Meaningful on large FeatureCollections.
func marshalGeom(g geometry.Geometry) ([]byte, error) {
	switch t := g.(type) {
	case geometry.Point:
		coords, err := encodePosition(t.X, t.Y, t.Z, t.HasZ)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "Point", Coordinates: coords})
	case geometry.LineString:
		coords, err := encodePositionList(t.Points, t.HasZ)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "LineString", Coordinates: coords})
	case geometry.Polygon:
		coords, err := encodeRings(t.Rings, t.HasZ)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "Polygon", Coordinates: coords})
	case geometry.MultiPoint:
		coords, err := encodePositionList(t.Points, t.HasZ)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "MultiPoint", Coordinates: coords})
	case geometry.MultiLineString:
		rings := make([][]geometry.Point, len(t.Lines))
		for i, l := range t.Lines {
			rings[i] = l.Points
		}
		coords, err := encodeRings(rings, t.HasZ)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "MultiLineString", Coordinates: coords})
	case geometry.MultiPolygon:
		coords, err := encodeMultiPolygonCoords(t.Polygons, t.HasZ)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "MultiPolygon", Coordinates: coords})
	case geometry.GeometryCollection:
		geoms, err := encodeGeometries(t.Geometries)
		if err != nil {
			return nil, err
		}
		return json.Marshal(geomObject{Type: "GeometryCollection", Geometries: geoms})
	}
	return nil, fmt.Errorf("%w: unsupported geometry %T", ErrInvalidGeoJSON, g)
}

func encodePosition(x, y, z float64, hasZ bool) (json.RawMessage, error) {
	if hasZ {
		return json.Marshal([3]float64{x, y, z})
	}
	return json.Marshal([2]float64{x, y})
}

func encodePositionList(pts []geometry.Point, hasZ bool) (json.RawMessage, error) {
	if hasZ {
		out := make([][3]float64, len(pts))
		for i, p := range pts {
			out[i] = [3]float64{p.X, p.Y, p.Z}
		}
		return json.Marshal(out)
	}
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[i] = [2]float64{p.X, p.Y}
	}
	return json.Marshal(out)
}

func encodeRings(rings [][]geometry.Point, hasZ bool) (json.RawMessage, error) {
	if hasZ {
		out := make([][][3]float64, len(rings))
		for i, r := range rings {
			row := make([][3]float64, len(r))
			for j, p := range r {
				row[j] = [3]float64{p.X, p.Y, p.Z}
			}
			out[i] = row
		}
		return json.Marshal(out)
	}
	out := make([][][2]float64, len(rings))
	for i, r := range rings {
		row := make([][2]float64, len(r))
		for j, p := range r {
			row[j] = [2]float64{p.X, p.Y}
		}
		out[i] = row
	}
	return json.Marshal(out)
}

func encodeMultiPolygonCoords(polys []geometry.Polygon, hasZ bool) (json.RawMessage, error) {
	if hasZ {
		out := make([][][][3]float64, len(polys))
		for i, p := range polys {
			rings := make([][][3]float64, len(p.Rings))
			for j, r := range p.Rings {
				coords := make([][3]float64, len(r))
				for k, pt := range r {
					coords[k] = [3]float64{pt.X, pt.Y, pt.Z}
				}
				rings[j] = coords
			}
			out[i] = rings
		}
		return json.Marshal(out)
	}
	out := make([][][][2]float64, len(polys))
	for i, p := range polys {
		rings := make([][][2]float64, len(p.Rings))
		for j, r := range p.Rings {
			coords := make([][2]float64, len(r))
			for k, pt := range r {
				coords[k] = [2]float64{pt.X, pt.Y}
			}
			rings[j] = coords
		}
		out[i] = rings
	}
	return json.Marshal(out)
}

// encodeGeometries builds a JSON array of GeoJSON geometry objects
// for GeometryCollection.geometries. Each nested geometry gets its
// own recursive marshal — depth is bounded because GeometryCollection
// can't nest inside GeometryCollection (RFC 7946 §3.1.8).
func encodeGeometries(gs []geometry.Geometry) (json.RawMessage, error) {
	pieces := make([]json.RawMessage, len(gs))
	for i, g := range gs {
		b, err := marshalGeom(g)
		if err != nil {
			return nil, err
		}
		pieces[i] = b
	}
	return json.Marshal(pieces)
}

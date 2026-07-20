// Package geojson encodes and decodes GeoJSON geometries per RFC 7946.
//
// Only individual geometries and Feature/FeatureCollection wrappers with
// primitive geometries (Point, LineString, Polygon, MultiPoint) are
// supported. Coordinate reference systems are always assumed to be WGS 84
// (EPSG:4326) per the RFC.
package geojson

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zoobst/gobi/geometry"
)

// ErrInvalidGeoJSON is returned when the input is malformed or references an
// unsupported geometry type.
var ErrInvalidGeoJSON = errors.New("geojson: invalid input")

// Feature is a GeoJSON Feature wrapper.
type Feature struct {
	Type       string          `json:"type"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties map[string]any  `json:"properties,omitempty"`
	ID         any             `json:"id,omitempty"`
}

// Marshal encodes a Geometry to its GeoJSON representation.
func Marshal(g geometry.Geometry) ([]byte, error) {
	obj, err := toGeoJSON(g)
	if err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

// Unmarshal decodes a GeoJSON geometry object. The returned geometry has
// its CRS set to WGS 84 per the RFC.
func Unmarshal(data []byte) (geometry.Geometry, error) {
	var raw struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
	}
	return decodeType(raw.Type, raw.Coordinates)
}

// UnmarshalFeature decodes a GeoJSON Feature into its geometry and property
// bag.
func UnmarshalFeature(data []byte) (geometry.Geometry, map[string]any, error) {
	var f Feature
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidGeoJSON, err)
	}
	g, err := Unmarshal(f.Geometry)
	if err != nil {
		return nil, nil, err
	}
	return g, f.Properties, nil
}

func decodeType(typ string, coords json.RawMessage) (geometry.Geometry, error) {
	switch typ {
	case "Point":
		var xy [2]float64
		if err := json.Unmarshal(coords, &xy); err != nil {
			return nil, fmt.Errorf("%w: point coords: %v", ErrInvalidGeoJSON, err)
		}
		return geometry.Point{X: xy[0], Y: xy[1], CRSValue: geometry.WGS84}, nil
	case "LineString":
		var pts [][2]float64
		if err := json.Unmarshal(coords, &pts); err != nil {
			return nil, fmt.Errorf("%w: linestring coords: %v", ErrInvalidGeoJSON, err)
		}
		return geometry.LineString{Points: toPoints(pts), CRSValue: geometry.WGS84}, nil
	case "Polygon":
		var rings [][][2]float64
		if err := json.Unmarshal(coords, &rings); err != nil {
			return nil, fmt.Errorf("%w: polygon coords: %v", ErrInvalidGeoJSON, err)
		}
		out := make([][]geometry.Point, len(rings))
		for i, r := range rings {
			out[i] = toPoints(r)
		}
		return geometry.Polygon{Rings: out, CRSValue: geometry.WGS84}, nil
	case "MultiPoint":
		var pts [][2]float64
		if err := json.Unmarshal(coords, &pts); err != nil {
			return nil, fmt.Errorf("%w: multipoint coords: %v", ErrInvalidGeoJSON, err)
		}
		return geometry.MultiPoint{Points: toPoints(pts), CRSValue: geometry.WGS84}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported type %q", ErrInvalidGeoJSON, typ)
	}
}

func toPoints(coords [][2]float64) []geometry.Point {
	out := make([]geometry.Point, len(coords))
	for i, c := range coords {
		out[i] = geometry.Point{X: c[0], Y: c[1], CRSValue: geometry.WGS84}
	}
	return out
}

func fromPoints(pts []geometry.Point) [][2]float64 {
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[i] = [2]float64{p.X, p.Y}
	}
	return out
}

func toGeoJSON(g geometry.Geometry) (any, error) {
	switch t := g.(type) {
	case geometry.Point:
		return map[string]any{"type": "Point", "coordinates": [2]float64{t.X, t.Y}}, nil
	case geometry.LineString:
		return map[string]any{"type": "LineString", "coordinates": fromPoints(t.Points)}, nil
	case geometry.Polygon:
		rings := make([][][2]float64, len(t.Rings))
		for i, r := range t.Rings {
			rings[i] = fromPoints(r)
		}
		return map[string]any{"type": "Polygon", "coordinates": rings}, nil
	case geometry.MultiPoint:
		return map[string]any{"type": "MultiPoint", "coordinates": fromPoints(t.Points)}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported geometry %T", ErrInvalidGeoJSON, g)
	}
}

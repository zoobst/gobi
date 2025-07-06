package geojson

import (
	"encoding/json"
	"errors"

	"github.com/zoobst/gobi/geometry"
)

type GeometryType string

const (
	PointType              GeometryType = "Point"
	MultiPointType         GeometryType = "MultiPoint"
	LineStringType         GeometryType = "LineString"
	MultiLineStringType    GeometryType = "MultiLineString"
	PolygonType            GeometryType = "Polygon"
	MultiPolygonType       GeometryType = "MultiPolygon"
	GeometryCollectionType GeometryType = "GeometryCollection"
	FeatureType            GeometryType = "Feature"
	FeatureCollectionType  GeometryType = "FeatureCollection"
)

type Geometry interface {
	geometry.Geometry
}

type Feature struct {
	Type       GeometryType   `json:"type"` // always "Feature"
	Geometry   *Geometry      `json:"geometry"`
	Properties map[string]any `json:"properties"`
	BBox       []float64      `json:"bbox,omitempty"`
}

type FeatureCollection struct {
	Type     GeometryType `json:"type"` // always "FeatureCollection"
	Features []*Feature   `json:"features"`
	BBox     []float64    `json:"bbox,omitempty"`
	CRS      *CRS         `json:"crs,omitempty"`
}

func (f *Feature) MarshalJSON() ([]byte, error) {
	f.Type = FeatureType
	return json.Marshal(f)
}

func (f *Feature) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, f)
}

func (fc *FeatureCollection) MarshalJSON() ([]byte, error) {
	fc.Type = FeatureCollectionType
	return json.Marshal(fc)
}

func (fc *FeatureCollection) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, fc)
}

type CRS struct {
	geometry.CRS
}

func (c *CRS) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type       string            `json:"type"`
		Properties map[string]string `json:"properties"`
	}{
		Type: "name",
		Properties: map[string]string{
			"name": c.Name,
		},
	})
}

func (c *CRS) UnmarshalJSON(data []byte) (err error) {
	temp := struct {
		Type       string            `json:"type"`
		Properties map[string]string `json:"properties"`
	}{}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	newC, err := c.ParseCRS(temp.Properties["name"])
	if err != nil {
		return err
	}
	newerC := CRS{*newC}
	c = &newerC
	return nil
}

type Point struct {
	geometry.Point
}

type LineString struct {
	geometry.LineString
}

type Polygon struct {
	geometry.Polygon
}
type GeometryCollection struct {
	geometry.GeometryCollection
}

// UnmarshalGeometry dispatches to the correct concrete geometry type
func UnmarshalGeometry(data []byte) (geometry.Geometry, error) {
	type geomHeader struct {
		Type GeometryType `json:"type"`
	}
	var h geomHeader
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	switch h.Type {
	case PointType:
		var g Point
		return &g, g.UnmarshalJSON(data)
	case LineStringType:
		var g LineString
		return &g, g.UnmarshalJSON(data)
	case PolygonType:
		var g Polygon
		return &g, g.UnmarshalJSON(data)
	case GeometryCollectionType:
		var g GeometryCollection
		return &g, g.UnmarshalJSON(data)
	default:
		return nil, errors.New("unknown geometry type")
	}
}

func UnmarshalAs[T Geometry](data []byte) (T, error) {
	var t T
	err := t.UnmarshalJSON(data)
	return t, err
}

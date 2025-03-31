package geojson

import (
	"encoding/json"

	berrors "github.com/zoobst/gobi/bErrors"
)

// NewFeatureCollection creates and returns a new GeoJSONFeatureCollection.
// It initializes the Type field to "FeatureCollection" and creates an empty Features slice.
//
// Returns:
//   - A new GeoJSONFeatureCollection instance.
func NewFeatureCollection() GeoJSONFeatureCollection {
	return GeoJSONFeatureCollection{
		Type:     "FeatureCollection",
		Features: []GeoJSONFeature{},
	}
}

// AddFeature adds a new feature to the GeoJSONFeatureCollection.
//
// Parameters:
//   - properties: A map of string keys to any values representing the feature's properties.
//   - geometry: A GeoJSONGeometry object representing the feature's geometry.
//
// The method creates a new GeoJSONFeature with the provided properties and geometry,
// and appends it to the Features slice of the GeoJSONFeatureCollection.
func (fc *GeoJSONFeatureCollection) AddFeature(properties map[string]any, geometry GeoJSONGeometry) {
	feature := GeoJSONFeature{
		Type:       "Feature",
		Geometry:   geometry,
		Properties: properties,
	}
	fc.Features = append(fc.Features, feature)
}

// Create a new GeoJSON Point Feature
func NewGeoJSONPointGeometry(coordinates [2]float64) GeoJSONGeometry {
	return GeoJSONGeometry{
		Type:        "Point",
		Coordinates: coordinates,
	}
}

// Create a new GeoJSON Polygon Feature
func NewGeoJSONPolygonGeometry(coordinates [][][2]float64) GeoJSONGeometry {
	return GeoJSONGeometry{
		Type:        "Polygon",
		Coordinates: coordinates,
	}
}

// Create a new GeoJSON LineString Feature
func NewGeoJSONLineStringGeometry(coordinates [][][2]float64) GeoJSONGeometry {
	return GeoJSONGeometry{
		Type:        "LineString",
		Coordinates: coordinates,
	}
}

// MarshalGeoJSON converts the GeoJSONFeatureCollection to a JSON byte slice.
//
// Returns:
//   - A byte slice containing the JSON representation of the GeoJSONFeatureCollection.
//   - An error if the JSON marshaling fails, nil otherwise.
//
// This method uses the standard library's json.Marshal function to convert the
// GeoJSONFeatureCollection struct to JSON format.
func (fc *GeoJSONFeatureCollection) MarshalGeoJSON() ([]byte, error) {
	jsonData, err := json.Marshal(fc)
	if err != nil {
		return nil, err
	}
	return jsonData, nil
}

func MarshalGeoJSON[T GeoJSONGetter](items []T) ([]byte, error) {
	var (
		featureCollection = NewFeatureCollection()
	)

	if len(items) == 0 {
		return []byte{}, berrors.ErrEmptyArray
	}

	for _, item := range items {
		geometry := item.GeoJSONGeometry()
		properties := item.GeoJSONProperties()
		featureCollection.AddFeature(properties, geometry)
	}

	geoJson, err := featureCollection.MarshalGeoJSON()
	if err != nil {
		return []byte{}, err
	}

	return geoJson, nil
}

func UnmarshalGeoJSON[T *[]byte](buf T) (fc GeoJSONFeatureCollection, err error) {
	fc = NewFeatureCollection()

	if len(*buf) == 0 {
		return fc, berrors.ErrEmptyArray
	}

	err = json.Unmarshal(*buf, &fc)
	if err != nil {
		return fc, err
	}
	return fc, nil
}

func UnmarshalJSON[T []byte](data T) error {
	var featureCollection GeoJSONFeatureCollection
	if err := json.Unmarshal(data, &featureCollection); err != nil {
		return err
	}
	return nil
}

func (fc *GeoJSONFeatureCollection) UnmarshalJSON(data []byte) error {
	err := UnmarshalJSON(data)
	if err != nil {
		return err
	}
	return nil
}

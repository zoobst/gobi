package geojson

// GeoJSONFeatureCollection represents a collection of GeoJSON features
type GeoJSONFeatureCollection struct {
	// Type is always "FeatureCollection" for GeoJSON feature collections.
	Type     string           `json:"type"`
	Features []GeoJSONFeature `json:"features"`
}

// GeoJSONFeature represents a single feature in a GeoJSON structure
type GeoJSONFeature struct {
	Type       string          `json:"type"`
	Geometry   GeoJSONGeometry `json:"geometry"`
	Properties map[string]any  `json:"properties"`
}

// GeoJSONGeometry represents the geometry of a GeoJSON feature
type GeoJSONGeometry struct {
	Type        string         `json:"type"`
	Coordinates [][][2]float64 `json:"coordinates"`
}

// GeoJSONMarshaler is an interface for types that can marshal themselves into GeoJSON format.
type GeoJSONMarshaler interface {
	// MarshalGeoJSON converts the implementing type into a GeoJSON representation.
	// and an error if the marshaling process fails.
	MarshalGeoJSON() ([]byte, error)
}

// GeoJSONGetter is an interface for types that can provide GeoJSON properties and geometry.
type GeoJSONGetter interface {
	// GetProperties returns a map of properties for the GeoJSON feature.
	GeoJSONProperties() map[string]any
	// GetGeometry returns the geometry of the GeoJSON feature.
	GeoJSONGeometry() GeoJSONGeometry
}

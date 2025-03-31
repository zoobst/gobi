package globalTypes

import (
	"fmt"
	"hash/fnv"

	"github.com/zoobst/gobi/geojson"

	"github.com/apache/arrow/go/arrow"
	"github.com/apache/arrow/go/arrow/array"
)

type GeometryType struct{ Geometry Geometry }

func (g GeometryType) String() string { return g.Geometry.String() }

func (g *GeometryType) ID() arrow.Type { return arrow.EXTENSION }

func (g *GeometryType) Name() string { return g.Geometry.Name() }

func (g *GeometryType) StorageType(dt arrow.DataType) arrow.DataType {
	return g.Geometry.StorageType(dt)
}

func (g *GeometryType) Fingerprint() string {
	// Implement the Fingerprint method.

	h := fnv.New64a()
	h.Write([]byte(g.Name())) // Use name as part of the fingerprint.
	return string(h.Sum(nil))
}

func (g *GeometryType) Equal(other arrow.DataType) bool {
	//Compare the fingerprints.
	if other, ok := other.(*GeometryType); ok {
		return g.Fingerprint() == other.Fingerprint()
	}
	return false
}

func (g *GeometryType) Serialize() string { return g.Name() }

func (g *GeometryType) Deserialize(s string) arrow.DataType { return &GeometryType{} }

func (g *GeometryType) ExtensionName() string { return g.Name() }

func (g *GeometryType) ExtensionMetadata() string { return g.Geometry.ExtensionMetadata() }

func (g GeometryType) WKT() string { return g.Geometry.WKT() }

func (g GeometryType) Coords() (fList [][2]float64) { return g.Geometry.Coords() }

func (g GeometryType) MaxX() float64 { return g.Geometry.MaxX() }

func (g GeometryType) MaxY() float64 { return g.Geometry.MaxY() }

func (g GeometryType) MinX() float64 { return g.Geometry.MinX() }

func (g GeometryType) MinY() float64 { return g.Geometry.MinY() }

// GetGeometry returns the GeoJSON geometry representation of the geometry.
func (g GeometryType) GeoJSONGeometry() geojson.GeoJSONGeometry {
	return geojson.GeoJSONGeometry{
		Type:        g.Geometry.GeoJSONGeometry().Type,
		Coordinates: [][][2]float64{g.Geometry.Coords()},
	}
}

type GeometryArray struct {
	array.ExtensionArray
	listArray *array.List
}

func (g *GeometryType) NewArray(data array.Data) array.ExtensionArray {
	return &GeometryArray{
		listArray: array.NewListData(&data),
	}
}

func (a *GeometryArray) DataType() arrow.DataType { return &GeometryType{} }

func (a *GeometryArray) Data() *array.Data { return a.listArray.Data() }

func (a *GeometryArray) String() string { return fmt.Sprintf("%v", a.listArray) }

func (a *GeometryArray) ListValues() *array.List { return a.listArray }

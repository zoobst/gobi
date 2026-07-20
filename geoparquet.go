package gobi

import (
	"encoding/json"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/geometry"
)

// GeoParquetVersion is the GeoParquet spec version this package emits.
const (
	GeoParquetVersion = "1.1.0"
	// GeoParquetMetadataKey is the file-level metadata key used by GeoParquet.
	GeoParquetMetadataKey = "geo"
)

// GeoParquetMetadata is the JSON payload written under the "geo" key of a
// GeoParquet file's Arrow-level metadata.
type GeoParquetMetadata struct {
	Version       string                          `json:"version"`
	PrimaryColumn string                          `json:"primary_column"`
	Columns       map[string]GeoParquetColumnMeta `json:"columns"`
}

// GeoParquetColumnMeta is the per-column entry inside GeoParquetMetadata.
type GeoParquetColumnMeta struct {
	Encoding      string         `json:"encoding"`
	GeometryTypes []string       `json:"geometry_types"`
	CRS           map[string]any `json:"crs,omitempty"`
	Bbox          []float64      `json:"bbox,omitempty"`
}

// BuildGeoParquetMetadata scans f and produces a GeoParquet metadata blob
// describing its geometry columns. Every column tagged as a geometry column
// (via GeometryField) is scanned once to compute its bounding box and the
// set of geometry types it contains.
func BuildGeoParquetMetadata(f *Frame) (*GeoParquetMetadata, error) {
	meta := &GeoParquetMetadata{
		Version: GeoParquetVersion,
		Columns: map[string]GeoParquetColumnMeta{},
	}
	for _, s := range f.series {
		if !s.IsGeometry() {
			continue
		}
		if meta.PrimaryColumn == "" {
			meta.PrimaryColumn = s.name
		}
		col, err := describeGeometryColumn(s)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", s.name, err)
		}
		meta.Columns[s.name] = col
	}
	if meta.PrimaryColumn == "" {
		return nil, nil // no geometry columns; not a GeoParquet file
	}
	return meta, nil
}

func describeGeometryColumn(s Series) (GeoParquetColumnMeta, error) {
	col := GeoParquetColumnMeta{Encoding: "WKB"}
	epsg := geometryCRSFromField(s.field)
	col.CRS = crsPROJJSON(epsg)

	types := map[string]struct{}{}
	bounds := geometry.EmptyBounds()

	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return col, fmt.Errorf("%w: expected Binary, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return col, err
			}
			name := g.Type().String()
			if g.Is3D() {
				name += " Z"
			}
			types[name] = struct{}{}
			bounds = bounds.Union(g.Bounds())
		}
		offset += bin.Len()
	}

	col.GeometryTypes = sortedKeys(types)
	if !bounds.Empty() {
		col.Bbox = []float64{bounds.MinX, bounds.MinY, bounds.MaxX, bounds.MaxY}
	}
	return col, nil
}

// crsPROJJSON returns a minimal PROJJSON-shaped map for the given EPSG code.
// EPSG:4326 returns nil, which the GeoParquet spec treats as "OGC:CRS84
// implicit" — a common interop shortcut.
func crsPROJJSON(epsg int32) map[string]any {
	if epsg == 0 || epsg == 4326 {
		return nil
	}
	return map[string]any{
		"$schema": "https://proj.org/schemas/v0.5/projjson.schema.json",
		"type":    "GeographicCRS",
		"id": map[string]any{
			"authority": "EPSG",
			"code":      epsg,
		},
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small n; simple insertion sort keeps everything stable.
	for i := range len(out) {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ParseGeoParquetMetadata decodes the JSON blob under the "geo" key.
func ParseGeoParquetMetadata(raw string) (*GeoParquetMetadata, error) {
	var m GeoParquetMetadata
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// MarshalGeoParquetMetadata serializes meta to JSON.
func MarshalGeoParquetMetadata(meta *GeoParquetMetadata) (string, error) {
	blob, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

// GeoParquetSchemaWithMetadata returns a copy of schema with the given
// GeoParquet metadata injected under the "geo" key at the file level.
func GeoParquetSchemaWithMetadata(schema *arrow.Schema, meta *GeoParquetMetadata) (*arrow.Schema, error) {
	if meta == nil {
		return schema, nil
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	keys := []string{GeoParquetMetadataKey}
	values := []string{string(blob)}
	if schema.HasMetadata() {
		old := schema.Metadata()
		for i, k := range old.Keys() {
			if k == GeoParquetMetadataKey {
				continue // will be overwritten
			}
			keys = append(keys, k)
			values = append(values, old.Values()[i])
		}
	}
	md := arrow.NewMetadata(keys, values)
	return arrow.NewSchema(schema.Fields(), &md), nil
}

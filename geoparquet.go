package gobi

import (
	json "encoding/json/v2"
	"fmt"
	"math"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

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

	// Covering names the per-row bounding-box columns that a reader
	// can use for row-group pruning without decoding WKB. Populated
	// by parquetio.WriteFile when it emits companion bbox columns
	// (see BboxCoveringSuffixes). Follows the GeoParquet 1.1
	// "covering.bbox" spec:
	//
	//	{"xmin": ["<col>"], "ymin": ["<col>"], "xmax": ["<col>"], "ymax": ["<col>"]}
	//
	// A single-element path array means "top-level column with this
	// name" — gobi's flat-column approach — but consumers that
	// support nested struct-covering (geopandas, DuckDB spatial) read
	// it identically.
	Covering *GeoParquetCovering `json:"covering,omitempty"`
}

// GeoParquetCovering describes how to compute a row's bounding box
// from other columns in the same file. Only the bbox variant is
// used today.
type GeoParquetCovering struct {
	Bbox *GeoParquetBboxCovering `json:"bbox,omitempty"`
}

// GeoParquetBboxCovering names the four columns holding per-row
// bbox coordinates. Each is a path array (typically single-element
// for flat top-level columns).
type GeoParquetBboxCovering struct {
	Xmin []string `json:"xmin"`
	Ymin []string `json:"ymin"`
	Xmax []string `json:"xmax"`
	Ymax []string `json:"ymax"`
}

// BboxColumnNames returns the four flat column names gobi uses to
// hold per-row bbox coordinates for a geometry column named
// geomName. Kept in one place so writer, reader, and predicate
// pushdown agree on the naming convention.
func BboxColumnNames(geomName string) (xmin, ymin, xmax, ymax string) {
	base := geomName + "_bbox_"
	return base + "xmin", base + "ymin", base + "xmax", base + "ymax"
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

// WithBboxCoveringColumns returns a copy of f augmented with four
// Float64 columns per geometry column (xmin/ymin/xmax/ymax) plus a
// GeoParquetMetadata whose per-column Covering references them.
// This is the write-side half of predicate pushdown: readers use
// the covering columns' parquet-native min/max row-group statistics
// to skip whole row groups whose bboxes are disjoint from a
// spatial-predicate constant.
//
// Empty/null geometry rows produce NaN bbox values, which parquet
// stats treat as "outside the min/max range" — safe (we err on
// keeping the row group) rather than incorrectly proving disjointness.
//
// The returned frame retains its own reference to every column
// (original + new); callers own the Release, symmetric with
// NewFrame.
func WithBboxCoveringColumns(f *Frame) (*Frame, *GeoParquetMetadata, error) {
	meta, err := BuildGeoParquetMetadata(f)
	if err != nil {
		return nil, nil, err
	}
	if meta == nil {
		// No geometry columns → nothing to augment.
		f.Retain()
		return f, nil, nil
	}
	pool := memory.DefaultAllocator

	origFields := f.Schema().Fields()
	newFields := make([]arrow.Field, 0, len(origFields)+4*len(meta.Columns))
	newFields = append(newFields, origFields...)

	newCols := make([]arrow.Column, 0, len(origFields)+4*len(meta.Columns))
	for _, s := range f.series {
		newCols = append(newCols, *arrow.NewColumn(s.field, s.col.Data()))
	}
	// arrow.NewColumn Retains each ChunkedArray; on error we Release
	// everything so we don't leak. Track our progress separately.
	rollback := func() {
		for _, c := range newCols {
			c.Release()
		}
	}

	for _, s := range f.series {
		if !s.IsGeometry() {
			continue
		}
		xminName, yminName, xmaxName, ymaxName := BboxColumnNames(s.name)
		xmin, ymin, xmax, ymax, err := computeBboxColumns(pool, s)
		if err != nil {
			rollback()
			return nil, nil, fmt.Errorf("compute bbox for %q: %w", s.name, err)
		}
		for _, bc := range []struct {
			name string
			arr  arrow.Array
		}{
			{xminName, xmin},
			{yminName, ymin},
			{xmaxName, xmax},
			{ymaxName, ymax},
		} {
			field := arrow.Field{Name: bc.name, Type: arrow.PrimitiveTypes.Float64, Nullable: false}
			newFields = append(newFields, field)
			chunked := arrow.NewChunked(field.Type, []arrow.Array{bc.arr})
			newCols = append(newCols, *arrow.NewColumn(field, chunked))
			chunked.Release()
			bc.arr.Release()
		}

		cm := meta.Columns[s.name]
		cm.Covering = &GeoParquetCovering{
			Bbox: &GeoParquetBboxCovering{
				Xmin: []string{xminName},
				Ymin: []string{yminName},
				Xmax: []string{xmaxName},
				Ymax: []string{ymaxName},
			},
		}
		meta.Columns[s.name] = cm
	}

	augSchema := arrow.NewSchema(newFields, schemaMetadataPtr(f.Schema()))
	out, err := NewFrame(augSchema, newCols)
	if err != nil {
		rollback()
		return nil, nil, err
	}
	// NewFrame takes ownership of every column in newCols — the
	// returned Frame's Release will call Release on each. Do NOT run
	// rollback() here; that would double-release and leave the
	// underlying Chunked at refcount 0 by the time WriteTable sees it.
	return out, meta, nil
}

// computeBboxColumns walks s once and emits four aligned Float64
// arrays (xmin, ymin, xmax, ymax) — one entry per row. Null and
// empty geometries produce NaN so parquet stats surface them as
// "no useful range" rather than a lie about the row's coordinates.
func computeBboxColumns(pool memory.Allocator, s Series) (xmin, ymin, xmax, ymax arrow.Array, err error) {
	xminB := array.NewFloat64Builder(pool)
	defer xminB.Release()
	yminB := array.NewFloat64Builder(pool)
	defer yminB.Release()
	xmaxB := array.NewFloat64Builder(pool)
	defer xmaxB.Release()
	ymaxB := array.NewFloat64Builder(pool)
	defer ymaxB.Release()

	nan := math.NaN()
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("%w: expected Binary, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				xminB.Append(nan)
				yminB.Append(nan)
				xmaxB.Append(nan)
				ymaxB.Append(nan)
				continue
			}
			g, perr := geometry.ParseWKB(bin.Value(i))
			if perr != nil {
				return nil, nil, nil, nil, perr
			}
			b := g.Bounds()
			if b.Empty() {
				xminB.Append(nan)
				yminB.Append(nan)
				xmaxB.Append(nan)
				ymaxB.Append(nan)
				continue
			}
			xminB.Append(b.MinX)
			yminB.Append(b.MinY)
			xmaxB.Append(b.MaxX)
			ymaxB.Append(b.MaxY)
		}
	}
	return xminB.NewArray(), yminB.NewArray(), xmaxB.NewArray(), ymaxB.NewArray(), nil
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

// crsPROJJSON returns a canonical PROJJSON object for the given EPSG code,
// suitable for embedding in a GeoParquet "geo" metadata blob. Returns nil
// for:
//
//   - EPSG 0 (unset) or 4326 (WGS84): the GeoParquet spec treats a null
//     crs as OGC:CRS84 implicit, matching what geographic-CRS data
//     already means.
//   - EPSG codes gobi doesn't have PROJJSON tables for. Downstream
//     readers will treat these files as CRS-unknown, which is closer
//     to correct than emitting invalid PROJJSON that pyproj rejects.
//
// The canonical PROJJSON comes from pyproj at authoring time and is
// embedded in [geometry/projjson_data.json] — that guarantees
// bit-compatible round-trips through geopandas without shipping a
// hand-rolled minimal blob that pyproj won't parse.
func crsPROJJSON(epsg int32) map[string]any {
	if epsg == 0 || epsg == 4326 {
		return nil
	}
	return geometry.PROJJSONFor(epsg)
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

package gobi

import (
	"encoding/json"
	"testing"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

func geoFrame(t *testing.T, epsg int32) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	names := array.NewStringBuilder(pool)
	defer names.Release()
	names.AppendValues([]string{"a", "b", "c"}, nil)
	geoms := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geoms.Release()
	points := []geometry.Point{
		{X: 0, Y: 0}, {X: 5, Y: 3}, {X: -1, Y: 2},
	}
	for _, p := range points {
		geoms.Append(geometry.WKB(p))
	}
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", epsg),
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{names.NewArray(), geoms.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBuildGeoParquetMetadata(t *testing.T) {
	f := geoFrame(t, 4326)
	meta, err := BuildGeoParquetMetadata(f)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("nil metadata")
	}
	if meta.Version != GeoParquetVersion {
		t.Errorf("version = %q", meta.Version)
	}
	if meta.PrimaryColumn != "geometry" {
		t.Errorf("primary = %q", meta.PrimaryColumn)
	}
	col, ok := meta.Columns["geometry"]
	if !ok {
		t.Fatal("no geometry entry")
	}
	if col.Encoding != "WKB" {
		t.Errorf("encoding = %q", col.Encoding)
	}
	if len(col.GeometryTypes) != 1 || col.GeometryTypes[0] != "Point" {
		t.Errorf("geometry_types = %v", col.GeometryTypes)
	}
	if col.CRS != nil {
		t.Errorf("EPSG:4326 should map to nil CRS (implicit OGC:CRS84), got %v", col.CRS)
	}
	// bbox is (-1, 0, 5, 3)
	want := []float64{-1, 0, 5, 3}
	if len(col.Bbox) != 4 {
		t.Fatalf("bbox = %v", col.Bbox)
	}
	for i, v := range want {
		if col.Bbox[i] != v {
			t.Errorf("bbox[%d] = %v, want %v", i, col.Bbox[i], v)
		}
	}
}

func TestBuildGeoParquetMetadata_CRSNon4326(t *testing.T) {
	f := geoFrame(t, 3857)
	meta, _ := BuildGeoParquetMetadata(f)
	col := meta.Columns["geometry"]
	if col.CRS == nil {
		t.Fatal("expected non-nil CRS for EPSG:3857")
	}
	if id, ok := col.CRS["id"].(map[string]any); !ok || id["code"] != int32(3857) {
		t.Errorf("crs id = %v", col.CRS["id"])
	}
}

func TestBuildGeoParquetMetadata_NoGeometryReturnsNil(t *testing.T) {
	f := smallFrame(t) // 'geom' column is BINARY but not tagged with metadata.
	// Explicitly untag the geometry column by using a plain field.
	// smallFrame does tag geom via GeometryField though — build a truly
	// non-geometry frame instead.

	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues([]int64{1, 2, 3}, nil)
	fields := []arrow.Field{{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: false}}
	schema := arrow.NewSchema(fields, nil)
	arr := b.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	cols := []arrow.Column{*arrow.NewColumn(fields[0], chunked)}
	plain, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := BuildGeoParquetMetadata(plain)
	if err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		t.Errorf("expected nil metadata for non-geo frame, got %+v", meta)
	}
	_ = f
}

func TestBuildGeoParquetMetadata_MixedDimGeomTypes(t *testing.T) {
	pool := memory.DefaultAllocator
	names := array.NewStringBuilder(pool)
	defer names.Release()
	names.AppendValues([]string{"a", "b"}, nil)
	geoms := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geoms.Release()
	// One 2D point + one 3D point in the same column.
	geoms.Append(geometry.WKB(geometry.Point{X: 0, Y: 0}))
	geoms.Append(geometry.WKB(geometry.Point{X: 1, Y: 2, Z: 3, HasZ: true}))

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		GeometryField("geometry", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{names.NewArray(), geoms.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := BuildGeoParquetMetadata(f)
	if err != nil {
		t.Fatal(err)
	}
	types := meta.Columns["geometry"].GeometryTypes
	if len(types) != 2 {
		t.Fatalf("expected 2 geometry_types entries, got %v", types)
	}
	sawPoint := false
	sawPointZ := false
	for _, s := range types {
		if s == "Point" {
			sawPoint = true
		}
		if s == "Point Z" {
			sawPointZ = true
		}
	}
	if !sawPoint || !sawPointZ {
		t.Fatalf("expected both 'Point' and 'Point Z' in geometry_types, got %v", types)
	}
}

func TestGeoParquetSchemaWithMetadata_RoundTripJSON(t *testing.T) {
	f := geoFrame(t, 4326)
	meta, _ := BuildGeoParquetMetadata(f)

	newSchema, err := GeoParquetSchemaWithMetadata(f.Schema(), meta)
	if err != nil {
		t.Fatal(err)
	}
	md := newSchema.Metadata()
	raw, ok := md.GetValue(GeoParquetMetadataKey)
	if !ok {
		t.Fatal("no geo key in schema metadata")
	}
	back, err := ParseGeoParquetMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.PrimaryColumn != "geometry" {
		t.Errorf("primary = %q", back.PrimaryColumn)
	}

	// Confirm valid JSON with the expected shape.
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != GeoParquetVersion {
		t.Errorf("version %v", m["version"])
	}
}

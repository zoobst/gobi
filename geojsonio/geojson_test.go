package geojsonio_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zoobst/gobi/geojsonio"
	"github.com/zoobst/gobi/geometry"
)

func TestPointRoundTrip(t *testing.T) {
	p := geometry.Point{X: 10, Y: 20, CRSValue: geometry.WGS84}
	buf, err := geojsonio.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(geometry.Point)
	if got.X != 10 || got.Y != 20 {
		t.Fatalf("point: %+v", got)
	}
}

func TestPolygonRoundTrip(t *testing.T) {
	poly := geometry.Polygon{
		Rings: [][]geometry.Point{
			{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0}},
		},
		CRSValue: geometry.WGS84,
	}
	buf, err := geojsonio.Marshal(poly)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojsonio.Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(geometry.Polygon)
	if len(got.Rings) != 1 || len(got.Rings[0]) != 5 {
		t.Fatalf("polygon rings: %+v", got.Rings)
	}
}

func TestUnmarshalFeature(t *testing.T) {
	src := []byte(`{
		"type": "Feature",
		"geometry": {"type": "Point", "coordinates": [-73.9857, 40.7484]},
		"properties": {"name": "Empire State"}
	}`)
	g, props, err := geojsonio.UnmarshalFeature(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(geometry.Point); !ok {
		t.Fatalf("expected Point, got %T", g)
	}
	if props["name"] != "Empire State" {
		t.Fatalf("props: %v", props)
	}
}

func TestUnmarshalInvalid(t *testing.T) {
	// GeometryTriangle isn't a GeoJSON type, so the dispatcher
	// rejects the input. MultiPolygon USED to be the canonical
	// "unsupported type" example — as of the geojsonio expansion
	// it's a first-class type, so this test picks a genuinely
	// invalid name instead.
	_, err := geojsonio.Unmarshal([]byte(`{"type":"GeometryTriangle","coordinates":[]}`))
	if !errors.Is(err, geojsonio.ErrInvalidGeoJSON) {
		t.Fatalf("expected ErrInvalidGeoJSON, got %v", err)
	}
}

func TestMarshalJSON_Structure(t *testing.T) {
	p := geometry.Point{X: 1, Y: 2}
	buf, _ := geojsonio.Marshal(p)
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "Point" {
		t.Fatalf("type: %v", out["type"])
	}
}

package geojson_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zoobst/gobi/geojson"
	"github.com/zoobst/gobi/geometry"
)

func TestPointRoundTrip(t *testing.T) {
	p := geometry.Point{X: 10, Y: 20, CRSValue: geometry.WGS84}
	buf, err := geojson.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojson.Unmarshal(buf)
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
	buf, err := geojson.Marshal(poly)
	if err != nil {
		t.Fatal(err)
	}
	back, err := geojson.Unmarshal(buf)
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
	g, props, err := geojson.UnmarshalFeature(src)
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
	_, err := geojson.Unmarshal([]byte(`{"type":"MultiPolygon","coordinates":[]}`))
	if !errors.Is(err, geojson.ErrInvalidGeoJSON) {
		t.Fatalf("expected ErrInvalidGeoJSON, got %v", err)
	}
}

func TestMarshalJSON_Structure(t *testing.T) {
	p := geometry.Point{X: 1, Y: 2}
	buf, _ := geojson.Marshal(p)
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "Point" {
		t.Fatalf("type: %v", out["type"])
	}
}

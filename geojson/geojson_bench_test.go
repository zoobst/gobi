package geojson_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zoobst/gobi/geojson"
	"github.com/zoobst/gobi/geometry"
)

// buildFeatureCollection returns a valid FeatureCollection JSON blob
// with n Point features, each of the shape:
//
//	{"type":"Feature",
//	 "geometry":{"type":"Point","coordinates":[LON,LAT]},
//	 "properties":{"id":N,"cat":"kK"}}
//
// Roughly ~110 bytes per feature; for n=1_000_000 that's ~105 MB.
func buildFeatureCollection(n int) []byte {
	var b strings.Builder
	b.Grow(n * 120)
	b.WriteString(`{"type":"FeatureCollection","features":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		// Cycle lon/lat through valid WGS 84 ranges so the JSON parses
		// as real coordinates, and give properties a couple of typed
		// fields so the parser can't shortcut to skipping the object.
		lon := float64((i%360)-180) + 0.5
		lat := float64((i%180)-90) + 0.5
		fmt.Fprintf(&b,
			`{"type":"Feature","geometry":{"type":"Point","coordinates":[%.1f,%.1f]},"properties":{"id":%d,"cat":"k%d"}}`,
			lon, lat, i, i%100)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// featureCollection1M is built once and reused across benchmarks. The
// fixture is ~105 MB — sync.OnceValue means we only pay to construct
// it if a benchmark actually runs.
var featureCollection1M = sync.OnceValue(func() []byte {
	return buildFeatureCollection(1_000_000)
})

// A single Feature blob, matching the shape of one row inside
// featureCollection1M. Used for the per-feature micro-benchmark.
var singleFeature = []byte(
	`{"type":"Feature","geometry":{"type":"Point","coordinates":[-74.006,40.7128]},"properties":{"id":1,"cat":"nyc"}}`,
)

// Sinks prevent the compiler from eliminating the decoded results.
var (
	sinkGeom  geometry.Geometry
	sinkProps map[string]any
)

// BenchmarkGeoJSON_UnmarshalFeature_Single decodes one Feature blob.
// This is the tightest micro-benchmark — good for spotting per-call
// overhead in json.Unmarshal + geometry construction.
func BenchmarkGeoJSON_UnmarshalFeature_Single(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(singleFeature)))
	for b.Loop() {
		g, props, err := geojson.UnmarshalFeature(singleFeature)
		if err != nil {
			b.Fatal(err)
		}
		sinkGeom = g
		sinkProps = props
	}
}

// BenchmarkGeoJSON_FeatureCollection_1M decodes an entire 1M-feature
// FeatureCollection blob (~105 MB) in one json.Unmarshal call, then
// walks the resulting features and decodes each geometry. This is the
// user-facing pattern for "load a whole GeoJSON file and iterate":
//
//	var fc struct { Features []geojson.Feature `json:"features"` }
//	json.Unmarshal(data, &fc)
//	for _, f := range fc.Features {
//	    g, _ := geojson.Unmarshal(f.Geometry)
//	    ...
//	}
//
// b.SetBytes reports MB/s so you can compare against json/v2 or any
// alternative decoder later.
func BenchmarkGeoJSON_FeatureCollection_1M(b *testing.B) {
	data := featureCollection1M()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		var fc struct {
			Type     string             `json:"type"`
			Features []geojson.Feature  `json:"features"`
		}
		if err := json.Unmarshal(data, &fc); err != nil {
			b.Fatal(err)
		}
		for i := range fc.Features {
			g, err := geojson.Unmarshal(fc.Features[i].Geometry)
			if err != nil {
				b.Fatal(err)
			}
			sinkGeom = g
		}
	}
}

// BenchmarkGeoJSON_FeatureCollection_1M_OuterOnly decodes only the
// outer FeatureCollection into []geojson.Feature — it does NOT walk
// each feature's Geometry field. This isolates the encoding/json cost
// of parsing the ~105 MB blob from the downstream geometry decoding
// work, so we can attribute the total time to (outer decode) +
// (per-geometry decode) separately.
func BenchmarkGeoJSON_FeatureCollection_1M_OuterOnly(b *testing.B) {
	data := featureCollection1M()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		var fc struct {
			Type     string             `json:"type"`
			Features []geojson.Feature  `json:"features"`
		}
		if err := json.Unmarshal(data, &fc); err != nil {
			b.Fatal(err)
		}
		if len(fc.Features) != 1_000_000 {
			b.Fatalf("features = %d, want 1_000_000", len(fc.Features))
		}
	}
}

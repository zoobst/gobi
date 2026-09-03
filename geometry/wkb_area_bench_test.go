package geometry

import (
	"testing"
)

var wkbAreaBenchSizes = []int{5, 64, 1_024, 65_536, 1_000_000}

// BenchmarkPlanarArea_ParseWKB_AoS — pre-Slice-7 baseline. Each iter
// does ParseWKB (allocates full geometry) → .Area(UnitMeters) on a
// projected CRS (planar shoelace).
func BenchmarkPlanarArea_ParseWKB_AoS(b *testing.B) {
	projected := CRS{EPSG: 3857, Projected: true}
	for _, n := range wkbAreaBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for i := 0; i < b.N; i++ {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				p := g.(Polygon)
				p.CRSValue = projected
				a, err := p.Area(UnitMeters)
				if err != nil {
					b.Fatal(err)
				}
				sink += a
			}
			_ = sink
		})
	}
}

// BenchmarkPlanarArea_FromWKB_SoA — Slice-7 fast path. One
// byte-stream scan with inline shoelace accumulator; alloc-free.
func BenchmarkPlanarArea_FromWKB_SoA(b *testing.B) {
	for _, n := range wkbAreaBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for i := 0; i < b.N; i++ {
				a, err := PlanarAreaFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink += a
			}
			_ = sink
		})
	}
}

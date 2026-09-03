package geometry

import (
	"testing"
)

// Sizes match the wkb_bounds_bench_test.go / wkb_centroid_bench_test.go
// grid so cross-slice comparisons are apples-to-apples.
var wkbLengthBenchSizes = []int{5, 64, 1_024, 65_536, 1_000_000}

// BenchmarkPlanarLength_ParseWKB_AoS — pre-Slice-7 baseline. Each
// iter does ParseWKB (allocates full geometry) → .Length() (walks
// []Point pairs and computes Euclidean segment sums). The
// alloc profile shows the full geometry allocation.
func BenchmarkPlanarLength_ParseWKB_AoS(b *testing.B) {
	projected := CRS{EPSG: 3857, Projected: true}
	for _, n := range wkbLengthBenchSizes {
		data := makeLineStringWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for i := 0; i < b.N; i++ {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				ls := g.(LineString)
				ls.CRSValue = projected
				l, err := ls.Length(UnitMeters)
				if err != nil {
					b.Fatal(err)
				}
				sink += l
			}
			_ = sink
		})
	}
}

// BenchmarkPlanarLength_FromWKB_SoA — Slice-7 fast path. One
// byte-stream scan; alloc-free.
func BenchmarkPlanarLength_FromWKB_SoA(b *testing.B) {
	for _, n := range wkbLengthBenchSizes {
		data := makeLineStringWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for i := 0; i < b.N; i++ {
				l, err := PlanarLengthFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink += l
			}
			_ = sink
		})
	}
}

package geometry

import (
	"math/rand"
	"testing"
)

// Sizes covering the WKB parse-and-bounds workload spectrum:
//
//	n=5     — a closed unit square (5 vertices)
//	n=64    — hand-drawn AOI polygon
//	n=1_024 — mid-detail admin boundary
//	n=65_536 — coastline / high-res boundary
//	n=1_000_000 — full-detail coastline chunk
var wkbBoundsBenchSizes = []int{5, 64, 1_024, 65_536, 1_000_000}

// makeLineStringWKB builds a WKB blob for an n-point LineString with
// deterministic randomly-placed vertices. Same seed → same bytes,
// so the AoS and SoA benches process identical inputs.
func makeLineStringWKB(n int) []byte {
	rng := rand.New(rand.NewSource(int64(n)))
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}
	return WKB(LineString{Points: pts})
}

// makePolygonWKB builds a WKB blob for a Polygon with one exterior
// ring of n vertices. Useful for exercising the polygon scan path
// (extra 4-byte per-ring header).
func makePolygonWKB(n int) []byte {
	rng := rand.New(rand.NewSource(int64(n)))
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}
	return WKB(Polygon{Rings: [][]Point{pts}})
}

// BenchmarkBounds_ParseWKB_AoS — pre-Slice-2 baseline. Each iter
// does ParseWKB (allocates full geometry) → .Bounds() (walks
// []Point). Alloc profile shows the full geometry allocation.
func BenchmarkBounds_ParseWKB_AoS(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makeLineStringWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = g.Bounds()
			}
			_ = sink
		})
	}
}

// BenchmarkBounds_FromWKB_SoA — Slice-2 fast path. One byte-stream
// scan with min/max accumulators, no allocation.
func BenchmarkBounds_FromWKB_SoA(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makeLineStringWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				got, err := BoundsFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = got
			}
			_ = sink
		})
	}
}

// BenchmarkBounds_ParseWKB_AoS_Polygon and _FromWKB_SoA_Polygon
// — same comparison on the Polygon shape (extra ring header per
// input adds a small per-input constant that shouldn't affect
// the delta materially, but worth measuring to confirm).
func BenchmarkBounds_ParseWKB_AoS_Polygon(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = g.Bounds()
			}
			_ = sink
		})
	}
}

func BenchmarkBounds_FromWKB_SoA_Polygon(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				got, err := BoundsFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = got
			}
			_ = sink
		})
	}
}

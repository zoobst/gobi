package geometry

import (
	"math/rand"
	"testing"
)

// Same size ladder as the Slice 2 bounds bench so the AoS→SoA
// delta at each size is directly comparable between the two.
var wkbCentroidBenchSizes = []int{5, 64, 1_024, 65_536, 1_000_000}

func makeLineStringForCentroid(n int) []byte {
	rng := rand.New(rand.NewSource(int64(n) ^ 0xC0FFEE))
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}
	return WKB(LineString{Points: pts})
}

func makePolygonForCentroid(n int) []byte {
	rng := rand.New(rand.NewSource(int64(n) ^ 0xF00D))
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}
	return WKB(Polygon{Rings: [][]Point{pts}})
}

// BenchmarkCentroid_ParseWKB_AoS — pre-Slice-3 baseline. Each iter
// does ParseWKB (allocates full geometry) → g.Centroid() (walks
// []Point with weighted-midpoint formula for LineString).
func BenchmarkCentroid_ParseWKB_AoS(b *testing.B) {
	for _, n := range wkbCentroidBenchSizes {
		data := makeLineStringForCentroid(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Point
			for b.Loop() {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = g.Centroid()
			}
			_ = sink
		})
	}
}

// BenchmarkCentroid_FromWKB_SoA — Slice-3 fast path. Zero-alloc
// byte-stream scan producing the same centroid.
func BenchmarkCentroid_FromWKB_SoA(b *testing.B) {
	for _, n := range wkbCentroidBenchSizes {
		data := makeLineStringForCentroid(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Point
			for b.Loop() {
				got, err := CentroidFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = got
			}
			_ = sink
		})
	}
}

// BenchmarkCentroid_ParseWKB_AoS_Polygon / _FromWKB_SoA_Polygon
// — same shape on Polygon. The Polygon centroid formula is
// heavier per point than LineString (shoelace + area-weighting)
// so the SoA path's fixed-cost overhead is diluted more.
func BenchmarkCentroid_ParseWKB_AoS_Polygon(b *testing.B) {
	for _, n := range wkbCentroidBenchSizes {
		data := makePolygonForCentroid(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Point
			for b.Loop() {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = g.Centroid()
			}
			_ = sink
		})
	}
}

func BenchmarkCentroid_FromWKB_SoA_Polygon(b *testing.B) {
	for _, n := range wkbCentroidBenchSizes {
		data := makePolygonForCentroid(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Point
			for b.Loop() {
				got, err := CentroidFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink = got
			}
			_ = sink
		})
	}
}

// BenchmarkCentroidAndBounds_Fused_AoS — the pre-Slice-3
// HilbertSortWithCovering shape: parse once, ask for both
// centroid AND bounds. This is the primary end-to-end target
// since the fused write path is the hottest downstream consumer.
func BenchmarkCentroidAndBounds_Fused_AoS(b *testing.B) {
	for _, n := range wkbCentroidBenchSizes {
		data := makePolygonForCentroid(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sinkC Point
			var sinkB Bounds
			for b.Loop() {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sinkC = g.Centroid()
				sinkB = g.Bounds()
			}
			_ = sinkC
			_ = sinkB
		})
	}
}

// BenchmarkCentroidAndBounds_Fused_SoA — post-Slice-3 fused
// scanner. Single byte-stream pass returns both centroid and
// bounds. The delta on this bench maps directly onto the
// HilbertSortWithCovering wall-time win.
func BenchmarkCentroidAndBounds_Fused_SoA(b *testing.B) {
	for _, n := range wkbCentroidBenchSizes {
		data := makePolygonForCentroid(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sinkC Point
			var sinkB Bounds
			for b.Loop() {
				c, bb, err := CentroidAndBoundsFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sinkC = c
				sinkB = bb
			}
			_ = sinkC
			_ = sinkB
		})
	}
}

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

var wkbAreaBenchSizes = []int{5, 64, 1_024, 65_536, 1_000_000}

// BenchmarkPlanarArea_ParseWKB_AoS — pre-Slice-7 baseline. Each iter
// does ParseWKB (allocates full geometry) → .Area(UnitMeters) on a
// projected CRS (planar shoelace).
func BenchmarkPlanarArea_ParseWKB_AoS(b *testing.B) {
	projected := CRS{EPSG: 3857}
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

// Sizes match the wkb_bounds_bench_test.go / wkb_centroid_bench_test.go
// grid so cross-slice comparisons are apples-to-apples.
var wkbLengthBenchSizes = []int{5, 64, 1_024, 65_536, 1_000_000}

// BenchmarkPlanarLength_ParseWKB_AoS — pre-Slice-7 baseline. Each
// iter does ParseWKB (allocates full geometry) → .Length() (walks
// []Point pairs and computes Euclidean segment sums). The
// alloc profile shows the full geometry allocation.
func BenchmarkPlanarLength_ParseWKB_AoS(b *testing.B) {
	projected := CRS{EPSG: 3857}
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

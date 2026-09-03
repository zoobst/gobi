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

package geometry

import (
	"testing"
)

// BenchmarkPolygonRingViews_ParseWKB_AoS — baseline. Each iter
// does ParseWKB → RingViews(). Two allocations per ring: []Point
// during ParseWKB, then []float64 pair during RingViews.
func BenchmarkPolygonRingViews_ParseWKB_AoS(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				views := g.(Polygon).RingViews()
				sink += len(views)
			}
			_ = sink
		})
	}
}

// BenchmarkPolygonRingViews_FromWKB_SoA — Slice-10 direct-parse.
// Single pass over the WKB blob into Xs / Ys slabs; skips the
// []Point intermediate.
func BenchmarkPolygonRingViews_FromWKB_SoA(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				views, err := PolygonRingViewsFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink += len(views)
			}
			_ = sink
		})
	}
}

// BenchmarkPrepareFromWKB — end-to-end: measures the full path
// callers building PreparedGeometry from WKB take. Keeps the
// ParseWKB copy so TestPrepared's non-fast-path fall-through
// still works — compare against Prepare(ParseWKB(data)) below.
func BenchmarkPrepare_ParseWKB_AoS(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				g, err := ParseWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				p := Prepare(g)
				sink += len(p.polyRings)
			}
			_ = sink
		})
	}
}

func BenchmarkPrepare_FromWKB_SoA(b *testing.B) {
	for _, n := range wkbBoundsBenchSizes {
		data := makePolygonWKB(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink int
			for i := 0; i < b.N; i++ {
				p, err := PrepareFromWKB(data)
				if err != nil {
					b.Fatal(err)
				}
				sink += len(p.polyRings)
			}
			_ = sink
		})
	}
}

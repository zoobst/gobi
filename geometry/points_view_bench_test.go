package geometry

import (
	"math/rand"
	"testing"
)

// Bench sizes covering the range of realistic geometry footprints:
//
//   - 4:    a bbox rectangle (5 vertices — closed square)
//   - 64:   a hand-drawn AOI polygon
//   - 1_024: a mid-detail admin boundary
//   - 65_536: a coastline / high-res boundary
//   - 1_000_000: worst-case (a full-detail GSHHS coastline chunk)
//
// AoS vs SoA behavior changes materially across this range —
// small geometries pay conversion overhead, large ones amortize.
var pointsViewBenchSizes = []int{4, 64, 1_024, 65_536, 1_000_000}

// buildLineString returns a LineString with n deterministic points
// scattered across a 1000×1000 plane. Same seed → same points, so
// AoS vs SoA runs compute over identical input.
func buildLineString(n int) LineString {
	rng := rand.New(rand.NewSource(int64(n)))
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
	}
	return LineString{Points: pts, CRSValue: WGS84}
}

// BenchmarkLineStringBounds_AoS measures the current in-tree bounds
// implementation (LineString.Bounds → for _, p := range Points →
// Bounds.Extend). This is the pre-Slice-1 baseline.
func BenchmarkLineStringBounds_AoS(b *testing.B) {
	for _, n := range pointsViewBenchSizes {
		l := buildLineString(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				sink = l.Bounds()
			}
			_ = sink
		})
	}
}

// BenchmarkLineStringBounds_SoA_ViewThenBounds — the SoA pipeline
// including fresh view materialization on every call. Measures the
// break-even point where View() + BoundsFromXY beats the AoS loop
// despite paying the O(n) conversion tax up front.
func BenchmarkLineStringBounds_SoA_ViewThenBounds(b *testing.B) {
	for _, n := range pointsViewBenchSizes {
		l := buildLineString(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				sink = l.View().Bounds()
			}
			_ = sink
		})
	}
}

// BenchmarkLineStringBounds_SoA_HeldView — the amortized-SoA path:
// materialize once, run bounds many times. Measures the raw
// SoA-Bounds kernel win vs AoS-Bounds on the same input. This is
// the shape hot paths that hold a PointsView across multiple ops
// will see (Slices 3-5 target this).
func BenchmarkLineStringBounds_SoA_HeldView(b *testing.B) {
	for _, n := range pointsViewBenchSizes {
		l := buildLineString(n)
		v := l.View()
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			var sink Bounds
			for i := 0; i < b.N; i++ {
				sink = v.Bounds()
			}
			_ = sink
		})
	}
}

// BenchmarkPointsView_Materialize isolates the AoS→SoA conversion
// cost. Downstream slicing decisions (do we cache the view? do we
// parse WKB directly into SoA?) hinge on this number relative to
// the win from operating on the view.
func BenchmarkPointsView_Materialize(b *testing.B) {
	for _, n := range pointsViewBenchSizes {
		l := buildLineString(n)
		b.Run(sizeLabel(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = l.View()
			}
		})
	}
}

func sizeLabel(n int) string {
	switch n {
	case 4:
		return "n=4"
	case 5:
		return "n=5"
	case 8:
		return "n=8"
	case 16:
		return "n=16"
	case 64:
		return "n=64"
	case 256:
		return "n=256"
	case 1_024:
		return "n=1K"
	case 65_536:
		return "n=64K"
	case 1_000_000:
		return "n=1M"
	}
	return "n=?"
}

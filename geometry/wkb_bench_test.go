package geometry

import (
	"math"
	"testing"
)

var (
	sinkGeom Geometry
	sinkBuf  []byte
)

// buildBenchPolygon returns a closed polygon with n exterior points arranged
// around a circle, for realistic WKB-decode benchmarking.
func buildBenchPolygon(n int) Polygon {
	pts := make([]Point, 0, n+1)
	for i := 0; i < n; i++ {
		θ := float64(i) * 2 * math.Pi / float64(n)
		pts = append(pts, Point{X: 100 + 10*math.Cos(θ), Y: 50 + 10*math.Sin(θ)})
	}
	pts = append(pts, pts[0])
	return SimplePolygon(pts, WGS84)
}

func BenchmarkParseWKB_Point(b *testing.B) {
	buf := WKB(Point{X: -73.9857, Y: 40.7484})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		g, err := ParseWKB(buf)
		if err != nil {
			b.Fatal(err)
		}
		sinkGeom = g
	}
}

func BenchmarkParseWKB_Polygon100(b *testing.B) {
	buf := WKB(buildBenchPolygon(100))
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		g, err := ParseWKB(buf)
		if err != nil {
			b.Fatal(err)
		}
		sinkGeom = g
	}
}

func BenchmarkAppendWKB_Polygon100(b *testing.B) {
	poly := buildBenchPolygon(100)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		sinkBuf = WKB(poly)
	}
}

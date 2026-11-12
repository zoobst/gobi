package geometry

import "testing"

// These benchmarks cover the operations that go through the
// trigonometric functions, which geometry takes from internal/detmath
// (the same bits on every platform) instead of math.

func BenchmarkBuffer_RoundPoint(b *testing.B) {
	p := Point{X: 500000, Y: 4500000}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Buffer(p, 100, BufferOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuffer_RoundLine(b *testing.B) {
	l := LineString{}
	for i := range 200 {
		l.Points = append(l.Points, Point{X: float64(i) * 10, Y: float64((i*37)%11) * 5})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Buffer(l, 3, BufferOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProject_WGS84ToUTM(b *testing.B) {
	utm, err := LookupCRS(32633)
	if err != nil {
		b.Fatal(err)
	}
	l := LineString{CRSValue: WGS84}
	for i := range 1000 {
		l.Points = append(l.Points, Point{X: 12 + float64(i)/400, Y: 40 + float64(i%300)/100})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Project(l, utm); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProject_UTMToWGS84(b *testing.B) {
	utm, err := LookupCRS(32633)
	if err != nil {
		b.Fatal(err)
	}
	l := LineString{CRSValue: utm}
	for i := range 1000 {
		l.Points = append(l.Points, Point{X: 300000 + float64(i)*400, Y: 4400000 + float64(i%300)*1000})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Project(l, WGS84); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProject_WGS84ToWebMercator(b *testing.B) {
	l := LineString{CRSValue: WGS84}
	for i := range 1000 {
		l.Points = append(l.Points, Point{X: -170 + float64(i)/3, Y: -80 + float64(i%160)})
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Project(l, PseudoMercator); err != nil {
			b.Fatal(err)
		}
	}
}

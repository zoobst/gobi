package geojsonio_test

import (
	"fmt"
	"io"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geojsonio"
	"github.com/zoobst/gobi/geometry"
)

// writeBenchFrame builds n rows with an id, a category, a value and a
// geometry: a point, or a 5-vertex polygon.
func writeBenchFrame(tb testing.TB, n int, polygons bool) *gobi.Frame {
	tb.Helper()
	ids := make([]int64, n)
	kinds := make([]string, n)
	vals := make([]float64, n)
	gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for i := range n {
		ids[i] = int64(i)
		kinds[i] = fmt.Sprintf("kind-%d", i%7)
		vals[i] = float64(i%1000) / 8
		x, y := float64(i%360)-180+0.123456, float64(i%170)-85+0.654321
		if polygons {
			gb.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
				{X: x, Y: y}, {X: x + 0.1, Y: y}, {X: x + 0.1, Y: y + 0.1}, {X: x, Y: y + 0.1}, {X: x, Y: y},
			}, geometry.WGS84)))
		} else {
			gb.Append(geometry.WKB(geometry.Point{X: x, Y: y}))
		}
	}
	f, err := gobi.NewFrameFromSeries(
		gobi.NewInt64Series("id", ids, nil),
		gobi.NewStringSeries("kind", kinds, nil),
		gobi.NewFloat64Series("value", vals, nil),
		// SeriesFromArray takes ownership of the array.
		gobi.SeriesFromArray(gobi.GeometryField("geometry", 4326), gb.NewArray()),
	)
	if err != nil {
		tb.Fatal(err)
	}
	return f
}

// BenchmarkWrite measures Write on 100k features of each shape.
func BenchmarkWrite(b *testing.B) {
	for _, c := range []struct {
		name     string
		polygons bool
		format   geojsonio.Format
	}{
		{"points", false, geojsonio.FormatFeatureCollection},
		{"polygons", true, geojsonio.FormatFeatureCollection},
		{"points_lines", false, geojsonio.FormatLineDelimited},
	} {
		b.Run(c.name, func(b *testing.B) {
			f := writeBenchFrame(b, 100_000, c.polygons)
			defer f.Release()
			b.ReportAllocs()
			for b.Loop() {
				if err := geojsonio.Write(f, io.Discard, &geojsonio.WriteOptions{Format: c.format}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// geomSeries builds a single-chunk Binary geometry Series from a list of
// geometries, using the given EPSG in the field metadata.
func geomSeries(t *testing.T, name string, epsg int32, gs []geometry.Geometry) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer b.Release()
	for _, g := range gs {
		if g == nil {
			b.AppendNull()
			continue
		}
		b.Append(geometry.WKB(g))
	}
	arr := b.NewArray()
	field := GeometryField(name, epsg)
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	return Series{name: field.Name, field: field, col: col}
}

func TestSeries_GeomArea(t *testing.T) {
	s := geomSeries(t, "geometry", 3857, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: 0, Y: 0}, {X: 2, Y: 0}, {X: 2, Y: 2}, {X: 0, Y: 2}, {X: 0, Y: 0},
		}, geometry.PseudoMercator),
		geometry.Point{X: 5, Y: 5, CRSValue: geometry.PseudoMercator}, // area 0
		nil, // null → null
	})
	out, err := s.GeomArea(geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	if out.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("output dtype = %s", out.DataType())
	}
	vals, arr, ok := out.singleF64()
	if !ok {
		t.Fatal("expected single-chunk Float64 output")
	}
	if math.Abs(vals[0]-4) > 1e-9 {
		t.Fatalf("row 0 area = %v, want 4", vals[0])
	}
	if vals[1] != 0 {
		t.Fatalf("row 1 (Point) area = %v, want 0", vals[1])
	}
	if !arr.IsNull(2) {
		t.Fatalf("row 2 (null input) should produce null; got %v", vals[2])
	}
}

func TestSeries_GeomLength(t *testing.T) {
	s := geomSeries(t, "geometry", 3857, []geometry.Geometry{
		geometry.LineString{
			Points:   []geometry.Point{{X: 0, Y: 0}, {X: 3, Y: 4}},
			CRSValue: geometry.PseudoMercator,
		},
		geometry.Point{X: 5, Y: 5, CRSValue: geometry.PseudoMercator}, // length 0
	})
	out, err := s.GeomLength(geometry.UnitMeters)
	if err != nil {
		t.Fatal(err)
	}
	vals, _, _ := out.singleF64()
	if math.Abs(vals[0]-5) > 1e-9 {
		t.Fatalf("row 0 length = %v, want 5", vals[0])
	}
	if vals[1] != 0 {
		t.Fatalf("row 1 (Point) length = %v, want 0", vals[1])
	}
}

func TestSeries_GeomCentroid(t *testing.T) {
	s := geomSeries(t, "geometry", 3857, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}, geometry.PseudoMercator),
	})
	out, err := s.GeomCentroid()
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsGeometry() {
		t.Fatal("centroid series is not tagged as geometry")
	}
	g, err := out.Geometry(0)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("expected Point centroid, got %T", g)
	}
	if math.Abs(p.X-5) > 1e-6 || math.Abs(p.Y-5) > 1e-6 {
		t.Fatalf("centroid = %+v, want (5, 5)", p)
	}
}

func TestSeries_GeomBounds(t *testing.T) {
	s := geomSeries(t, "geometry", 3857, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: -1, Y: -2}, {X: 3, Y: -2}, {X: 3, Y: 5}, {X: -1, Y: 5}, {X: -1, Y: -2},
		}, geometry.PseudoMercator),
	})
	f, err := s.GeomBounds()
	if err != nil {
		t.Fatal(err)
	}
	if r, c := f.Shape(); r != 1 || c != 4 {
		t.Fatalf("shape = (%d, %d), want (1, 4)", r, c)
	}
	for i, name := range []string{"minx", "miny", "maxx", "maxy"} {
		col, _ := f.Column(name)
		v, ok, _ := col.numericAt(0)
		if !ok {
			t.Fatalf("%s row 0 unexpectedly null", name)
		}
		want := []float64{-1, -2, 3, 5}[i]
		if v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
}

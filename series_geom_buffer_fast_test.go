package gobi

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// bigEndianPointWKB encodes a 2D point in XDR (big-endian) byte order,
// which geometry.WKB never emits but readers must accept.
func bigEndianPointWKB(x, y float64) []byte {
	b := make([]byte, 21)
	b[0] = 0
	binary.BigEndian.PutUint32(b[1:], 1)
	binary.BigEndian.PutUint64(b[5:], math.Float64bits(x))
	binary.BigEndian.PutUint64(b[13:], math.Float64bits(y))
	return b
}

// mixedGeomSeries: points (2D, 3D, big-endian, empty), nulls, and
// non-point geometries, so both the fast and the general path run.
func mixedGeomSeries(t *testing.T, n int) Series {
	t.Helper()
	r := rand.New(rand.NewPCG(7, 11))
	b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	defer b.Release()
	for i := range n {
		x, y := r.Float64()*1e6-5e5, r.Float64()*1e6-5e5
		switch i % 9 {
		case 0:
			b.AppendNull()
		case 1:
			b.Append(geometry.WKB(geometry.Point{X: x, Y: y, Z: 3, HasZ: true}))
		case 2:
			b.Append(bigEndianPointWKB(x, y))
		case 3:
			b.Append(geometry.WKB(geometry.Point{X: math.NaN(), Y: math.NaN()})) // POINT EMPTY
		case 4:
			b.Append(geometry.WKB(geometry.LineString{Points: []geometry.Point{{X: x, Y: y}, {X: x + 10, Y: y + 5}}}))
		case 5:
			b.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
				{X: x, Y: y}, {X: x + 4, Y: y}, {X: x + 4, Y: y + 4}, {X: x, Y: y + 4}, {X: x, Y: y},
			}, geometry.CRS{})))
		default:
			b.Append(geometry.WKB(geometry.Point{X: x, Y: y}))
		}
	}
	arr := b.NewArray()
	return SeriesFromArray(GeometryField("g", 0), arr)
}

// TestGeomBuffer_FastPathMatchesGeneral — every row is byte-identical
// to the general parse → Buffer → encode path, across segment counts
// and styles.
func TestGeomBuffer_FastPathMatchesGeneral(t *testing.T) {
	s := mixedGeomSeries(t, 2_000) // > geomReserveSample, so reservation runs
	defer s.col.Release()
	for _, opts := range []geometry.BufferOptions{
		{}, {Segments: 4}, {Segments: 7}, {Segments: 64}, {Style: geometry.BufferSquare},
	} {
		for _, d := range []float64{0.5, 1234.5} {
			fast, err := s.GeomBuffer(d, opts)
			if err != nil {
				t.Fatalf("%+v d=%v: %v", opts, d, err)
			}
			general, err := geomTransformOp(s, "_buffer", func(g geometry.Geometry) (geometry.Geometry, error) {
				return geometry.Buffer(g, d, opts)
			})
			if err != nil {
				t.Fatalf("general %+v: %v", opts, err)
			}
			fa := fast.col.Data().Chunk(0).(*array.Binary)
			ga := general.col.Data().Chunk(0).(*array.Binary)
			if fa.Len() != ga.Len() {
				t.Fatalf("len %d vs %d", fa.Len(), ga.Len())
			}
			for i := range fa.Len() {
				if fa.IsNull(i) != ga.IsNull(i) || !bytes.Equal(fa.Value(i), ga.Value(i)) {
					t.Fatalf("%+v d=%v row %d differs from the general path", opts, d, i)
				}
			}
			if fast.Name() != "g_buffer" {
				t.Errorf("name = %q", fast.Name())
			}
			fast.col.Release()
			general.col.Release()
		}
	}
	if _, err := s.GeomBuffer(0, geometry.BufferOptions{}); err == nil {
		t.Error("distance 0: want error, as before")
	}
}

// TestGeomBuffer_OutputNotOverallocated — the reserved value buffer
// ends within the 1/16 headroom of what's used, instead of the up-to-2×
// slack doubling growth leaves.
func TestGeomBuffer_OutputNotOverallocated(t *testing.T) {
	b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	for i := range 20_000 {
		b.Append(geometry.WKB(geometry.Point{X: float64(i), Y: float64(-i)}))
	}
	s := SeriesFromArray(GeometryField("g", 0), b.NewArray())
	b.Release()
	defer s.col.Release()
	out, err := s.GeomBuffer(1, geometry.BufferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer out.col.Release()
	a := out.col.Data().Chunk(0).(*array.Binary)
	used := len(a.ValueBytes())
	capacity := a.Data().Buffers()[2].Cap()
	if float64(capacity) > float64(used)*1.07 {
		t.Errorf("value buffer capacity %d for %d used bytes (%.2fx)", capacity, used, float64(capacity)/float64(used))
	}
}

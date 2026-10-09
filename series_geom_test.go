package gobi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
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

func stringSeriesValues(t *testing.T, s Series) []any {
	t.Helper()
	out := make([]any, 0, s.Len())
	for _, chunk := range s.Column().Data().Chunks() {
		b := chunk.(*array.String)
		for i := range b.Len() {
			if b.IsNull(i) {
				out = append(out, nil)
			} else {
				out = append(out, b.Value(i))
			}
		}
	}
	return out
}

func TestSeries_GeomBuffer_RoundAndSquare(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
	})
	// Round buffer with default 32 segments — area ≈ π*r² for r=5.
	got, err := s.GeomBuffer(5, geometry.BufferOptions{})
	if err != nil {
		t.Fatalf("GeomBuffer round: %v", err)
	}
	g0, _ := got.Geometry(0)
	roundArea := polygonArea(t, g0)
	if roundArea < 76 || roundArea > 79 {
		// 32-gon inscribed in radius-5 circle: (1/2)*32*25*sin(2π/32) ≈ 78.02
		t.Errorf("round buffer area = %v, want ~78 (32-gon in r=5 circle)", roundArea)
	}
	// Square buffer of same distance — a 10×10 square, area exactly 100.
	got, err = s.GeomBuffer(5, geometry.BufferOptions{Style: geometry.BufferSquare})
	if err != nil {
		t.Fatalf("GeomBuffer square: %v", err)
	}
	g0, _ = got.Geometry(0)
	if area := polygonArea(t, g0); math.Abs(area-100) > 1e-9 {
		t.Errorf("square buffer area = %v, want 100", area)
	}
}

func TestSeries_GeomSimplify(t *testing.T) {
	// Line with a nearly-straight kink: (0,0)-(5,0.001)-(10,0). Simplify
	// at tolerance 0.01 should drop the middle vertex.
	line := geometry.LineString{
		Points:   []geometry.Point{{X: 0, Y: 0}, {X: 5, Y: 0.001}, {X: 10, Y: 0}},
		CRSValue: geometry.PseudoMercator,
	}
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{line})
	got, err := s.GeomSimplify(0.01)
	if err != nil {
		t.Fatalf("GeomSimplify: %v", err)
	}
	g, _ := got.Geometry(0)
	l, ok := g.(geometry.LineString)
	if !ok {
		t.Fatalf("got %T, want LineString", g)
	}
	if len(l.Points) != 2 {
		t.Errorf("simplified vertex count = %d, want 2", len(l.Points))
	}
}

func TestSeries_GeomConvexHull(t *testing.T) {
	// Concave L-shape polygon: convex hull is its bounding rectangle.
	l := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 10}, {X: 10, Y: 10},
		{X: 10, Y: 20}, {X: 0, Y: 20}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{l})
	got, err := s.GeomConvexHull()
	if err != nil {
		t.Fatalf("GeomConvexHull: %v", err)
	}
	g, _ := got.Geometry(0)
	// Hull of the L-shape spans (0,0) to (20,20) but is not the full
	// bounding rectangle — it's a pentagon (5 unique convex vertices).
	// Area = 350: 20×20 = 400 minus the 5×5 triangle at (10,10)-(20,20)? no —
	// actually the hull is (0,0)-(20,0)-(20,10)-(10,20)-(0,20)-(0,0), area
	// (10*20 + 10*10 + 5*10*2) = 350.
	if area := polygonArea(t, g); math.Abs(area-350) > 1e-9 {
		t.Errorf("hull area = %v, want 350", area)
	}
}

func TestSeries_GeomEnvelope(t *testing.T) {
	// Concave L-shape: envelope is the full bounding 20×20 rectangle,
	// area 400.
	l := geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 10}, {X: 10, Y: 10},
		{X: 10, Y: 20}, {X: 0, Y: 20}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{l})
	got, err := s.GeomEnvelope()
	if err != nil {
		t.Fatalf("GeomEnvelope: %v", err)
	}
	g, _ := got.Geometry(0)
	if area := polygonArea(t, g); math.Abs(area-400) > 1e-9 {
		t.Errorf("envelope area = %v, want 400", area)
	}
}

func TestSeries_GeomDistance(t *testing.T) {
	// Row 0 is a point 5 units from other; row 1 overlaps other.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
		projectedSquare(5, 0, 20),
	})
	other := projectedSquare(5, 0, 5)
	got, err := s.GeomDistance(other, geometry.UnitMeters)
	if err != nil {
		t.Fatalf("GeomDistance: %v", err)
	}
	vals, _, ok := got.singleF64()
	if !ok {
		t.Fatal("expected Float64 output")
	}
	// Row 0: point at (0,0), other spans [5,10]×[0,5]. Distance = 5.
	if math.Abs(vals[0]-5) > 1e-9 {
		t.Errorf("row 0 distance = %v, want 5", vals[0])
	}
	// Row 1: overlaps other → 0.
	if vals[1] != 0 {
		t.Errorf("row 1 distance = %v, want 0", vals[1])
	}
}

func TestSeries_GeomTouches(t *testing.T) {
	// Row 0: square touching mask on the right edge (no interior overlap).
	// Row 1: square overlapping mask.
	// Row 2: square disjoint.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(10, 0, 5),  // touches at X=10 with mask [5,10]×[0,5]
		projectedSquare(0, 0, 10),  // overlaps
		projectedSquare(50, 50, 5), // disjoint
	})
	mask := projectedSquare(5, 0, 5)
	got, err := s.GeomTouches(mask)
	if err != nil {
		t.Fatalf("GeomTouches: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomOverlaps(t *testing.T) {
	// Row 0: partial overlap ([0,10]² vs [5,15]×[0,10]) — Overlaps = true.
	// Row 1: mask FULLY contains row's polygon ([6,8]² inside [5,15]×[0,10]).
	//        Overlaps = false because one contains the other.
	// Row 2: disjoint. Overlaps = false.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		projectedSquare(6, 1, 2),
		projectedSquare(50, 50, 5),
	})
	mask := projectedSquare(5, 0, 10)
	got, err := s.GeomOverlaps(mask)
	if err != nil {
		t.Fatalf("GeomOverlaps: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomIsEmpty(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		geometry.Polygon{}, // empty
		nil,
	})
	got, err := s.GeomIsEmpty()
	if err != nil {
		t.Fatalf("GeomIsEmpty: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{false, true, nil}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomIsValid(t *testing.T) {
	// Row 0: valid square.
	// Row 1: LineString with only 1 point — invalid.
	// Row 2: self-intersecting bowtie polygon — invalid.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		geometry.LineString{Points: []geometry.Point{{X: 0, Y: 0}}, CRSValue: geometry.PseudoMercator},
		geometry.SimplePolygon([]geometry.Point{
			{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 10, Y: 0}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}, geometry.PseudoMercator),
	})
	got, err := s.GeomIsValid()
	if err != nil {
		t.Fatalf("GeomIsValid: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomType(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),
		geometry.Point{X: 5, Y: 5, CRSValue: geometry.PseudoMercator},
		geometry.LineString{Points: []geometry.Point{
			{X: 0, Y: 0}, {X: 1, Y: 1},
		}, CRSValue: geometry.PseudoMercator},
	})
	got, err := s.GeomType()
	if err != nil {
		t.Fatalf("GeomType: %v", err)
	}
	vals := stringSeriesValues(t, got)
	want := []any{"Polygon", "Point", "LineString"}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

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

// makeFloat64Series builds a single-chunk Float64 Series named name
// with the given values. valid=nil means all values are valid.
func makeFloat64Series(t *testing.T, name string, vals []float64, valid []bool) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	b.AppendValues(vals, valid)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Float64, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked))
}

func TestPointsFromXY_BasicRoundTrip(t *testing.T) {
	// Longitude, latitude for NYC, LA, Chicago.
	lng := makeFloat64Series(t, "lng", []float64{-74.006, -118.2437, -87.6298}, nil)
	lat := makeFloat64Series(t, "lat", []float64{40.7128, 34.0522, 41.8781}, nil)

	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	if geom.Len() != 3 {
		t.Fatalf("len = %d, want 3", geom.Len())
	}
	if !geom.IsGeometry() {
		t.Fatal("returned Series should be tagged as geometry")
	}

	// WKB survives: decode each row and confirm coordinates.
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	for i, want := range []geometry.Point{
		{X: -74.006, Y: 40.7128},
		{X: -118.2437, Y: 34.0522},
		{X: -87.6298, Y: 41.8781},
	} {
		g, err := geometry.ParseWKB(arr.Value(i))
		if err != nil {
			t.Fatalf("row %d WKB parse: %v", i, err)
		}
		p, ok := g.(geometry.Point)
		if !ok {
			t.Fatalf("row %d not a Point: %T", i, g)
		}
		if p.X != want.X || p.Y != want.Y {
			t.Errorf("row %d = (%v, %v), want (%v, %v)",
				i, p.X, p.Y, want.X, want.Y)
		}
	}
}

func TestPointsFromXY_CRSStamped(t *testing.T) {
	lng := makeFloat64Series(t, "lng", []float64{0}, nil)
	lat := makeFloat64Series(t, "lat", []float64{0}, nil)

	geom, err := PointsFromXY(lng, lat, 3857)
	if err != nil {
		t.Fatal(err)
	}
	if got := geometryCRSFromField(geom.field); got != 3857 {
		t.Fatalf("stamped CRS = %d, want 3857", got)
	}
}

func TestPointsFromXY_NullInputProducesNullGeometry(t *testing.T) {
	// Row 1 has a null lng; row 2 has a null lat. Both rows should
	// produce a null geometry.
	lng := makeFloat64Series(t, "lng",
		[]float64{-74, 0, -87},
		[]bool{true, false, true},
	)
	lat := makeFloat64Series(t, "lat",
		[]float64{40, 34, 0},
		[]bool{true, true, false},
	)

	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	if arr.IsNull(0) {
		t.Errorf("row 0 should be non-null")
	}
	if !arr.IsNull(1) {
		t.Errorf("row 1 should be null (lng null)")
	}
	if !arr.IsNull(2) {
		t.Errorf("row 2 should be null (lat null)")
	}
}

func TestPointsFromXY_LenMismatch(t *testing.T) {
	lng := makeFloat64Series(t, "lng", []float64{0, 1}, nil)
	lat := makeFloat64Series(t, "lat", []float64{0}, nil)
	_, err := PointsFromXY(lng, lat, 4326)
	if !errors.Is(err, ErrColumnLenMismatch) {
		t.Fatalf("want ErrColumnLenMismatch, got %v", err)
	}
}

func TestPointsFromXY_MixedNumericTypesPromoteToFloat64(t *testing.T) {
	// lng is Float64, lat is Int64. numericAt promotes int64→float64
	// silently, so PointsFromXY should accept the mix.
	pool := memory.DefaultAllocator
	lng := makeFloat64Series(t, "lng", []float64{-74.5}, nil)

	latB := array.NewInt64Builder(pool)
	defer latB.Release()
	latB.AppendValues([]int64{40}, nil)
	latArr := latB.NewArray()
	defer latArr.Release()
	latField := arrow.Field{Name: "lat", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	latChunked := arrow.NewChunked(latField.Type, []arrow.Array{latArr})
	lat := NewSeries(arrow.NewColumn(latField, latChunked))

	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	g, err := geometry.ParseWKB(arr.Value(0))
	if err != nil {
		t.Fatal(err)
	}
	p := g.(geometry.Point)
	if p.X != -74.5 || p.Y != 40 {
		t.Fatalf("got (%v, %v), want (-74.5, 40)", p.X, p.Y)
	}
}

func TestPointsFromXY_NonNumericErrors(t *testing.T) {
	// Pass a String column as x — should error out with ErrNotNumeric.
	pool := memory.DefaultAllocator
	sb := array.NewStringBuilder(pool)
	defer sb.Release()
	sb.AppendValues([]string{"a", "b"}, nil)
	strArr := sb.NewArray()
	defer strArr.Release()
	strField := arrow.Field{Name: "x", Type: arrow.BinaryTypes.String, Nullable: true}
	strChunked := arrow.NewChunked(strField.Type, []arrow.Array{strArr})
	strCol := NewSeries(arrow.NewColumn(strField, strChunked))

	y := makeFloat64Series(t, "y", []float64{1, 2}, nil)
	_, err := PointsFromXY(strCol, y, 4326)
	if !errors.Is(err, ErrNotNumeric) {
		t.Fatalf("want ErrNotNumeric, got %v", err)
	}
}

func TestPointsFromXY_ComposesWithWithColumn(t *testing.T) {
	// End-to-end: build a Frame with lng/lat columns, derive a
	// geometry column via PointsFromXY, attach with WithColumn.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"NYC", "LA"}, nil)
	lngB := array.NewFloat64Builder(pool)
	defer lngB.Release()
	lngB.AppendValues([]float64{-74.006, -118.2437}, nil)
	latB := array.NewFloat64Builder(pool)
	defer latB.Release()
	latB.AppendValues([]float64{40.7128, 34.0522}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "lng", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "lat", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), lngB.NewArray(), latB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	lng, _ := df.Column("lng")
	lat, _ := df.Column("lat")
	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	out, err := df.WithColumn("geometry", geom)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumCols() != 4 {
		t.Fatalf("cols = %d, want 4", out.NumCols())
	}
	geomCol, _ := out.Column("geometry")
	if !geomCol.IsGeometry() {
		t.Fatal("attached column should be geometry-tagged")
	}
	// Frame.Geometry decode still works.
	g, err := out.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := g.(geometry.Point)
	if p.X != -74.006 {
		t.Fatalf("row 0 X = %v, want -74.006", p.X)
	}
}

func TestPointsFromXYZ_ProducesXYZPoint(t *testing.T) {
	x := makeFloat64Series(t, "x", []float64{1, 2}, nil)
	y := makeFloat64Series(t, "y", []float64{3, 4}, nil)
	z := makeFloat64Series(t, "z", []float64{5, 6}, nil)

	geom, err := PointsFromXYZ(x, y, z, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	g, err := geometry.ParseWKB(arr.Value(0))
	if err != nil {
		t.Fatal(err)
	}
	p := g.(geometry.Point)
	if !p.HasZ {
		t.Fatal("Point should have HasZ=true")
	}
	if p.X != 1 || p.Y != 3 || p.Z != 5 {
		t.Fatalf("row 0 = (%v, %v, %v), want (1, 3, 5)", p.X, p.Y, p.Z)
	}
}

func TestPointsFromXYZ_NullZProducesNullPoint(t *testing.T) {
	x := makeFloat64Series(t, "x", []float64{1, 2}, nil)
	y := makeFloat64Series(t, "y", []float64{3, 4}, nil)
	z := makeFloat64Series(t, "z", []float64{5, 0}, []bool{true, false})

	geom, err := PointsFromXYZ(x, y, z, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	if arr.IsNull(0) {
		t.Errorf("row 0 should be non-null")
	}
	if !arr.IsNull(1) {
		t.Errorf("row 1 should be null (z null)")
	}
}

func TestSeries_GeomCircleContains(t *testing.T) {
	// Two points inside a unit circle around origin, one outside, one null.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
		geometry.Point{X: 0.5, Y: 0.5, CRSValue: geometry.PseudoMercator},
		geometry.Point{X: 5, Y: 0, CRSValue: geometry.PseudoMercator},
		nil,
	})
	c := geometry.Circle{Center: geometry.Point{X: 0, Y: 0}, Radius: 1}
	got, err := s.GeomCircleContains(c)
	if err != nil {
		t.Fatalf("GeomCircleContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, true, false, nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_GeomDistanceToCircle(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},  // center → -r
		geometry.Point{X: 5, Y: 0, CRSValue: geometry.PseudoMercator},  // on boundary
		geometry.Point{X: 10, Y: 0, CRSValue: geometry.PseudoMercator}, // 5 outside
	})
	c := geometry.Circle{Center: geometry.Point{X: 0, Y: 0}, Radius: 5}
	got, err := s.GeomDistanceToCircle(c, geometry.UnitMeters)
	if err != nil {
		t.Fatalf("GeomDistanceToCircle: %v", err)
	}
	vals, _, ok := got.singleF64()
	if !ok {
		t.Fatal("expected Float64 output")
	}
	want := []float64{-5, 0, 5}
	for i, w := range want {
		if math.Abs(vals[i]-w) > 1e-9 {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_GeomFitCircle(t *testing.T) {
	// 12 points on a circle of radius 7 around (3, -2).
	const cx, cy, r = 3.0, -2.0, 7.0
	pts := make([]geometry.Geometry, 12)
	for i := range 12 {
		theta := 2 * math.Pi * float64(i) / 12
		pts[i] = geometry.Point{
			X:        cx + r*math.Cos(theta),
			Y:        cy + r*math.Sin(theta),
			CRSValue: geometry.PseudoMercator,
		}
	}
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), pts)
	c, err := s.GeomFitCircle(geometry.CircleFitOptions{})
	if err != nil {
		t.Fatalf("GeomFitCircle: %v", err)
	}
	if math.Abs(c.Center.X-cx) > 1e-6 || math.Abs(c.Center.Y-cy) > 1e-6 {
		t.Errorf("center = %v, want (%v,%v)", c.Center, cx, cy)
	}
	if math.Abs(c.Radius-r) > 1e-6 {
		t.Errorf("radius = %v, want %v", c.Radius, r)
	}
}

func TestSeries_GeomFitCircle_TooFewPoints(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0},
		geometry.Point{X: 1, Y: 0},
	})
	if _, err := s.GeomFitCircle(geometry.CircleFitOptions{}); err == nil {
		t.Errorf("expected error for 2-point input")
	}
}

func TestSeries_GeomDensifyGeodesic_Roundtrip(t *testing.T) {
	// A LineString with a 90° eastward equatorial segment (10,000 km).
	// Densify at 1000 km spacing → ~11 vertices per segment.
	line := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, CRSValue: geometry.WGS84},
			{X: 90, Y: 0, CRSValue: geometry.WGS84},
		},
		CRSValue: geometry.WGS84,
	}
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{line, nil})
	got, err := s.GeomDensifyGeodesic(1_000_000)
	if err != nil {
		t.Fatalf("GeomDensifyGeodesic: %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("row count = %d, want 2", got.Len())
	}
	g0, err := got.Geometry(0)
	if err != nil {
		t.Fatalf("Geometry(0): %v", err)
	}
	dl, ok := g0.(geometry.LineString)
	if !ok {
		t.Fatalf("row 0 = %T, want LineString", g0)
	}
	if len(dl.Points) < 9 || len(dl.Points) > 13 {
		t.Errorf("densified vertex count = %d, want ~11", len(dl.Points))
	}
	// Endpoints preserved.
	if dl.Points[0].X != 0 || dl.Points[0].Y != 0 {
		t.Errorf("first = %v, want (0,0)", dl.Points[0])
	}
	last := dl.Points[len(dl.Points)-1]
	if math.Abs(last.X-90) > 1e-9 || math.Abs(last.Y) > 1e-9 {
		t.Errorf("last = %v, want (90,0)", last)
	}
	// Null row passes through as null.
	g1, err := got.Geometry(1)
	if err != nil {
		t.Fatalf("Geometry(1): %v", err)
	}
	if g1 != nil {
		t.Errorf("row 1 = %v, want nil", g1)
	}
}

func TestSeries_GeomDensifyGeodesic_NonLineStringPassesThrough(t *testing.T) {
	// A Point row shouldn't change — no segments to densify.
	pt := geometry.Point{X: 5, Y: 10, CRSValue: geometry.WGS84}
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{pt})
	got, err := s.GeomDensifyGeodesic(100_000)
	if err != nil {
		t.Fatalf("GeomDensifyGeodesic: %v", err)
	}
	g0, _ := got.Geometry(0)
	p, ok := g0.(geometry.Point)
	if !ok {
		t.Fatalf("got %T, want Point (pass-through)", g0)
	}
	if p.X != 5 || p.Y != 10 {
		t.Errorf("Point = %v, want (5,10)", p)
	}
}

func TestSeries_GeomDensifyGeodesic_ProjectedCRSErrors(t *testing.T) {
	line := geometry.LineString{
		Points: []geometry.Point{
			{X: 0, Y: 0, CRSValue: geometry.PseudoMercator},
			{X: 1_000_000, Y: 1_000_000, CRSValue: geometry.PseudoMercator},
		},
		CRSValue: geometry.PseudoMercator,
	}
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{line})
	_, err := s.GeomDensifyGeodesic(100_000)
	if !errors.Is(err, geometry.ErrGeodesicRequiresGeographic) {
		t.Errorf("err = %v, want ErrGeodesicRequiresGeographic", err)
	}
}

func TestSeries_GeomCrossesAntimeridian(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: -1, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: 0},
		}, geometry.WGS84),
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: 0}, {X: -170, Y: 0}, {X: -170, Y: 1}, {X: 170, Y: 1}, {X: 170, Y: 0},
		}, geometry.WGS84),
		nil,
	})
	got, err := s.GeomCrossesAntimeridian()
	if err != nil {
		t.Fatalf("GeomCrossesAntimeridian: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{false, true, nil}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomSplitAtAntimeridian(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		// Non-crossing: passes through.
		geometry.SimplePolygon([]geometry.Point{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
		}, geometry.WGS84),
		// Crossing rectangle: splits.
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: -10}, {X: -170, Y: -10}, {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
		}, geometry.WGS84),
	})
	got, err := s.GeomSplitAtAntimeridian()
	if err != nil {
		t.Fatalf("GeomSplitAtAntimeridian: %v", err)
	}
	// Row 0: passes through as Polygon.
	g0, _ := got.Geometry(0)
	if _, ok := g0.(geometry.Polygon); !ok {
		t.Errorf("row 0 = %T, want Polygon (pass-through)", g0)
	}
	// Row 1: split → MultiPolygon.
	g1, _ := got.Geometry(1)
	if _, ok := g1.(geometry.MultiPolygon); !ok {
		t.Errorf("row 1 = %T, want MultiPolygon (split output)", g1)
	}
}

func TestSeries_GeomEstimateUTMCRS_AntimeridianRejects(t *testing.T) {
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: -10}, {X: -170, Y: -10}, {X: -170, Y: 10}, {X: 170, Y: 10}, {X: 170, Y: -10},
		}, geometry.WGS84),
	})
	_, err := s.GeomEstimateUTMCRS()
	if !errors.Is(err, geometry.ErrAntimeridianCrossing) {
		t.Errorf("GeomEstimateUTMCRS on antimeridian-crossing input: got %v, want ErrAntimeridianCrossing", err)
	}
}

func TestSeries_GeomEstimateUTMCRS_DisjointHemispheresRejects(t *testing.T) {
	// Two rows that individually don't cross but whose bounds
	// aggregate to > 180° in width. The Series-level detector should
	// catch this too.
	s := geomSeries(t, "geom", 4326, []geometry.Geometry{
		geometry.SimplePolygon([]geometry.Point{
			{X: -178, Y: 0}, {X: -170, Y: 0}, {X: -170, Y: 1}, {X: -178, Y: 1}, {X: -178, Y: 0},
		}, geometry.WGS84),
		geometry.SimplePolygon([]geometry.Point{
			{X: 170, Y: 0}, {X: 178, Y: 0}, {X: 178, Y: 1}, {X: 170, Y: 1}, {X: 170, Y: 0},
		}, geometry.WGS84),
	})
	_, err := s.GeomEstimateUTMCRS()
	if !errors.Is(err, geometry.ErrAntimeridianCrossing) {
		t.Errorf("GeomEstimateUTMCRS on hemispheres-straddling input: got %v, want ErrAntimeridianCrossing", err)
	}
}

func TestSeries_GeomEllipseContains(t *testing.T) {
	// Ellipse centered at origin, SemiA=3, SemiB=2, rotated by 45°.
	e := geometry.NewEllipse(geometry.Point{X: 0, Y: 0}, 3, 2, math.Pi/4)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		geometry.Point{X: 0, Y: 0, CRSValue: geometry.PseudoMercator}, // center → inside
		// The rotated ellipse extends to (3·cos45°, 3·sin45°) ≈ (2.12, 2.12)
		// along its major axis. (2, 2) is just inside.
		geometry.Point{X: 2, Y: 2, CRSValue: geometry.PseudoMercator},
		// (3, 3) is well outside.
		geometry.Point{X: 3, Y: 3, CRSValue: geometry.PseudoMercator},
		nil,
	})
	got, err := s.GeomEllipseContains(e)
	if err != nil {
		t.Fatalf("GeomEllipseContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, true, false, nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

func TestSeries_GeomEllipseContains_NonPointViaCentroid(t *testing.T) {
	// A Polygon whose centroid falls inside the ellipse should
	// return true; one whose centroid is outside should return false.
	e := geometry.NewEllipse(geometry.Point{X: 0, Y: 0}, 5, 3, 0)
	inside := geometry.SimplePolygon([]geometry.Point{
		{X: -1, Y: -1}, {X: 1, Y: -1}, {X: 1, Y: 1}, {X: -1, Y: 1}, {X: -1, Y: -1},
	}, geometry.PseudoMercator) // centroid at (0, 0)
	outside := geometry.SimplePolygon([]geometry.Point{
		{X: 10, Y: 10}, {X: 11, Y: 10}, {X: 11, Y: 11}, {X: 10, Y: 11}, {X: 10, Y: 10},
	}, geometry.PseudoMercator) // centroid at (10.5, 10.5)
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		inside, outside,
	})
	got, err := s.GeomEllipseContains(e)
	if err != nil {
		t.Fatalf("GeomEllipseContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

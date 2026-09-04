package geometry

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// TestBoundsFromWKB_MatchesParseWKB — the SoA scanner must produce
// the same bounds as ParseWKB(...).Bounds() for every geometry type.
// This is the correctness guarantee that lets computeBboxColumns +
// any other bbox-only caller swap over safely.
func TestBoundsFromWKB_MatchesParseWKB(t *testing.T) {
	cases := []struct {
		name string
		g    Geometry
	}{
		{"Point", Point{X: 5, Y: -3}},
		{"PointZ", Point{X: 5, Y: -3, Z: 7, HasZ: true}},
		{"LineString", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 5}, {X: -3, Y: 12}, {X: 7, Y: -8},
		}}},
		{"LineStringZ", LineString{Points: []Point{
			{X: 0, Y: 0, Z: 100, HasZ: true},
			{X: 10, Y: 5, Z: 200, HasZ: true},
		}, HasZ: true}},
		{"Polygon", Polygon{Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 2, Y: 2}, {X: 4, Y: 2}, {X: 4, Y: 4}, {X: 2, Y: 4}, {X: 2, Y: 2}}, // hole
		}}},
		{"MultiPoint", MultiPoint{Points: []Point{
			{X: 1, Y: 2}, {X: 3, Y: 4}, {X: -1, Y: 5},
		}}},
		{"MultiLineString", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
			{Points: []Point{{X: 5, Y: 5}, {X: 6, Y: 6}, {X: 7, Y: 5}}},
		}}},
		{"MultiPolygon", MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
			}}},
			{Rings: [][]Point{{
				{X: 10, Y: 10}, {X: 11, Y: 10}, {X: 11, Y: 11}, {X: 10, Y: 11}, {X: 10, Y: 10},
			}}},
		}}},
		{"GeometryCollection", GeometryCollection{Geometries: []Geometry{
			Point{X: 0, Y: 0},
			LineString{Points: []Point{{X: 5, Y: 5}, {X: 10, Y: 10}}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := marshalWKB(c.g)
			if err != nil {
				t.Fatalf("marshalWKB: %v", err)
			}
			want := c.g.Bounds()
			got, err := BoundsFromWKB(data)
			if err != nil {
				t.Fatalf("BoundsFromWKB: %v", err)
			}
			if got != want {
				t.Errorf("SoA-scan bounds %+v != AoS bounds %+v", got, want)
			}
			// And explicitly against ParseWKB round-trip too, to
			// catch any bug where marshalWKB and BoundsFromWKB agree
			// but ParseWKB disagrees.
			roundtrip, err := ParseWKB(data)
			if err != nil {
				t.Fatalf("ParseWKB round-trip: %v", err)
			}
			if roundtrip.Bounds() != got {
				t.Errorf("ParseWKB(data).Bounds() = %+v, BoundsFromWKB = %+v",
					roundtrip.Bounds(), got)
			}
		})
	}
}

// TestBoundsFromWKB_Empty — empty geometry types produce
// EmptyBounds. Matches the ParseWKB→.Bounds() output for these
// degenerate shapes.
func TestBoundsFromWKB_Empty(t *testing.T) {
	cases := []Geometry{
		LineString{},
		Polygon{},
		MultiPoint{},
		MultiLineString{},
		MultiPolygon{},
		GeometryCollection{},
	}
	for _, g := range cases {
		data, err := marshalWKB(g)
		if err != nil {
			t.Fatalf("marshalWKB %T: %v", g, err)
		}
		got, err := BoundsFromWKB(data)
		if err != nil {
			t.Fatalf("BoundsFromWKB %T: %v", g, err)
		}
		if !got.Empty() {
			t.Errorf("%T: got %+v, want empty", g, got)
		}
	}
}

// TestBoundsFromWKB_ShortInput — every entry point must reject
// truncated input with ErrShortWKB, not panic.
func TestBoundsFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x01},                         // just byte order
		{0x01, 0x01, 0x00, 0x00, 0x00}, // Point header, no coords
		{0x01, 0x02, 0x00, 0x00, 0x00, 0x03, 0, 0}, // LineString header, truncated count
	}
	for i, data := range cases {
		_, err := BoundsFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB or ErrInvalidByteOrder", i, err)
		}
	}
}

// TestBoundsFromWKB_RejectsNestedCollection — matches ParseWKB's
// rule that GeometryCollections cannot nest.
func TestBoundsFromWKB_RejectsNestedCollection(t *testing.T) {
	// Manually construct WKB for GeometryCollection([GeometryCollection([Point(0,0)])])
	var buf []byte
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 1) // 1 inner geom
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbPoint)
	buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(0))
	buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(0))

	_, err := BoundsFromWKB(buf)
	if !errors.Is(err, ErrUnsupportedWKB) {
		t.Errorf("got %v, want ErrUnsupportedWKB (nested GeometryCollection)", err)
	}
}

// TestBoundsFromWKB_BigEndian — every path handles XDR byte order.
// Fuzzes correctness by re-encoding LineString points with BE and
// checking bounds match LE-encoded version.
func TestBoundsFromWKB_BigEndian(t *testing.T) {
	pts := []Point{{X: 1.5, Y: 2.5}, {X: -3.25, Y: 7.125}, {X: 0.5, Y: -1.5}}
	// Build both LE and BE WKBs by hand for the same LineString.
	var le []byte
	le = append(le, wkbNDR)
	le = binary.LittleEndian.AppendUint32(le, wkbLineString)
	le = binary.LittleEndian.AppendUint32(le, uint32(len(pts)))
	for _, p := range pts {
		le = binary.LittleEndian.AppendUint64(le, math.Float64bits(p.X))
		le = binary.LittleEndian.AppendUint64(le, math.Float64bits(p.Y))
	}
	var be []byte
	be = append(be, wkbXDR)
	be = binary.BigEndian.AppendUint32(be, wkbLineString)
	be = binary.BigEndian.AppendUint32(be, uint32(len(pts)))
	for _, p := range pts {
		be = binary.BigEndian.AppendUint64(be, math.Float64bits(p.X))
		be = binary.BigEndian.AppendUint64(be, math.Float64bits(p.Y))
	}

	leBounds, err := BoundsFromWKB(le)
	if err != nil {
		t.Fatal(err)
	}
	beBounds, err := BoundsFromWKB(be)
	if err != nil {
		t.Fatal(err)
	}
	if leBounds != beBounds {
		t.Errorf("byte order mismatch: LE=%+v BE=%+v", leBounds, beBounds)
	}
}

// TestBoundsFromWKB_ZeroAllocations — the SoA scanner must be
// alloc-free on well-formed input; that's the property that lets
// parquetio's bbox-covering-column write scale.
func TestBoundsFromWKB_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	pts := make([]Point, 100)
	for i := range pts {
		pts[i] = Point{X: rng.Float64(), Y: rng.Float64()}
	}
	data, err := marshalWKB(LineString{Points: pts})
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = BoundsFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("BoundsFromWKB: %v allocs/op, want 0", allocs)
	}
}

// marshalWKB encodes g to a WKB byte string using the geometry
// package's public WKB helper so tests don't need their own
// encoder. Returned as (bytes, err) rather than plain bytes to
// keep the call sites future-proof if error surfaces are added.
func marshalWKB(g Geometry) ([]byte, error) {
	return WKB(g), nil
}

// TestBoundsFromWKB_NaNCoordsDontNarrow locks in the current
// extendBoundsInline semantics: NaN coordinates are silently
// ignored (every comparison against NaN is false, so the
// running bounds stay untouched). This is a NaN-safe reduce —
// the alternative (poisoning the bounds with NaN) would break
// downstream R-tree inserts. If a future refactor changes the
// comparison shape, this test guards against accidental drift.
func TestBoundsFromWKB_NaNCoordsDontNarrow(t *testing.T) {
	// Mix real + NaN vertices in a LineString. Expected bounds
	// span only the real vertices (per-axis — a point with NaN X
	// but real Y still contributes its Y).
	pts := []Point{
		{X: 0, Y: 0},
		{X: math.NaN(), Y: math.NaN()},
		{X: 10, Y: 5},
		{X: math.NaN(), Y: 3},
		{X: 2, Y: math.NaN()},
	}
	data, err := marshalWKB(LineString{Points: pts})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BoundsFromWKB(data)
	if err != nil {
		t.Fatal(err)
	}
	// Real X contributions: {0, 10, 2}. Real Y: {0, 5, 3}.
	if b.MinX != 0 || b.MaxX != 10 || b.MinY != 0 || b.MaxY != 5 {
		t.Errorf("NaN-mixed bounds = %+v, want {0,0,10,5}", b)
	}
	if math.IsNaN(b.MinX) || math.IsNaN(b.MinY) ||
		math.IsNaN(b.MaxX) || math.IsNaN(b.MaxY) {
		t.Errorf("bounds contain NaN: %+v", b)
	}
}

// TestBoundsFromWKB_AllNaNStaysEmpty — if every vertex is NaN,
// the running bounds retain the EmptyBounds sentinel (MinX >
// MaxX), signaling "no valid extent" to downstream callers.
func TestBoundsFromWKB_AllNaNStaysEmpty(t *testing.T) {
	pts := []Point{
		{X: math.NaN(), Y: math.NaN()},
		{X: math.NaN(), Y: math.NaN()},
	}
	data, err := marshalWKB(LineString{Points: pts})
	if err != nil {
		t.Fatal(err)
	}
	b, err := BoundsFromWKB(data)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Empty() {
		t.Errorf("all-NaN bounds = %+v, want Empty()", b)
	}
}

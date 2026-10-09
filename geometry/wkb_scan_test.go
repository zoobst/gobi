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

// centroidsAlmostEqual is a NaN-aware Point comparison used by the
// centroid tests. Distinct from the package-level pointsAlmostEqual
// (clip_linestring_test.go) so NaN handling stays local to the
// centroid tests where the pathological single-point-ring polygon
// case is expected to produce NaN.
func centroidsAlmostEqual(a, b Point, tol float64) bool {
	xOK := (math.IsNaN(a.X) && math.IsNaN(b.X)) || math.Abs(a.X-b.X) <= tol
	yOK := (math.IsNaN(a.Y) && math.IsNaN(b.Y)) || math.Abs(a.Y-b.Y) <= tol
	return xOK && yOK
}

// TestCentroidFromWKB_MatchesGCentroid — SoA scanner produces the
// same centroid as ParseWKB(data).Centroid() for every type where
// the SoA formula is defined to match. See CentroidFromWKB
// docstring for MultiPolygon / GeometryCollection divergences —
// those are covered by a separate test.
func TestCentroidFromWKB_MatchesGCentroid(t *testing.T) {
	cases := []struct {
		name string
		g    Geometry
	}{
		{"Point", Point{X: 5, Y: -3}},
		{"PointZ", Point{X: 5, Y: -3, Z: 7, HasZ: true}},
		{"LineString_closed_square", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}},
		{"LineString_irregular", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 3, Y: 4}, {X: 10, Y: 4}, {X: 15, Y: 12},
		}}},
		{"LineString_single_point", LineString{Points: []Point{{X: 7, Y: 3}}}},
		{"LineString_all_coincident", LineString{Points: []Point{
			{X: 5, Y: 5}, {X: 5, Y: 5}, {X: 5, Y: 5},
		}}},
		{"Polygon_closed_square", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}}},
		{"Polygon_unclosed_square", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
		}}}},
		{"Polygon_with_hole", Polygon{Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 2, Y: 2}, {X: 4, Y: 2}, {X: 4, Y: 4}, {X: 2, Y: 4}, {X: 2, Y: 2}},
		}}},
		{"MultiPoint", MultiPoint{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
		}}},
		{"MultiPoint_single", MultiPoint{Points: []Point{{X: 42, Y: -7}}}},
		{"MultiLineString_two_lines", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 4, Y: 0}}},     // length 4, mid (2, 0)
			{Points: []Point{{X: 10, Y: 10}, {X: 10, Y: 20}}}, // length 10, mid (10, 15)
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := WKB(c.g)
			want := c.g.Centroid()
			// Bench-frame CRS on returned centroid: AoS sets CRSValue
			// from the source geometry; SoA returns unset. Compare
			// only X/Y — that's the contract.
			got, err := CentroidFromWKB(data)
			if err != nil {
				t.Fatalf("CentroidFromWKB: %v", err)
			}
			if !centroidsAlmostEqual(got, want, 1e-9) {
				t.Errorf("SoA centroid %+v, AoS centroid %+v", got, want)
			}
		})
	}
}

// TestCentroidFromWKB_MultiPolygon_UsesBBoxCenter — documents +
// verifies the intentional divergence from
// MultiPolygon.Centroid() (which uses geodesic area-weighting).
// The SoA fast path returns bbox-center; verify against a
// manually-computed bbox-center for the test case.
func TestCentroidFromWKB_MultiPolygon_UsesBBoxCenter(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{{
			{X: 10, Y: 10}, {X: 11, Y: 10}, {X: 11, Y: 11}, {X: 10, Y: 11}, {X: 10, Y: 10},
		}}},
	}}
	got, err := CentroidFromWKB(WKB(m))
	if err != nil {
		t.Fatal(err)
	}
	// Combined bbox is (0, 0)-(11, 11), center (5.5, 5.5).
	want := Point{X: 5.5, Y: 5.5}
	if got != want {
		t.Errorf("bbox-center = %+v, want %+v", got, want)
	}
}

// TestCentroidFromWKB_GeometryCollection_MatchesBBoxCenter —
// GeometryCollection.Centroid IS bbox-center in the AoS
// implementation, so SoA should agree exactly on the (X, Y).
func TestCentroidFromWKB_GeometryCollection_MatchesBBoxCenter(t *testing.T) {
	gc := GeometryCollection{Geometries: []Geometry{
		Point{X: 0, Y: 0},
		LineString{Points: []Point{{X: 5, Y: 5}, {X: 10, Y: 10}}},
	}}
	got, err := CentroidFromWKB(WKB(gc))
	if err != nil {
		t.Fatal(err)
	}
	want := gc.Centroid() // AoS = bbox-center — should match exactly.
	if got.X != want.X || got.Y != want.Y {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestCentroidAndBoundsFromWKB_MatchesSeparately — the fused
// scanner produces the same centroid AND bounds as calling
// CentroidFromWKB and BoundsFromWKB separately.
func TestCentroidAndBoundsFromWKB_MatchesSeparately(t *testing.T) {
	cases := []Geometry{
		Point{X: 5, Y: -3},
		LineString{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}, {X: 10, Y: 4}}},
		Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}},
		MultiPoint{Points: []Point{{X: 1, Y: 2}, {X: 3, Y: 4}}},
		MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
			}}},
		}},
	}
	for _, g := range cases {
		data := WKB(g)
		wantC, err := CentroidFromWKB(data)
		if err != nil {
			t.Fatalf("%T CentroidFromWKB: %v", g, err)
		}
		wantB, err := BoundsFromWKB(data)
		if err != nil {
			t.Fatalf("%T BoundsFromWKB: %v", g, err)
		}
		gotC, gotB, err := CentroidAndBoundsFromWKB(data)
		if err != nil {
			t.Fatalf("%T CentroidAndBoundsFromWKB: %v", g, err)
		}
		if !centroidsAlmostEqual(gotC, wantC, 1e-9) {
			t.Errorf("%T: fused centroid %+v, standalone %+v", g, gotC, wantC)
		}
		if gotB != wantB {
			t.Errorf("%T: fused bounds %+v, standalone %+v", g, gotB, wantB)
		}
	}
}

// TestCentroidFromWKB_ShortInput — every entry point must reject
// truncated input without panicking.
func TestCentroidFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x01},
		{0x01, 0x01, 0x00, 0x00, 0x00},
		{0x01, 0x02, 0x00, 0x00, 0x00, 0x03, 0, 0},
	}
	for i, data := range cases {
		_, err := CentroidFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB or ErrInvalidByteOrder", i, err)
		}
	}
}

// TestCentroidFromWKB_ZeroAllocations — parity with BoundsFromWKB's
// zero-alloc contract. The whole point of the Slice 3 path is to
// eliminate per-row allocation in the SortByHilbert loop.
func TestCentroidFromWKB_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	pts := make([]Point, 100)
	for i := range pts {
		pts[i] = Point{X: rng.Float64(), Y: rng.Float64()}
	}
	data := WKB(LineString{Points: pts})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = CentroidFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("CentroidFromWKB: %v allocs/op, want 0", allocs)
	}
	allocs = testing.AllocsPerRun(100, func() {
		_, _, _ = CentroidAndBoundsFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("CentroidAndBoundsFromWKB: %v allocs/op, want 0", allocs)
	}
}

// TestCentroidFromWKB_BigEndian — LE and BE encodings of the same
// geometry produce the same centroid.
func TestCentroidFromWKB_BigEndian(t *testing.T) {
	pts := []Point{{X: 1.5, Y: 2.5}, {X: -3.25, Y: 7.125}, {X: 0.5, Y: -1.5}}
	// Build both LE and BE LineString WKBs by hand.
	var le, be []byte
	le = append(le, wkbNDR)
	le = binary.LittleEndian.AppendUint32(le, wkbLineString)
	le = binary.LittleEndian.AppendUint32(le, uint32(len(pts)))
	for _, p := range pts {
		le = binary.LittleEndian.AppendUint64(le, math.Float64bits(p.X))
		le = binary.LittleEndian.AppendUint64(le, math.Float64bits(p.Y))
	}
	be = append(be, wkbXDR)
	be = binary.BigEndian.AppendUint32(be, wkbLineString)
	be = binary.BigEndian.AppendUint32(be, uint32(len(pts)))
	for _, p := range pts {
		be = binary.BigEndian.AppendUint64(be, math.Float64bits(p.X))
		be = binary.BigEndian.AppendUint64(be, math.Float64bits(p.Y))
	}
	leC, err := CentroidFromWKB(le)
	if err != nil {
		t.Fatal(err)
	}
	beC, err := CentroidFromWKB(be)
	if err != nil {
		t.Fatal(err)
	}
	if !centroidsAlmostEqual(leC, beC, 1e-12) {
		t.Errorf("byte-order mismatch: LE=%+v BE=%+v", leC, beC)
	}
}

// TestPlanarAreaFromWKB_MatchesAoS — the SoA scanner must match
// Polygon.Area / MultiPolygon.Area on a projected CRS (planar).
// Non-areal types return 0 matching geometry.Area's dispatch.
func TestPlanarAreaFromWKB_MatchesAoS(t *testing.T) {
	projected := CRS{EPSG: 3857}
	cases := []struct {
		name string
		g    Geometry
		want float64
	}{
		{"Point", Point{X: 5, Y: -3}, 0},
		{"LineString-nonareal", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10},
		}}, 0},
		{"MultiPoint-nonareal", MultiPoint{Points: []Point{
			{X: 1, Y: 2}, {X: 3, Y: 4},
		}}, 0},
		{"MultiLineString-nonareal", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
		}}, 0},
		{"Polygon-square", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
		}}, CRSValue: projected}, 100},
		{"Polygon-with-hole", Polygon{Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 2, Y: 2}, {X: 4, Y: 2}, {X: 4, Y: 4}, {X: 2, Y: 4}, {X: 2, Y: 2}},
		}, CRSValue: projected}, 100 - 4},
		{"Polygon-unclosed", Polygon{Rings: [][]Point{{
			// Not closed — scanner should virtual-close.
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10},
		}}, CRSValue: projected}, 100},
		{"MultiPolygon", MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
			}}},
			{Rings: [][]Point{{
				{X: 20, Y: 0}, {X: 25, Y: 0}, {X: 25, Y: 5}, {X: 20, Y: 5}, {X: 20, Y: 0},
			}}},
		}, CRSValue: projected}, 100 + 25},
		{"GeometryCollection-mixed", GeometryCollection{Geometries: []Geometry{
			Polygon{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
			}}, CRSValue: projected},
			Point{X: 100, Y: 100, CRSValue: projected},
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}}, CRSValue: projected},
		}, CRSValue: projected}, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := WKB(c.g)
			got, err := PlanarAreaFromWKB(data)
			if err != nil {
				t.Fatalf("PlanarAreaFromWKB: %v", err)
			}
			if !almostEqualF(got, c.want, 1e-9) {
				t.Errorf("PlanarAreaFromWKB = %v, want %v", got, c.want)
			}
			wantAoS, err := Area(c.g, UnitMeters)
			if err != nil {
				t.Fatalf("Area: %v", err)
			}
			if !almostEqualF(got, wantAoS, 1e-9) {
				t.Errorf("SoA=%v differs from AoS Area=%v", got, wantAoS)
			}
		})
	}
}

// TestPlanarAreaFromWKB_RandomPolygons — fuzz against Polygon.Area
// on projected CRS. Uses convex random polygons to sidestep
// self-intersection edge cases.
func TestPlanarAreaFromWKB_RandomPolygons(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	projected := CRS{EPSG: 3857}
	for iter := range 50 {
		// n points on a circle, guaranteed convex + non-self-intersecting.
		n := 3 + rng.Intn(12)
		cx := rng.Float64() * 100
		cy := rng.Float64() * 100
		r := 1 + rng.Float64()*50
		pts := make([]Point, n+1)
		for i := range n {
			θ := float64(i) * 2 * math.Pi / float64(n)
			pts[i] = Point{X: cx + r*math.Cos(θ), Y: cy + r*math.Sin(θ)}
		}
		pts[n] = pts[0] // close
		p := Polygon{Rings: [][]Point{pts}, CRSValue: projected}
		data := WKB(p)
		got, err := PlanarAreaFromWKB(data)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		want, err := p.Area(UnitMeters)
		if err != nil {
			t.Fatalf("iter %d Area: %v", iter, err)
		}
		if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Errorf("iter %d: SoA=%v AoS=%v", iter, got, want)
		}
	}
}

// TestPlanarAreaFromWKB_ShortInput — malformed input returns
// ErrShortWKB / ErrInvalidByteOrder rather than panicking.
func TestPlanarAreaFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x01}, // just byte order
		{0x01, 0x03, 0x00, 0x00, 0x00, 0x01, 0, 0}, // Polygon header, truncated ring count
	}
	for i, data := range cases {
		_, err := PlanarAreaFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB/ErrInvalidByteOrder", i, err)
		}
	}
}

// TestPlanarAreaFromWKB_RejectsNestedCollection — matches
// ParseWKB's rule.
func TestPlanarAreaFromWKB_RejectsNestedCollection(t *testing.T) {
	var buf []byte
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 0)

	_, err := PlanarAreaFromWKB(buf)
	if !errors.Is(err, ErrUnsupportedWKB) {
		t.Errorf("got %v, want ErrUnsupportedWKB", err)
	}
}

// TestPlanarAreaFromWKB_ZeroAllocations — hot path must be
// alloc-free on well-formed input.
func TestPlanarAreaFromWKB_ZeroAllocations(t *testing.T) {
	n := 100
	pts := make([]Point, n+1)
	for i := range n {
		θ := float64(i) * 2 * math.Pi / float64(n)
		pts[i] = Point{X: 10 * math.Cos(θ), Y: 10 * math.Sin(θ)}
	}
	pts[n] = pts[0]
	data := WKB(Polygon{Rings: [][]Point{pts}})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = PlanarAreaFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("%v allocs/op, want 0", allocs)
	}
}

// TestPlanarLengthFromWKB_MatchesAoS — the SoA scanner must match
// LineString.Length / MultiLineString.Length on a projected CRS
// (planar) across the full geometry-type matrix. Non-linear types
// return 0 matching geometry.Length's dispatch.
func TestPlanarLengthFromWKB_MatchesAoS(t *testing.T) {
	projected := CRS{EPSG: 3857}
	cases := []struct {
		name string
		g    Geometry
		want float64
	}{
		{"Point", Point{X: 5, Y: -3}, 0},
		{"LineString-2pts", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 3, Y: 4},
		}, CRSValue: projected}, 5},
		{"LineString-many", LineString{Points: []Point{
			{X: 0, Y: 0}, {X: 3, Y: 0}, {X: 3, Y: 4}, {X: 0, Y: 4},
		}, CRSValue: projected}, 3 + 4 + 3},
		{"LineString-degenerate-1pt", LineString{Points: []Point{
			{X: 5, Y: 5},
		}, CRSValue: projected}, 0},
		{"Polygon-nonlinear", Polygon{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
		}}, CRSValue: projected}, 0},
		{"MultiPoint-nonlinear", MultiPoint{Points: []Point{
			{X: 1, Y: 2}, {X: 3, Y: 4},
		}, CRSValue: projected}, 0},
		{"MultiLineString", MultiLineString{Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}}},
			{Points: []Point{{X: 0, Y: 0}, {X: 5, Y: 12}}},
		}, CRSValue: projected}, 5 + 13},
		{"MultiPolygon-nonlinear", MultiPolygon{Polygons: []Polygon{
			{Rings: [][]Point{{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0}}}},
		}, CRSValue: projected}, 0},
		{"GeometryCollection-mixed", GeometryCollection{Geometries: []Geometry{
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 4}}, CRSValue: projected},
			Point{X: 100, Y: 100, CRSValue: projected},
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 5, Y: 12}}, CRSValue: projected},
		}, CRSValue: projected}, 5 + 13},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := WKB(c.g)
			got, err := PlanarLengthFromWKB(data)
			if err != nil {
				t.Fatalf("PlanarLengthFromWKB: %v", err)
			}
			if !almostEqualF(got, c.want, 1e-9) {
				t.Errorf("PlanarLengthFromWKB = %v, want %v", got, c.want)
			}
			// Cross-check against AoS Length on a projected CRS.
			wantAoS, err := Length(c.g, UnitMeters)
			if err != nil {
				t.Fatalf("Length: %v", err)
			}
			if !almostEqualF(got, wantAoS, 1e-9) {
				t.Errorf("SoA=%v differs from AoS Length=%v", got, wantAoS)
			}
		})
	}
}

// TestPlanarLengthFromWKB_RandomLineStrings — fuzz against
// LineString.Length on projected CRS across many random shapes.
func TestPlanarLengthFromWKB_RandomLineStrings(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	projected := CRS{EPSG: 3857}
	for iter := range 100 {
		n := 2 + rng.Intn(20)
		pts := make([]Point, n)
		for i := range pts {
			pts[i] = Point{X: rng.Float64() * 1000, Y: rng.Float64() * 1000}
		}
		ls := LineString{Points: pts, CRSValue: projected}
		data := WKB(ls)
		got, err := PlanarLengthFromWKB(data)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		want, err := ls.Length(UnitMeters)
		if err != nil {
			t.Fatalf("iter %d Length: %v", iter, err)
		}
		if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Errorf("iter %d: SoA=%v AoS=%v", iter, got, want)
		}
	}
}

// TestPlanarLengthFromWKB_ShortInput — malformed input returns
// ErrShortWKB / ErrInvalidByteOrder rather than panicking.
func TestPlanarLengthFromWKB_ShortInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x01}, // just byte order
		{0x01, 0x02, 0x00, 0x00, 0x00, 0x03, 0, 0}, // LineString header, truncated count
	}
	for i, data := range cases {
		_, err := PlanarLengthFromWKB(data)
		if !errors.Is(err, ErrShortWKB) && !errors.Is(err, ErrInvalidByteOrder) {
			t.Errorf("case %d: got %v, want ErrShortWKB/ErrInvalidByteOrder", i, err)
		}
	}
}

// TestPlanarLengthFromWKB_RejectsNestedCollection — matches
// ParseWKB's rule (no nested GeometryCollections).
func TestPlanarLengthFromWKB_RejectsNestedCollection(t *testing.T) {
	var buf []byte
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = append(buf, wkbNDR)
	buf = binary.LittleEndian.AppendUint32(buf, wkbGeometryCollection)
	buf = binary.LittleEndian.AppendUint32(buf, 0)

	_, err := PlanarLengthFromWKB(buf)
	if !errors.Is(err, ErrUnsupportedWKB) {
		t.Errorf("got %v, want ErrUnsupportedWKB", err)
	}
}

// TestPlanarLengthFromWKB_ZeroAllocations — hot path must be
// alloc-free on well-formed input.
func TestPlanarLengthFromWKB_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	pts := make([]Point, 100)
	for i := range pts {
		pts[i] = Point{X: rng.Float64(), Y: rng.Float64()}
	}
	data := WKB(LineString{Points: pts})
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = PlanarLengthFromWKB(data)
	})
	if allocs != 0 {
		t.Errorf("%v allocs/op, want 0", allocs)
	}
}

func almostEqualF(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

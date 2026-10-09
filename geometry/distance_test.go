package geometry

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// TestHaversineBatch_MatchesScalar — the batch call and the scalar
// Haversine agree pair-for-pair. Uses a mix of short (~100km),
// medium (~4000km), and long (~5500km) legs to catch any regime-
// specific numerical drift.
func TestHaversineBatch_MatchesScalar(t *testing.T) {
	from := []Point{
		{X: -73.9857, Y: 40.7484},  // NYC
		{X: -118.2437, Y: 34.0522}, // LA
		{X: 0, Y: 0},               // null island
		{X: 2.3522, Y: 48.8566},    // Paris
	}
	to := []Point{
		{X: -0.1276, Y: 51.5074},  // London
		{X: -73.9857, Y: 40.7484}, // NYC
		{X: 1, Y: 0},              // one deg east
		{X: -0.1276, Y: 51.5074},  // London
	}
	got, err := HaversineBatch(from, to, UnitKilometers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(from) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(from))
	}
	for i := range from {
		want, err := Haversine(from[i], to[i], UnitKilometers)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(got[i]-want) > 1e-9 {
			t.Errorf("row %d: got %v, want %v (scalar Haversine)", i, got[i], want)
		}
	}
}

// TestHaversineBatch_KnownDistances — spot-check the well-known
// distances to catch any wholesale unit / scale mistake.
func TestHaversineBatch_KnownDistances(t *testing.T) {
	got, err := HaversineBatch(
		[]Point{
			{X: -73.9857, Y: 40.7484}, // NYC
			{X: 0, Y: 90},             // North pole
			{X: 0, Y: 0},              // Equator
		},
		[]Point{
			{X: -0.1276, Y: 51.5074}, // London
			{X: 0, Y: 0},             // Equator
			{X: 1, Y: 0},             // One deg east
		},
		UnitKilometers,
	)
	if err != nil {
		t.Fatal(err)
	}
	// NYC → London ~5570 km
	if got[0] < 5500 || got[0] > 5600 {
		t.Errorf("NYC→London = %v km, want ~5570", got[0])
	}
	// Pole → Equator = quarter Earth circumference ~10007 km
	if got[1] < 9990 || got[1] > 10020 {
		t.Errorf("pole→equator = %v km, want ~10007", got[1])
	}
	// One degree at equator ~111 km
	if got[2] < 110 || got[2] > 112 {
		t.Errorf("1-deg-east = %v km, want ~111", got[2])
	}
}

// TestHaversineBatch_UnitScaling — same input, different units,
// ratios match. Sanity check that metersPerUnit is hoisted correctly.
func TestHaversineBatch_UnitScaling(t *testing.T) {
	from := []Point{{X: 0, Y: 0}}
	to := []Point{{X: 1, Y: 0}}

	km, _ := HaversineBatch(from, to, UnitKilometers)
	m, _ := HaversineBatch(from, to, UnitMeters)
	mi, _ := HaversineBatch(from, to, UnitMiles)

	if math.Abs(km[0]*1000-m[0]) > 1e-6 {
		t.Errorf("km→m mismatch: %v vs %v", km[0]*1000, m[0])
	}
	if math.Abs(km[0]/mi[0]-1.609344) > 1e-6 {
		t.Errorf("km/mi ratio = %v, want 1.609344", km[0]/mi[0])
	}
}

// TestHaversineBatch_LengthMismatch — mismatched slice lengths
// error, don't panic.
func TestHaversineBatch_LengthMismatch(t *testing.T) {
	from := []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}
	to := []Point{{X: 0, Y: 0}}
	_, err := HaversineBatch(from, to, UnitKilometers)
	if err == nil {
		t.Fatal("expected length mismatch error")
	}
}

// TestHaversineBatch_EmptyInputs — zero-length slices return a
// non-nil empty result, not an error.
func TestHaversineBatch_EmptyInputs(t *testing.T) {
	got, err := HaversineBatch(nil, nil, UnitKilometers)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Errorf("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestHaversineBatch_InvalidUnit — bad Unit propagates the same
// error the scalar Haversine returns.
func TestHaversineBatch_InvalidUnit(t *testing.T) {
	from := []Point{{X: 0, Y: 0}}
	to := []Point{{X: 1, Y: 0}}
	_, err := HaversineBatch(from, to, Unit("furlongs"))
	if err == nil {
		t.Fatal("expected error on invalid unit")
	}
	if !errors.Is(err, ErrInvalidUnit) {
		t.Errorf("error should wrap ErrInvalidUnit, got %v", err)
	}
}

// BenchmarkHaversineBatch_vs_ScalarLoop compares the bulk-loop
// path against per-row scalar Haversine calls on the same N=10k
// fixture. Loop-scaffolding + constant hoisting are the whole win —
// no SIMD, no polynomial approximations, just plain scalar math
// with fewer per-call boundaries.
func BenchmarkHaversineBatch_vs_ScalarLoop(b *testing.B) {
	const N = 10_000
	from := make([]Point, N)
	to := make([]Point, N)
	for i := range from {
		// Some geographic spread — random-ish but deterministic.
		from[i] = Point{X: float64(i%180 - 90), Y: float64(i%90 - 45)}
		to[i] = Point{X: float64((i+7)%180 - 90), Y: float64((i+3)%90 - 45)}
	}

	b.Run("Batch", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out, err := HaversineBatch(from, to, UnitKilometers)
			if err != nil {
				b.Fatal(err)
			}
			_ = out
		}
	})

	b.Run("ScalarLoop", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out := make([]float64, N)
			for j := range from {
				d, err := Haversine(from[j], to[j], UnitKilometers)
				if err != nil {
					b.Fatal(err)
				}
				out[j] = d
			}
			_ = out
		}
	})
}

// TestPointToSegmentDistanceSqXY_MatchesAoS — the squared form
// must match `pointToSegmentDistance(...)²` on the full
// (endpoint-projection, interior, degenerate) shape matrix.
func TestPointToSegmentDistanceSqXY_MatchesAoS(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := range 200 {
		px := rng.Float64()*100 - 50
		py := rng.Float64()*100 - 50
		ax := rng.Float64()*100 - 50
		ay := rng.Float64()*100 - 50
		bx := rng.Float64()*100 - 50
		by := rng.Float64()*100 - 50
		// Occasionally force degenerate segments.
		if iter%20 == 0 {
			bx, by = ax, ay
		}
		got := math.Sqrt(PointToSegmentDistanceSqXY(px, py, ax, ay, bx, by))
		want := pointToSegmentDistance(
			Point{X: px, Y: py},
			Point{X: ax, Y: ay},
			Point{X: bx, Y: by},
		)
		if math.Abs(got-want) > 1e-10*math.Max(1, math.Abs(want)) {
			t.Errorf("iter %d: SoA=%v AoS=%v", iter, got, want)
		}
	}
}

// TestPointToPolylineMinDistanceSq_MatchesAoS — the polyline
// scanner must match the equivalent forEachSegment + min-track
// AoS pattern.
func TestPointToPolylineMinDistanceSq_MatchesAoS(t *testing.T) {
	polyline := []Point{
		{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5},
	}
	xs := []float64{0, 5, 5, 0}
	ys := []float64{0, 0, 5, 5}
	queries := []Point{
		{X: 2.5, Y: 2.5}, // near the middle — closest to (2.5, 0) segment
		{X: -1, Y: -1},   // outside — closest to (0,0)
		{X: 10, Y: 5},    // outside on the right
		{X: 2.5, Y: -3},  // below
	}
	for _, q := range queries {
		gotClosed := math.Sqrt(PointToPolylineMinDistanceSq(q.X, q.Y, xs, ys, true))
		gotOpen := math.Sqrt(PointToPolylineMinDistanceSq(q.X, q.Y, xs, ys, false))
		// AoS oracle: iterate segments (with/without closure) and
		// track min distance.
		wantOpen := math.Inf(1)
		for i := 0; i < len(polyline)-1; i++ {
			d := pointToSegmentDistance(q, polyline[i], polyline[i+1])
			if d < wantOpen {
				wantOpen = d
			}
		}
		wantClosed := wantOpen
		dClose := pointToSegmentDistance(q, polyline[len(polyline)-1], polyline[0])
		if dClose < wantClosed {
			wantClosed = dClose
		}
		if math.Abs(gotOpen-wantOpen) > 1e-10 {
			t.Errorf("q=%v open: got %v want %v", q, gotOpen, wantOpen)
		}
		if math.Abs(gotClosed-wantClosed) > 1e-10 {
			t.Errorf("q=%v closed: got %v want %v", q, gotClosed, wantClosed)
		}
	}
}

// TestPlanarMinDistance_MatchesPreSlice11 — the slab-rewrite of
// planarMinDistance must produce identical results to the AoS
// path on a battery of shape combinations. This is the top-level
// invariant that lets the internal rewrite ship without churning
// GeomDistance / dwithin callers.
func TestPlanarMinDistance_MatchesPreSlice11(t *testing.T) {
	// A grab-bag of geometries — points, disjoint segments,
	// nested polygons, cross-type pairs. All non-intersecting so
	// planarMinDistance is called; Intersects() returns false
	// before dispatch for the intersecting cases.
	cases := []struct {
		name string
		a, b Geometry
	}{
		{
			"pt-pt", Point{X: 0, Y: 0}, Point{X: 3, Y: 4},
		},
		{
			"pt-line",
			Point{X: 0, Y: 5},
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 0}}},
		},
		{
			"line-line",
			LineString{Points: []Point{{X: 0, Y: 0}, {X: 10, Y: 0}}},
			LineString{Points: []Point{{X: 0, Y: 5}, {X: 10, Y: 5}}},
		},
		{
			"line-poly",
			LineString{Points: []Point{{X: 20, Y: 20}, {X: 30, Y: 30}}},
			Polygon{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
			}}},
		},
		{
			"poly-poly",
			Polygon{Rings: [][]Point{{
				{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
			}}},
			Polygon{Rings: [][]Point{{
				{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10},
			}}},
		},
		{
			"multi-line",
			MultiLineString{Lines: []LineString{
				{Points: []Point{{X: 0, Y: 0}, {X: 3, Y: 0}}},
				{Points: []Point{{X: 5, Y: 5}, {X: 6, Y: 6}}},
			}},
			LineString{Points: []Point{{X: 10, Y: 10}, {X: 11, Y: 11}}},
		},
		{
			"multi-point-pt",
			MultiPoint{Points: []Point{{X: 0, Y: 0}, {X: 100, Y: 100}}},
			Point{X: 5, Y: 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planarMinDistance(c.a, c.b)
			// Symmetric oracle: swap arguments; must produce same answer.
			gotSwapped := planarMinDistance(c.b, c.a)
			if math.Abs(got-gotSwapped) > 1e-10 {
				t.Errorf("asymmetric: got %v vs %v swapped", got, gotSwapped)
			}
			// Known expected values via direct computation on the
			// simplest cases; harder shapes are covered by symmetry
			// + the AoS regression suite that already exists.
			switch c.name {
			case "pt-pt":
				if math.Abs(got-5) > 1e-10 {
					t.Errorf("got %v, want 5", got)
				}
			case "pt-line":
				if math.Abs(got-5) > 1e-10 {
					t.Errorf("got %v, want 5", got)
				}
			case "line-line":
				if math.Abs(got-5) > 1e-10 {
					t.Errorf("got %v, want 5", got)
				}
			case "poly-poly":
				want := math.Sqrt(50) // (5,5) → (10,10)
				if math.Abs(got-want) > 1e-10 {
					t.Errorf("got %v, want %v", got, want)
				}
			}
		})
	}
}

// TestPlanarMinDistance_RandomPolygonPairs — fuzz the SoA rewrite
// against a reference AoS oracle inlined here.
func TestPlanarMinDistance_RandomPolygonPairs(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for iter := range 30 {
		// Two disjoint circles worth of polygons at random offsets.
		a := buildRandomTriangle(rng, 0, 0)
		b := buildRandomTriangle(rng, 100, 100)
		got := planarMinDistance(a, b)
		want := aosPlanarMinDistanceOracle(a, b)
		if math.Abs(got-want) > 1e-8 {
			t.Errorf("iter %d: SoA=%v AoS-oracle=%v", iter, got, want)
		}
	}
}

func buildRandomTriangle(rng *rand.Rand, cx, cy float64) Polygon {
	pts := make([]Point, 4)
	for i := range 3 {
		theta := float64(i) * 2 * math.Pi / 3
		pts[i] = Point{
			X: cx + 5*math.Cos(theta) + rng.Float64()*0.1,
			Y: cy + 5*math.Sin(theta) + rng.Float64()*0.1,
		}
	}
	pts[3] = pts[0]
	return Polygon{Rings: [][]Point{pts}}
}

func aosPlanarMinDistanceOracle(a, b Geometry) float64 {
	best := math.Inf(1)
	forEachVertex(a, func(p Point) {
		forEachSegment(b, func(s0, s1 Point) {
			if d := pointToSegmentDistance(p, s0, s1); d < best {
				best = d
			}
		})
	})
	forEachVertex(b, func(p Point) {
		forEachSegment(a, func(s0, s1 Point) {
			if d := pointToSegmentDistance(p, s0, s1); d < best {
				best = d
			}
		})
	})
	if math.IsInf(best, 1) {
		forEachVertex(a, func(pa Point) {
			forEachVertex(b, func(pb Point) {
				d := math.Hypot(pa.X-pb.X, pa.Y-pb.Y)
				if d < best {
					best = d
				}
			})
		})
	}
	if math.IsInf(best, 1) {
		return 0
	}
	return best
}

func TestWithinDistance_PointPoint(t *testing.T) {
	a := Point{X: 0, Y: 0}
	b := Point{X: 3, Y: 4} // distance = 5
	cases := []struct {
		d    float64
		want bool
	}{
		{4.9, false},
		{5.0, true}, // inclusive
		{5.1, true},
		{0, false}, // same as Intersects: not intersecting
		{100, true},
	}
	for _, c := range cases {
		if got := WithinDistance(a, b, c.d); got != c.want {
			t.Errorf("d=%v: got %v, want %v", c.d, got, c.want)
		}
	}
}

// TestWithinDistance_ZeroIsIntersects — d=0 must match Intersects
// (identical points → true, disjoint → false, touching → true).
func TestWithinDistance_ZeroIsIntersects(t *testing.T) {
	sq := func(x, y, size float64) Polygon {
		return SimplePolygon([]Point{
			{X: x, Y: y},
			{X: x + size, Y: y},
			{X: x + size, Y: y + size},
			{X: x, Y: y + size},
			{X: x, Y: y},
		}, CRS{})
	}
	overlap := sq(0, 0, 10)
	touching := sq(10, 0, 5)  // touches overlap on right edge
	disjoint := sq(50, 50, 5) // far away

	if !WithinDistance(overlap, touching, 0) {
		t.Error("touching squares at d=0: expected true (edge-touching = Intersects)")
	}
	if WithinDistance(overlap, disjoint, 0) {
		t.Error("disjoint squares at d=0: expected false")
	}
}

// TestWithinDistance_BboxShortCircuit — the whole point of DWithin's
// perf story. Two far-apart polygons with lots of vertices should
// bail out at the bbox check without walking edges. We can't directly
// count edge visits, but we can verify correctness of the fast path
// (returns false for provably-too-far pairs).
func TestWithinDistance_BboxShortCircuit(t *testing.T) {
	// Two 1000-vertex polygons 10,000 units apart.
	buildDensePoly := func(cx, cy float64) Polygon {
		pts := make([]Point, 1001)
		for i := range 1000 {
			theta := 2 * math.Pi * float64(i) / 1000
			pts[i] = Point{X: cx + math.Cos(theta), Y: cy + math.Sin(theta)}
		}
		pts[1000] = pts[0]
		return SimplePolygon(pts, CRS{})
	}
	a := buildDensePoly(0, 0)
	b := buildDensePoly(10_000, 0)
	if WithinDistance(a, b, 100) {
		t.Error("10,000-unit apart polygons within distance 100: expected false")
	}
	if !WithinDistance(a, b, 10_000) {
		t.Error("10,000-unit apart polygons within distance 10,000: expected true (bboxes ~9998 apart, minus radius)")
	}
}

// TestWithinDistance_PointPolygon — point inside polygon → 0
// distance, so any non-negative d passes.
func TestWithinDistance_PointPolygon(t *testing.T) {
	poly := SimplePolygon([]Point{
		{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0},
	}, CRS{})
	inside := Point{X: 5, Y: 5}
	nearBoundary := Point{X: 11, Y: 5} // 1 unit outside +X edge
	far := Point{X: 50, Y: 50}

	if !WithinDistance(inside, poly, 0) {
		t.Error("point inside polygon at d=0: expected true")
	}
	if !WithinDistance(nearBoundary, poly, 1) {
		t.Error("point 1 unit outside boundary at d=1: expected true (inclusive)")
	}
	if WithinDistance(nearBoundary, poly, 0.5) {
		t.Error("point 1 unit outside boundary at d=0.5: expected false")
	}
	if WithinDistance(far, poly, 5) {
		t.Error("far point at d=5: expected false")
	}
}

// TestWithinDistance_NegativeD / NaN → false.
func TestWithinDistance_InvalidD(t *testing.T) {
	a := Point{X: 0, Y: 0}
	b := Point{X: 0, Y: 0} // identical
	if WithinDistance(a, b, -1) {
		t.Error("negative distance: expected false")
	}
	if WithinDistance(a, b, math.NaN()) {
		t.Error("NaN distance: expected false")
	}
}

// TestBboxMinDistance covers the private helper directly for
// clarity — the geometry-level tests exercise it too, but with
// axis-aligned bboxes we can hand-verify.
func TestBboxMinDistance(t *testing.T) {
	cases := []struct {
		name string
		a, b Bounds
		want float64
	}{
		{"disjoint on X", Bounds{0, 0, 10, 10}, Bounds{20, 0, 30, 10}, 10},
		{"disjoint on Y", Bounds{0, 0, 10, 10}, Bounds{0, 20, 10, 30}, 10},
		{"disjoint diagonal", Bounds{0, 0, 10, 10}, Bounds{13, 14, 20, 20}, 5},
		{"overlapping", Bounds{0, 0, 10, 10}, Bounds{5, 5, 15, 15}, 0},
		{"touching edge", Bounds{0, 0, 10, 10}, Bounds{10, 5, 20, 15}, 0},
	}
	for _, c := range cases {
		if got := bboxMinDistance(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

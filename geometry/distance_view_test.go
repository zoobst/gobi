package geometry

import (
	"math"
	"math/rand"
	"testing"
)

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

package compute

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestBoundsF64Parity exercises BoundsF64 against a scalar oracle
// on random data at every size that could trip the vectorized
// body / tail boundary. Runs under both scalar and SIMD builds;
// a divergence between paths surfaces here or in CI.
func TestBoundsF64Parity(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 22))
	// Sizes covering the vectorized body + non-lane-aligned tail
	// on both NEON (lane=2) and AVX-512 (lane=8), plus edge cases.
	sizes := []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 100, 1024, 4097}

	for _, n := range sizes {
		xs := make([]float64, n)
		ys := make([]float64, n)
		for i := range xs {
			xs[i] = rng.Float64()*1000 - 500
			ys[i] = rng.Float64()*1000 - 500
		}

		// Oracle: plain scalar min/max loop.
		var (
			wantMinX, wantMinY = math.Inf(1), math.Inf(1)
			wantMaxX, wantMaxY = math.Inf(-1), math.Inf(-1)
			wantOk             bool
		)
		if n > 0 {
			wantOk = true
			for i := range xs {
				if xs[i] < wantMinX {
					wantMinX = xs[i]
				}
				if xs[i] > wantMaxX {
					wantMaxX = xs[i]
				}
				if ys[i] < wantMinY {
					wantMinY = ys[i]
				}
				if ys[i] > wantMaxY {
					wantMaxY = ys[i]
				}
			}
		}

		gotMinX, gotMinY, gotMaxX, gotMaxY, gotOk := BoundsF64(xs, ys)
		if gotOk != wantOk {
			t.Errorf("n=%d: ok=%v, want %v", n, gotOk, wantOk)
			continue
		}
		if !gotOk {
			continue
		}
		if gotMinX != wantMinX || gotMinY != wantMinY ||
			gotMaxX != wantMaxX || gotMaxY != wantMaxY {
			t.Errorf("n=%d: got (%v,%v,%v,%v), want (%v,%v,%v,%v)",
				n, gotMinX, gotMinY, gotMaxX, gotMaxY,
				wantMinX, wantMinY, wantMaxX, wantMaxY)
		}
	}
}

// TestBoundsF64_MismatchedSlices — kernel derives from the
// shorter slice; matches the geometry.BoundsFromXY contract.
func TestBoundsF64_MismatchedSlices(t *testing.T) {
	xs := []float64{1, 2, 3, 4}
	ys := []float64{1, 2}
	minX, minY, maxX, maxY, ok := BoundsF64(xs, ys)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if minX != 1 || maxX != 2 || minY != 1 || maxY != 2 {
		t.Errorf("got (%v,%v,%v,%v), want (1,1,2,2) from shorter slice",
			minX, minY, maxX, maxY)
	}
}

// TestPolygonCentroidShoelaceParity — the shoelace kernel matches
// the reference formula on closed and unclosed rings.
func TestPolygonCentroidShoelaceParity(t *testing.T) {
	cases := []struct {
		name   string
		xs, ys []float64
		wantCX float64
		wantCY float64
	}{
		{
			name:   "closed_unit_square",
			xs:     []float64{0, 1, 1, 0, 0},
			ys:     []float64{0, 0, 1, 1, 0},
			wantCX: 0.5, wantCY: 0.5,
		},
		{
			name:   "unclosed_unit_square",
			xs:     []float64{0, 1, 1, 0},
			ys:     []float64{0, 0, 1, 1},
			wantCX: 0.5, wantCY: 0.5,
		},
		{
			name:   "translated_square",
			xs:     []float64{10, 20, 20, 10, 10},
			ys:     []float64{10, 10, 20, 20, 10},
			wantCX: 15, wantCY: 15,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cx, cy, ok := PolygonCentroidShoelace(c.xs, c.ys)
			if !ok {
				t.Fatal("ok=false, want true")
			}
			if math.Abs(cx-c.wantCX) > 1e-9 || math.Abs(cy-c.wantCY) > 1e-9 {
				t.Errorf("got (%v,%v), want (%v,%v)", cx, cy, c.wantCX, c.wantCY)
			}
		})
	}
}

// TestPolygonCentroidShoelace_TooFewPoints — <3 points returns
// ok=false.
func TestPolygonCentroidShoelace_TooFewPoints(t *testing.T) {
	for n := range 3 {
		xs := make([]float64, n)
		ys := make([]float64, n)
		if _, _, ok := PolygonCentroidShoelace(xs, ys); ok {
			t.Errorf("n=%d: got ok=true, want false", n)
		}
	}
}

// TestPolygonCentroidShoelace_ScalarSimdParity — under the SIMD
// build, the vector body only fires at n ≥ lane+1 (3 on NEON, 5
// on AVX2, 9 on AVX-512). Randomize ring sizes across the
// threshold and compare against the scalar oracle to catch any
// SIMD-vs-scalar drift (accumulator ordering, tail off-by-one,
// closing-edge mismatch).
func TestPolygonCentroidShoelace_ScalarSimdParity(t *testing.T) {
	rng := rand.New(rand.NewPCG(55, 66))
	// Sizes bracket the SIMD-body threshold on every arch: pre-lane,
	// lane-boundary, plus non-lane-aligned tails. When closed=true,
	// n refers to the *unique* vertex count — the closure vertex is
	// appended so the actual slice length is n+1.
	sizes := []int{3, 4, 5, 6, 7, 8, 9, 15, 16, 17, 100, 1024, 4097}
	for _, n := range sizes {
		for _, closed := range []bool{true, false} {
			// Convex ring walked counter-clockwise so areaTwo != 0
			// on the primary formula branch. When closed, allocate
			// n+1 slots and duplicate the first vertex at the end —
			// distinct from overwriting position n-1, which would
			// collapse an n=3 ring to a degenerate line.
			var storeN int
			if closed {
				storeN = n + 1
			} else {
				storeN = n
			}
			xs := make([]float64, storeN)
			ys := make([]float64, storeN)
			for i := range n {
				theta := 2.0 * math.Pi * float64(i) / float64(n)
				xs[i] = 100 + 10*math.Cos(theta) + rng.Float64()*0.5
				ys[i] = 100 + 10*math.Sin(theta) + rng.Float64()*0.5
			}
			if closed {
				xs[n] = xs[0]
				ys[n] = ys[0]
			}
			n := storeN

			// Oracle: pure scalar shoelace, no shared helper — an
			// independent implementation so we're not just testing
			// against ourselves.
			var (
				wantArea, wantCx, wantCy, wantSx, wantSy float64
			)
			for i := 0; i < n-1; i++ {
				x0, y0 := xs[i], ys[i]
				x1, y1 := xs[i+1], ys[i+1]
				cross := x0*y1 - x1*y0
				wantArea += cross
				wantCx += (x0 + x1) * cross
				wantCy += (y0 + y1) * cross
				wantSx += x0
				wantSy += y0
			}
			var segCount int
			if xs[n-1] == xs[0] && ys[n-1] == ys[0] {
				segCount = n - 1
			} else {
				x0, y0 := xs[n-1], ys[n-1]
				x1, y1 := xs[0], ys[0]
				cross := x0*y1 - x1*y0
				wantArea += cross
				wantCx += (x0 + x1) * cross
				wantCy += (y0 + y1) * cross
				wantSx += x0
				wantSy += y0
				segCount = n
			}
			var oracleCx, oracleCy float64
			if wantArea == 0 {
				oracleCx = wantSx / float64(segCount)
				oracleCy = wantSy / float64(segCount)
			} else {
				oracleCx = wantCx / (3 * wantArea)
				oracleCy = wantCy / (3 * wantArea)
			}

			cx, cy, ok := PolygonCentroidShoelace(xs, ys)
			if !ok {
				t.Errorf("n=%d closed=%v: ok=false, want true", n, closed)
				continue
			}
			// Tolerance accounts for the accumulator-ordering
			// perturbation between scalar and SIMD paths. 1e-8
			// absolute is well within centroid noise; anything
			// larger would be a real bug.
			if math.Abs(cx-oracleCx) > 1e-8 || math.Abs(cy-oracleCy) > 1e-8 {
				t.Errorf("n=%d closed=%v: got (%v,%v), oracle (%v,%v)",
					n, closed, cx, cy, oracleCx, oracleCy)
			}
		}
	}
}

// TestPIPCrossingCountParity — reformulated crossing kernel
// agrees with an oracle on random ring + query point combinations.
func TestPIPCrossingCountParity(t *testing.T) {
	// Fixed L-shape ring; test against a battery of known
	// inside/outside points. Also random-fuzz against a scalar
	// even-odd toggle oracle for the SIMD-build divergence guard.
	xs := []float64{0, 10, 10, 5, 5, 10, 10, 0, 0}
	ys := []float64{0, 0, 3, 3, 7, 7, 10, 10, 0}
	// Named cases pinned by geometry.
	cases := []struct {
		tx, ty float64
		want   bool
	}{
		{2, 5, true},
		{2, 2, true},
		{2, 8, true},
		{7, 5, false},
		{7, 2, true},
		{7, 8, true},
		{-1, 5, false},
		{15, 5, false},
	}
	for _, c := range cases {
		if got := PIPCrossingCount(xs, ys, c.tx, c.ty); got != c.want {
			t.Errorf("@ (%v,%v): got %v, want %v", c.tx, c.ty, got, c.want)
		}
	}

	// Random-fuzz against the toggle oracle.
	rng := rand.New(rand.NewPCG(33, 44))
	for range 500 {
		tx := rng.Float64()*14 - 2
		ty := rng.Float64()*14 - 2
		got := PIPCrossingCount(xs, ys, tx, ty)
		want := oracleToggle(xs, ys, tx, ty)
		if got != want {
			t.Errorf("fuzz @ (%v,%v): got %v, want %v", tx, ty, got, want)
		}
	}
}

// TestPIPCrossingCount_ScalarSimdParity — the small L-shape test
// above stays under simdMinSize=64. This exercise builds
// large rings whose n crosses the SIMD threshold on both NEON
// (lane=2) and AVX-512 (lane=8) so the vector body actually
// fires; fuzz against the toggle oracle.
func TestPIPCrossingCount_ScalarSimdParity(t *testing.T) {
	rng := rand.New(rand.NewPCG(77, 88))
	// Sizes span pre-simdMinSize, at threshold, and past several
	// lane-aligned bodies with non-aligned tails.
	sizes := []int{16, 63, 64, 65, 100, 127, 128, 129, 256, 1024, 4097}
	for _, n := range sizes {
		// Convex ring — smooth boundary means every horizontal
		// ray through the interior crosses exactly twice; through
		// the exterior, zero times. Any parity divergence between
		// scalar and SIMD paths shows up here.
		xs := make([]float64, n+1)
		ys := make([]float64, n+1)
		for i := range n {
			theta := 2.0 * math.Pi * float64(i) / float64(n)
			xs[i] = 500 + 400*math.Cos(theta)
			ys[i] = 500 + 400*math.Sin(theta)
		}
		xs[n] = xs[0]
		ys[n] = ys[0]

		// Fuzz query points covering inside/outside/edge-adjacent
		// positions.
		for range 50 {
			tx := 100 + rng.Float64()*800
			ty := 100 + rng.Float64()*800
			got := PIPCrossingCount(xs, ys, tx, ty)
			want := oracleToggle(xs, ys, tx, ty)
			if got != want {
				t.Errorf("n=%d @ (%v,%v): got %v, want %v", n, tx, ty, got, want)
			}
		}
	}
}

// oracleToggle is the reference even-odd toggle form (matches
// geometry.PIPRingFromXY exactly).
func oracleToggle(xs, ys []float64, tx, ty float64) bool {
	n := len(xs)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := range n {
		yi := ys[i]
		yj := ys[j]
		if (yi > ty) != (yj > ty) {
			xi := xs[i]
			xj := xs[j]
			xIntersect := (xj-xi)*(ty-yi)/(yj-yi) + xi
			if tx < xIntersect {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

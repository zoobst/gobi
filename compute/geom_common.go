// Build-tag-neutral geometry kernels: the shared scalar helpers used
// by both the scalar back-ends in geom_scalar.go and the SIMD
// back-ends in geom_simd.go, plus PIPCrossingCount, which is scalar
// in every build (see the note on it below).

package compute

import "math"

// polygonCentroidShoelaceScalar is the portable shoelace loop.
// Kept separate from the exported wrapper so the SIMD version
// can call it as a tail-handling fallback if needed.
func polygonCentroidShoelaceScalar(xs, ys []float64, n int) (cx, cy float64, ok bool) {
	fx, fy := xs[0], ys[0]
	var (
		areaTwo float64
		sx, sy  float64
	)
	px, py := fx, fy
	for i := 1; i < n; i++ {
		x, y := xs[i], ys[i]
		cross := px*y - x*py
		areaTwo += cross
		cx += (px + x) * cross
		cy += (py + y) * cross
		sx += px
		sy += py
		px, py = x, y
	}
	var segCount int
	if px == fx && py == fy {
		segCount = n - 1
	} else {
		cross := px*fy - fx*py
		areaTwo += cross
		cx += (px + fx) * cross
		cy += (py + fy) * cross
		sx += px
		sy += py
		segCount = n
	}
	if areaTwo == 0 {
		return sx / float64(segCount), sy / float64(segCount), true
	}
	return cx / (3 * areaTwo), cy / (3 * areaTwo), true
}

// pipCrossingCountScalar is the portable crossing-count loop.
// Shared between the neutral wrapper and any future SIMD tail
// handler.
func pipCrossingCountScalar(xs, ys []float64, tx, ty float64, n int) bool {
	var crossings int
	j := n - 1
	for i := range n {
		yi := ys[i]
		yj := ys[j]
		if (yi > ty) != (yj > ty) {
			xi := xs[i]
			xj := xs[j]
			xIntersect := (xj-xi)*(ty-yi)/(yj-yi) + xi
			if tx < xIntersect {
				crossings++
			}
		}
		j = i
	}
	return crossings&1 == 1
}

// boundsF64ScalarNaN is the scalar min/max reduce shared by both
// builds, with the package's NaN rule: a NaN anywhere on an axis makes
// that axis's min and max NaN. Plain `<` / `>` comparisons would drop
// a NaN anywhere but the first element (both are false against NaN),
// so the third branch records it; it only runs for values that are
// neither a new min nor a new max. Caller must ensure n > 0.
func boundsF64ScalarNaN(xs, ys []float64, n int) (minX, minY, maxX, maxY float64) {
	minX, maxX = xs[0], xs[0]
	minY, maxY = ys[0], ys[0]
	nanX, nanY := xs[0] != xs[0], ys[0] != ys[0]
	for i := 1; i < n; i++ {
		x := xs[i]
		if x < minX {
			minX = x
		} else if x > maxX {
			maxX = x
		} else if x != x {
			nanX = true
		}
		y := ys[i]
		if y < minY {
			minY = y
		} else if y > maxY {
			maxY = y
		} else if y != y {
			nanY = true
		}
	}
	if nanX {
		minX, maxX = math.NaN(), math.NaN()
	}
	if nanY {
		minY, maxY = math.NaN(), math.NaN()
	}
	return
}

// PIPCrossingCount is scalar in every build. Its SIMD body regressed
// ~2.4× on 2-lane NEON (Apple M3) and still lost 22–70% at every size
// on 4-lane AVX2 (Ryzen 7 5800X): with typical rings almost no segment
// straddles the query point's y, so the scalar branch skips nearly all
// the work that every vector lane pays for.
// PIPCrossingCount — scalar back-end. Delegates to the shared
// pipCrossingCountScalar helper in geom_common.go. SIMD variant
// with a lane-parallel body lives in geom_simd.go.
//
// Semantics: returns the parity of ray-crossings when casting a
// horizontal ray from (tx, ty) rightward through the polygon
// ring defined by parallel Xs / Ys. inside=true when the
// crossing count is odd; false when even (or when the ring has
// fewer than 3 points).
//
// The reformulated crossing-count form (running `crossings int`
// accumulator, `inside = (crossings & 1) == 1` at the tail)
// breaks the scalar `inside = !inside` dependency chain used in
// geometry.PIPRingFromXY. Output matches the AoS toggle exactly.
//
// Handles closed and unclosed rings via the same (n-1, 0)
// closing-edge walk PIPRingFromXY uses.
func PIPCrossingCount(xs, ys []float64, tx, ty float64) bool {
	n := min(len(ys), len(xs))
	if n < 3 {
		return false
	}
	return pipCrossingCountScalar(xs, ys, tx, ty, n)
}

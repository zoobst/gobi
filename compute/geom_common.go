// Build-tag-neutral geometry kernels. Shared internal helpers used
// by both the scalar back-ends in geom_scalar.go and the SIMD
// back-ends in geom_simd.go.
//
// PIPCrossingCount now has a paired scalar/SIMD implementation
// (Slice 8) — this file only holds the shared scalar helper that
// the SIMD variant falls back to at the tail. The public wrapper
// lives in geom_scalar.go (build-gated fallback) and geom_simd.go
// (vector body).

package compute

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

package geometry

// PointsView is a struct-of-arrays view over a sequence of 2D or 3D
// points sharing a single CRS. Callers who need SoA-friendly access
// (bbox compute, Hilbert index, SIMD-eligible loops) call `.View()`
// on the source geometry to materialize a fresh view, then work on
// the parallel `Xs` / `Ys` slices directly.
//
// # Amortization
//
// Materialization is O(n) per `.View()` call — a fresh allocation
// of parallel float64 slabs plus a struct-to-slabs copy loop. Do
// NOT call `.View()` for a single operation; the fresh conversion
// costs more than the AoS-form equivalent. Benchmarks on
// LineString.Bounds (n=1M): AoS = 1.55 ms, SoA on held view =
// 1.10 ms (−29%), SoA with fresh view = 3.25 ms (+109%). Rule of
// thumb: break-even is roughly two SoA operations per view.
//
// The default `.Bounds()` / `.Contains()` / etc. methods on
// LineString / Polygon / MultiPolygon deliberately keep the AoS
// path so single-op callers don't regress. Downstream slices
// (WKB parse-into-view, Hilbert index, spatial predicates) offer
// entry points that skip the AoS intermediate entirely on the
// read-heavy paths where amortization actually holds.
type PointsView struct {
	// Xs and Ys are parallel arrays of the same length holding the
	// point coordinates. Never nil for a materialized view — an
	// empty geometry produces an empty (zero-length, non-nil) slice.
	Xs, Ys []float64
	// Zs holds Z coordinates when HasZ is true. Nil otherwise —
	// callers that don't check HasZ but iterate Zs get an obvious
	// panic rather than reading zeros as valid altitudes.
	Zs []float64
	// HasZ mirrors the source geometry's Is3D() at view time.
	HasZ bool
	// CRS is the coordinate reference system for the whole view.
	// In the AoS `[]Point` representation this is duplicated per
	// point; SoA carries it once at the collection level.
	CRS CRS
}

// Len returns the number of points in the view.
func (v PointsView) Len() int { return len(v.Xs) }

// Bounds returns the 2D axis-aligned bounding box of the view.
// Delegates to BoundsFromXY on Xs and Ys — Z, if present, is not
// part of the 2D bounds (matches the Bounds type's XY-only shape).
func (v PointsView) Bounds() Bounds {
	return BoundsFromXY(v.Xs, v.Ys)
}

// BoundsFromXY computes the axis-aligned bounding box of the points
// held in parallel Xs / Ys slices. Assumes len(xs) == len(ys); a
// mismatch is caller error and produces bounds derived from the
// shorter slice.
//
// This is the SoA-form kernel that spatial-predicate hot paths call
// once they've materialized (or been handed) a PointsView. Kept as
// a free function so callers holding raw `[]float64` slabs — e.g.
// from a WKB parse-into-slab entry point in a future slice, or from
// arrow-backed coordinate columns — can call it directly without
// constructing a PointsView.
//
// Portable Go body — no SIMD wire-in. Slice 6a shipped
// compute.BoundsF64 with a SIMD variant that wins 2.4× on Apple
// silicon (deep-OOO cores) but LOSES 48% on Ampere Neoverse
// (throughput-tuned server cores where vector Min/Max costs more
// per lane than scalar compare-branch). Rather than pick a
// default that regresses on one class of production hardware,
// keep the scalar body here and let advanced callers who know
// their target arch call compute.BoundsF64 explicitly. See the
// Slice 6 recap in .vscode/CLAUDE.md for the full Apple-vs-Ampere
// measurement.
func BoundsFromXY(xs, ys []float64) Bounds {
	if len(xs) == 0 || len(ys) == 0 {
		return EmptyBounds()
	}
	n := min(len(ys), len(xs))
	minX, maxX := xs[0], xs[0]
	minY, maxY := ys[0], ys[0]
	for i := 1; i < n; i++ {
		x := xs[i]
		if x < minX {
			minX = x
		} else if x > maxX {
			maxX = x
		}
		y := ys[i]
		if y < minY {
			minY = y
		} else if y > maxY {
			maxY = y
		}
	}
	return Bounds{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

// -----------------------------------------------------------------------------
// View() constructors on the concrete geometry types.
//
// Every constructor allocates fresh parallel slices and copies the
// per-point coordinates over. This is the O(n) amortization tax
// callers pay to switch to SoA; every downstream operation on the
// returned view is a tight loop over contiguous []float64 memory.
//
// The source geometry is NOT retained — the view is independent of
// the input. Callers may mutate the source's []Point after taking
// a view without observing changes through the view.
// -----------------------------------------------------------------------------

// View materializes a PointsView from a LineString's ordered points.
// Empty input yields a non-nil empty view.
func (l LineString) View() PointsView {
	return viewFromPoints(l.Points, l.HasZ, l.CRSValue)
}

// View materializes a PointsView from a MultiPoint's points. Order
// follows MultiPoint.Points.
func (m MultiPoint) View() PointsView {
	return viewFromPoints(m.Points, m.HasZ, m.CRSValue)
}

// RingViews materializes one PointsView per ring in a Polygon.
// Rings[0] is the exterior; subsequent rings are holes. Each view
// carries the polygon's CRS + HasZ. Empty Polygon returns nil.
func (p Polygon) RingViews() []PointsView {
	if len(p.Rings) == 0 {
		return nil
	}
	views := make([]PointsView, len(p.Rings))
	for i, ring := range p.Rings {
		views[i] = viewFromPoints(ring, p.HasZ, p.CRSValue)
	}
	return views
}

// LineViews materializes one PointsView per LineString in a
// MultiLineString. Each view carries the multi's CRS + HasZ.
func (m MultiLineString) LineViews() []PointsView {
	if len(m.Lines) == 0 {
		return nil
	}
	views := make([]PointsView, len(m.Lines))
	for i, line := range m.Lines {
		// Rely on the multi's HasZ/CRS rather than per-line to keep
		// views consistent with a single-CRS collection. The
		// per-line HasZ is preserved on the source; the view echoes
		// the collection.
		views[i] = viewFromPoints(line.Points, m.HasZ, m.CRSValue)
	}
	return views
}

// PolygonRingViews materializes one []PointsView per Polygon in a
// MultiPolygon. Outer slice index selects the polygon; inner slice
// mirrors that polygon's ring layout (exterior first, then holes).
func (m MultiPolygon) PolygonRingViews() [][]PointsView {
	if len(m.Polygons) == 0 {
		return nil
	}
	out := make([][]PointsView, len(m.Polygons))
	for i, poly := range m.Polygons {
		rings := make([]PointsView, len(poly.Rings))
		for j, ring := range poly.Rings {
			rings[j] = viewFromPoints(ring, m.HasZ, m.CRSValue)
		}
		out[i] = rings
	}
	return out
}

// viewFromPoints is the shared allocator+copier that all View()
// constructors delegate to. Kept in one place so the AoS→SoA hot
// path (a straight-line loop the compiler auto-vectorizes on
// arm64/amd64) is written once.
func viewFromPoints(pts []Point, hasZ bool, crs CRS) PointsView {
	v := PointsView{
		Xs:   make([]float64, len(pts)),
		Ys:   make([]float64, len(pts)),
		HasZ: hasZ,
		CRS:  crs,
	}
	if hasZ {
		v.Zs = make([]float64, len(pts))
		for i, p := range pts {
			v.Xs[i] = p.X
			v.Ys[i] = p.Y
			v.Zs[i] = p.Z
		}
	} else {
		for i, p := range pts {
			v.Xs[i] = p.X
			v.Ys[i] = p.Y
		}
	}
	return v
}

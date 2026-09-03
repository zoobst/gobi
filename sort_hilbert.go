package gobi

import (
	"fmt"
	"math"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// HilbertSortOptions controls the reference frame used by
// SortByHilbertWith. Zero-value is the same as calling
// SortByHilbert (bounds computed from the column's own centroids,
// default order).
type HilbertSortOptions struct {
	// Bounds is the reference rectangle used to normalize centroid
	// (x, y) into the Hilbert grid. Empty (zero-value) means "derive
	// from the column's own centroids" — matches SortByHilbert's
	// default behavior.
	//
	// Non-empty bounds are the point of this variant: multi-file or
	// multi-partition sorts that need a SHARED reference frame so
	// their outputs merge cleanly. Rows whose centroids fall outside
	// bounds clamp to the grid edge (see geometry.HilbertIndex).
	Bounds geometry.Bounds

	// Order is the Hilbert-curve depth. Zero uses
	// geometry.DefaultHilbertOrder. Higher values give finer
	// discrimination at cost of nothing meaningful — the sort itself
	// is O(N log N) regardless.
	Order int
}

// SortByHilbert returns a new Frame with rows reordered by the
// Hilbert-curve position of each row's geometry centroid. Spatial
// pre-sorting is what turns GeoParquet 1.1 row-group bbox pushdown
// from a synthetic-benchmark curiosity into a real-world speedup:
// after a Hilbert sort, per-row-group bboxes cluster tightly in
// space, so an AOI-shaped predicate can skip most of a file.
//
// geomCol is the name of the geometry column to sort by. If the
// column is missing or isn't a geometry column, an error is
// returned. Empty frames pass through as-is.
//
// The sort is stable — rows whose centroids hash to the same
// Hilbert cell (order-16 default → 65,536 cells per axis) retain
// their input order. Null-geometry rows sort last so downstream
// consumers can drop them with a Head/Limit if desired.
//
// The bounding box used for Hilbert normalization is computed from
// the column's own centroids, so the sort is self-contained. For
// multi-file / multi-partition sorts that need a SHARED reference
// bbox (so outputs merge cleanly), use SortByHilbertWith and pass
// HilbertSortOptions.Bounds.
//
// **Multi-part geometries:** the sort key is the row's single
// centroid, which for a scattered MultiPolygon (US with Alaska +
// Hawaii, France with overseas territories, a MultiLineString of
// disconnected roads) can land in a "no-man's-land" Hilbert cell
// far from any actual part. Rows like that can end up ordering
// worse than their per-part centroids would suggest. If it matters
// for your access pattern, Explode the multi-part column first
// (Frame.Explode) so each row carries a single spatial location,
// sort, then re-aggregate — or accept the coarser ordering and
// rely on the per-row bbox stats to handle the outliers.
func (f *Frame) SortByHilbert(geomCol string) (*Frame, error) {
	return f.SortByHilbertWith(geomCol, HilbertSortOptions{})
}

// SortByHilbertWith is the bounds-and-order-parameterized variant
// of SortByHilbert. Empty opts.Bounds falls back to the column-
// derived rectangle; empty opts.Order falls back to
// geometry.DefaultHilbertOrder.
//
// Multi-file usage: compute a single Bounds covering every partition
// (e.g. by unioning per-partition bboxes) and pass it to every
// SortByHilbertWith call so the resulting Hilbert indices are
// comparable across files. Then a downstream merge preserves global
// spatial locality.
func (f *Frame) SortByHilbertWith(geomCol string, opts HilbertSortOptions) (*Frame, error) {
	s, err := f.Column(geomCol)
	if err != nil {
		return nil, err
	}
	if !s.IsGeometry() {
		return nil, fmt.Errorf("%w: %s is not a geometry column", ErrNotGeometry, geomCol)
	}

	n := f.NumRows()
	if n == 0 {
		f.Retain()
		return f, nil
	}

	order := opts.Order
	if order == 0 {
		order = geometry.DefaultHilbertOrder
	}

	// Two passes: (1) compute centroids + column-wide bbox (only
	// used when opts.Bounds is unspecified), (2) compute Hilbert
	// indices. "Unspecified" = zero-value Bounds{} (the natural
	// callsite for a caller who didn't set the field) OR the
	// inverted-sentinel EmptyBounds() (defensive for callers who
	// explicitly reset). Either way, derive from data.
	centroids := make([]geometry.Point, n)
	nullMask := make([]bool, n)
	bounds := opts.Bounds
	deriveBounds := bounds.IsZero() || bounds.Empty()
	if deriveBounds {
		bounds = geometry.EmptyBounds()
	}
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				nullMask[idx] = true
				idx++
				continue
			}
			// Fast path: CentroidFromWKB extracts the centroid via a
			// byte-stream scan without materializing the geometry.
			// Semantics match g.Centroid() for Point / LineString /
			// Polygon / MultiPoint / MultiLineString exactly; for
			// MultiPolygon and GeometryCollection it uses bbox-center
			// (see CentroidFromWKB's docstring for the geodesic-Area
			// rationale — locality-preserving, CRS-independent, right
			// for the Hilbert-sort use case here). Zero-alloc per row.
			c, perr := geometry.CentroidFromWKB(bin.Value(i))
			if perr != nil {
				return nil, fmt.Errorf("row %d: %w", idx, perr)
			}
			centroids[idx] = c
			if deriveBounds {
				bounds = bounds.Extend(c.X, c.Y)
			}
			idx++
		}
	}

	// Compute Hilbert indices.
	indices := make([]uint64, n)
	for i, c := range centroids {
		if nullMask[i] {
			continue
		}
		indices[i] = geometry.HilbertIndex(c.X, c.Y, bounds, order)
	}

	// Stable sort a permutation by (nullMask, hilbertIndex): nulls
	// last, otherwise ascending by Hilbert position.
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(a, b int) bool {
		ra, rb := perm[a], perm[b]
		if nullMask[ra] != nullMask[rb] {
			return !nullMask[ra] // non-null before null
		}
		return indices[ra] < indices[rb]
	})

	return f.take(perm)
}

// HilbertSortWithCovering is the fused single-pass equivalent of
// `f.SortByHilbert(geomCol)` followed by `WithBboxCoveringColumns`.
// Parses each row's WKB exactly once (vs twice for the two-step
// form) — the sort's centroid computation and the covering
// columns' per-row bbox computation share a single scan pass.
//
// On the parquetio.WriteFile HilbertSort=true path this halves the
// dominant O(N·V) parse cost for large row counts.
//
// Only the specified `geomCol` gets its covering columns emitted
// from the fused pass. If the frame carries additional geometry
// columns, their bbox covering is computed via the standard
// (second-pass) route — negligible relative to the primary
// column's cost in typical single-geom-column frames.
//
// Semantics match the two-step form: null-last stable sort by
// centroid Hilbert index over bounds derived from the column's
// own centroids, covering columns declared under the geo
// metadata's `columns[geomCol].covering.bbox`.
func HilbertSortWithCovering(f *Frame, geomCol string) (*Frame, *GeoParquetMetadata, error) {
	s, err := f.Column(geomCol)
	if err != nil {
		return nil, nil, err
	}
	if !s.IsGeometry() {
		return nil, nil, fmt.Errorf("%w: %s is not a geometry column", ErrNotGeometry, geomCol)
	}

	n := f.NumRows()
	if n == 0 {
		// Nothing to sort; fall through to the standard
		// augmentation which handles empty-frame semantics uniformly.
		return WithBboxCoveringColumns(f)
	}

	// Single WKB pass: compute centroid AND bbox per row, plus the
	// column-wide centroid bounds used for Hilbert normalization.
	centroids := make([]geometry.Point, n)
	bboxes := make([]geometry.Bounds, n)
	nullMask := make([]bool, n)
	centroidBounds := geometry.EmptyBounds()
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				nullMask[idx] = true
				idx++
				continue
			}
			// Fast path: CentroidAndBoundsFromWKB scans the WKB byte
			// stream once and returns both centroid + 2D bounds
			// without allocating an intermediate geometry. Same
			// centroid semantics as the two-pass SortByHilbertWith
			// path (see CentroidFromWKB docstring); bounds match
			// BoundsFromWKB. This eliminates the "parse every row's
			// WKB and immediately discard the geometry" cost that
			// dominated the fused write path.
			c, bb, perr := geometry.CentroidAndBoundsFromWKB(bin.Value(i))
			if perr != nil {
				return nil, nil, fmt.Errorf("row %d: %w", idx, perr)
			}
			centroids[idx] = c
			bboxes[idx] = bb
			centroidBounds = centroidBounds.Extend(c.X, c.Y)
			idx++
		}
	}

	// Compute Hilbert index per non-null row.
	indices := make([]uint64, n)
	for i, c := range centroids {
		if nullMask[i] {
			continue
		}
		indices[i] = geometry.HilbertIndex(c.X, c.Y, centroidBounds, geometry.DefaultHilbertOrder)
	}

	// Sort a permutation, nulls last, ascending by Hilbert index.
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(a, b int) bool {
		ra, rb := perm[a], perm[b]
		if nullMask[ra] != nullMask[rb] {
			return !nullMask[ra]
		}
		return indices[ra] < indices[rb]
	})

	// Apply the permutation to the frame — reuses Frame.take, so no
	// WKB re-parse here either.
	sortedFrame, err := f.take(perm)
	if err != nil {
		return nil, nil, err
	}

	// Sort the pre-computed bboxes by the same permutation so they
	// line up with the sorted rows.
	sortedBboxes := make([]geometry.Bounds, n)
	sortedNullMask := make([]bool, n)
	for i, p := range perm {
		sortedBboxes[i] = bboxes[p]
		sortedNullMask[i] = nullMask[p]
	}

	// Augment the sorted frame with covering columns for `geomCol`
	// straight from sortedBboxes — no third WKB parse.
	out, meta, err := withPrecomputedBboxCovering(sortedFrame, geomCol, sortedBboxes, sortedNullMask)
	sortedFrame.Release() // withPrecomputedBboxCovering retained what it needed
	return out, meta, err
}

// withPrecomputedBboxCovering augments f with the four covering-
// bbox columns for `geomCol`, using the caller's pre-computed
// per-row bounds instead of re-parsing WKB. Handles multi-geometry
// frames by falling back to WithBboxCoveringColumns for any
// non-primary geometry columns' bboxes (rare — most frames have a
// single geometry column).
//
// nullMask[i] == true rows emit NaN for all four bbox coordinates,
// matching computeBboxColumns's null semantics.
func withPrecomputedBboxCovering(f *Frame, geomCol string, bboxes []geometry.Bounds, nullMask []bool) (*Frame, *GeoParquetMetadata, error) {
	// Count non-primary geometry columns; if any exist, fall back to
	// the standard two-pass augmentation for them. The common case
	// (a single geometry column) hits the fast path unchanged.
	extraGeoms := 0
	for _, s := range f.series {
		if s.IsGeometry() && s.name != geomCol {
			extraGeoms++
		}
	}

	// Base metadata (types + file-level bbox) computed once. Doesn't
	// need per-row bbox stats — those go on the covering columns.
	meta, err := BuildGeoParquetMetadata(f)
	if err != nil {
		return nil, nil, err
	}
	if meta == nil {
		f.Retain()
		return f, nil, nil
	}

	// Frame we'll augment: start from f, add bbox columns for
	// geomCol via the precomputed slice, and let
	// WithBboxCoveringColumns handle the rest if there are other
	// geometry columns. To keep the code simple, we materialize
	// the primary bbox columns first (fast path), then run the
	// standard helper on the result — the standard helper will
	// find that geomCol already has covering columns declared in
	// meta and skip its own scan for that column.
	//
	// Simplification: we don't fully implement the "skip already-
	// covered" path in the standard helper. Instead, when extra
	// geometries exist, we just fall through to the standard
	// two-pass helper (accepting the extra parse for geomCol). The
	// fast path fires cleanly for the single-geom-column case
	// (the 99% shape) — that's where the perf win matters most.
	if extraGeoms > 0 {
		return WithBboxCoveringColumns(f)
	}

	// Fast path: build the 4 bbox arrays from the precomputed
	// bounds, wrap them as columns, and construct the augmented
	// frame.
	pool := memoryPoolFromFrame()
	xminA, yminA, xmaxA, ymaxA := bboxArraysFromBounds(pool, bboxes, nullMask)
	xminName, yminName, xmaxName, ymaxName := BboxColumnNames(geomCol)

	origFields := f.Schema().Fields()
	newFields := make([]arrow.Field, 0, len(origFields)+4)
	newFields = append(newFields, origFields...)
	newCols := make([]arrow.Column, 0, len(origFields)+4)
	for _, s := range f.series {
		newCols = append(newCols, *arrow.NewColumn(s.field, s.col.Data()))
	}
	rollback := func() {
		for _, c := range newCols {
			c.Release()
		}
	}
	for _, bc := range []struct {
		name string
		arr  arrow.Array
	}{
		{xminName, xminA},
		{yminName, yminA},
		{xmaxName, xmaxA},
		{ymaxName, ymaxA},
	} {
		field := arrow.Field{Name: bc.name, Type: arrow.PrimitiveTypes.Float64, Nullable: false}
		newFields = append(newFields, field)
		chunked := arrow.NewChunked(field.Type, []arrow.Array{bc.arr})
		newCols = append(newCols, *arrow.NewColumn(field, chunked))
		chunked.Release()
		bc.arr.Release()
	}

	cm := meta.Columns[geomCol]
	cm.Covering = &GeoParquetCovering{
		Bbox: &GeoParquetBboxCovering{
			Xmin: []string{xminName},
			Ymin: []string{yminName},
			Xmax: []string{xmaxName},
			Ymax: []string{ymaxName},
		},
	}
	meta.Columns[geomCol] = cm

	augSchema := arrow.NewSchema(newFields, schemaMetadataPtr(f.Schema()))
	out, err := NewFrame(augSchema, newCols)
	if err != nil {
		rollback()
		return nil, nil, err
	}
	return out, meta, nil
}

// memoryPoolFromFrame returns the arrow allocator the fused path
// should use for its intermediate builders. Currently a shim over
// the default allocator — hoisted into a named function so future
// per-frame or per-request allocator plumbing has a single call
// site to update.
func memoryPoolFromFrame() memory.Allocator {
	return memory.DefaultAllocator
}

// bboxArraysFromBounds emits the 4 aligned Float64 arrays
// (xmin/ymin/xmax/ymax) from a precomputed per-row bounds slice.
// nullMask entries force NaN across all four coords, matching the
// null semantics of computeBboxColumns (WithBboxCoveringColumns's
// scanning variant).
func bboxArraysFromBounds(pool memory.Allocator, bboxes []geometry.Bounds, nullMask []bool) (xmin, ymin, xmax, ymax arrow.Array) {
	xminB := array.NewFloat64Builder(pool)
	defer xminB.Release()
	yminB := array.NewFloat64Builder(pool)
	defer yminB.Release()
	xmaxB := array.NewFloat64Builder(pool)
	defer xmaxB.Release()
	ymaxB := array.NewFloat64Builder(pool)
	defer ymaxB.Release()

	nan := math.NaN()
	for i, b := range bboxes {
		if nullMask[i] || b.Empty() {
			xminB.Append(nan)
			yminB.Append(nan)
			xmaxB.Append(nan)
			ymaxB.Append(nan)
			continue
		}
		xminB.Append(b.MinX)
		yminB.Append(b.MinY)
		xmaxB.Append(b.MaxX)
		ymaxB.Append(b.MaxY)
	}
	return xminB.NewArray(), yminB.NewArray(), xmaxB.NewArray(), ymaxB.NewArray()
}

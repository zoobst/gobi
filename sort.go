package gobi

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi/geometry"
)

// SortKey names a column to sort by and its direction. Compose multiple
// SortKeys in a single SortBy call for lexicographic (a-then-b-then-c)
// ordering — earlier keys have priority; later keys break ties.
//
// Nulls sort last regardless of Descending, matching pandas / polars
// default behavior. NaN floats also sort last (numpy semantics).
type SortKey struct {
	Column     string
	Descending bool
}

// SortBy returns a new Frame with rows arranged according to keys. The
// sort is stable: rows that compare equal on every key retain their
// input order.
//
// Key columns can be any integer, float (Float16/32/64), Decimal128/256,
// Bool, String / LargeString / StringView, Binary / LargeBinary /
// BinaryView / FixedSizeBinary (byte order), Timestamp, Date32/64,
// Time32/64, Duration or Null column, or a dictionary of any of those
// (sorted by the dictionary values, not the indices). Strings compare
// by byte order. A multi-chunk key column is concatenated into one
// array for the sort. Nulls sort last.
//
// Example:
//
//	// Chronologically by date; break ties by value descending.
//	out, err := df.SortBy(
//	    gobi.SortKey{Column: "date"},
//	    gobi.SortKey{Column: "value", Descending: true},
//	)
func (f *Frame) SortBy(keys ...SortKey) (*Frame, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("gobi: SortBy requires at least one key")
	}
	cmps := make([]rowComparator, len(keys))
	for i, k := range keys {
		s, err := f.Column(k.Column)
		if err != nil {
			return nil, err
		}
		cmp, release, err := newRowComparator(s, k.Descending)
		if err != nil {
			return nil, fmt.Errorf("gobi: SortBy key %q: %w", k.Column, err)
		}
		defer release()
		cmps[i] = cmp
	}

	n := f.NumRows()
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(a, b int) bool {
		ra, rb := perm[a], perm[b]
		for _, cmp := range cmps {
			c := cmp(ra, rb)
			if c != 0 {
				return c < 0
			}
		}
		return false
	})

	return f.take(perm)
}

// rowComparator returns -1 / 0 / +1 comparing rows i and j on a single
// key column. Null-last and direction (ascending/descending) are baked
// into the comparator at construction time so the hot loop doesn't
// branch on either.
type rowComparator func(i, j int) int

// newRowComparator returns the comparator for key column s. A
// multi-chunk key is concatenated into one array first (the key column
// only, not the frame); release frees that copy and must be called once
// the comparator is no longer used.
func newRowComparator(s Series, descending bool) (cmp rowComparator, release func(), err error) {
	release = func() {}
	chunks := s.col.Data().Chunks()
	var arr arrow.Array
	switch len(chunks) {
	case 0:
		arr = array.MakeArrayOfNull(memory.DefaultAllocator, s.DataType(), 0)
		release = arr.Release
	case 1:
		arr = chunks[0]
	default:
		cat, err := array.Concatenate(chunks, memory.DefaultAllocator)
		if err != nil {
			return nil, release, fmt.Errorf("concatenate %d-chunk sort key: %w", len(chunks), err)
		}
		arr, release = cat, cat.Release
	}
	cmp, err = arrayComparator(arr, descending)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return cmp, release, nil
}

// arrayComparator dispatches on arr's concrete type. Fixed-width
// numeric types compare their raw value slices; the rest go through
// the type's Value accessor.
func arrayComparator(arr arrow.Array, descending bool) (rowComparator, error) {
	switch a := arr.(type) {
	case *array.Int64:
		return ordered(a, a.Int64Values(), descending), nil
	case *array.Int32:
		return ordered(a, a.Int32Values(), descending), nil
	case *array.Int16:
		return ordered(a, a.Int16Values(), descending), nil
	case *array.Int8:
		return ordered(a, a.Int8Values(), descending), nil
	case *array.Uint64:
		return ordered(a, a.Uint64Values(), descending), nil
	case *array.Uint32:
		return ordered(a, a.Uint32Values(), descending), nil
	case *array.Uint16:
		return ordered(a, a.Uint16Values(), descending), nil
	case *array.Uint8:
		return ordered(a, a.Uint8Values(), descending), nil
	case *array.Timestamp:
		return ordered(a, a.TimestampValues(), descending), nil
	case *array.Date32:
		return ordered(a, a.Date32Values(), descending), nil
	case *array.Date64:
		return ordered(a, a.Date64Values(), descending), nil
	case *array.Time32:
		return ordered(a, a.Time32Values(), descending), nil
	case *array.Time64:
		return ordered(a, a.Time64Values(), descending), nil
	case *array.Duration:
		return ordered(a, a.DurationValues(), descending), nil
	case *array.Float64:
		return floats(a, a.Float64Values(), descending), nil
	case *array.Float32:
		return floats(a, a.Float32Values(), descending), nil
	case *array.Float16:
		vals := a.Values()
		return floatsBy(a, func(i int) float64 { return float64(vals[i].Float32()) }, descending), nil
	case *array.String:
		return byIndex(a, func(i, j int) int { return strings.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.LargeString:
		return byIndex(a, func(i, j int) int { return strings.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.StringView:
		return byIndex(a, func(i, j int) int { return strings.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.Binary:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.LargeBinary:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.BinaryView:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.FixedSizeBinary:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.Boolean:
		return byIndex(a, func(i, j int) int { return cmpBool(a.Value(i), a.Value(j)) }, descending), nil
	case *array.Decimal128:
		return byIndex(a, func(i, j int) int { return a.Value(i).Cmp(a.Value(j)) }, descending), nil
	case *array.Decimal256:
		return byIndex(a, func(i, j int) int { return a.Value(i).Cmp(a.Value(j)) }, descending), nil
	case *array.Dictionary:
		// Compare the dictionary entries the indices point at. The inner
		// comparator applies direction and puts null entries last.
		inner, err := arrayComparator(a.Dictionary(), descending)
		if err != nil {
			return nil, err
		}
		return func(i, j int) int {
			ni, nj := a.IsNull(i), a.IsNull(j)
			if ni || nj {
				return nullAwareCompare(ni, nj, 0, descending)
			}
			return inner(a.GetValueIndex(i), a.GetValueIndex(j))
		}, nil
	case *array.Null:
		return func(i, j int) int { return 0 }, nil
	}
	return nil, fmt.Errorf("unsupported sort key type %s", arr.DataType())
}

// ordered compares a fixed-width column through its value slice. Kept
// generic over the element type rather than going through byIndex so
// the hot numeric case is one closure call per comparison.
func ordered[T cmp.Ordered](a arrow.Array, vals []T, descending bool) rowComparator {
	sign := direction(descending)
	if a.NullN() == 0 {
		return func(i, j int) int { return sign * cmp.Compare(vals[i], vals[j]) }
	}
	return func(i, j int) int {
		ni, nj := a.IsNull(i), a.IsNull(j)
		if ni || nj {
			return nullAwareCompare(ni, nj, 0, descending)
		}
		return sign * cmp.Compare(vals[i], vals[j])
	}
}

// floats compares a float column with NaN treated like null: last in
// either direction.
func floats[T float32 | float64](a arrow.Array, vals []T, descending bool) rowComparator {
	return floatsBy(a, func(i int) float64 { return float64(vals[i]) }, descending)
}

func floatsBy(a arrow.Array, at func(int) float64, descending bool) rowComparator {
	sign := direction(descending)
	return func(i, j int) int {
		ni, nj := a.IsNull(i), a.IsNull(j)
		if ni || nj {
			return nullAwareCompare(ni, nj, 0, descending)
		}
		x, y := at(i), at(j)
		xNaN, yNaN := isNaN(x), isNaN(y)
		if xNaN || yNaN {
			return nullAwareCompare(xNaN, yNaN, 0, descending)
		}
		return sign * cmp.Compare(x, y)
	}
}

// byIndex wraps a value comparator over non-null rows with the null-last
// policy and direction.
func byIndex(a arrow.Array, cmp func(i, j int) int, descending bool) rowComparator {
	sign := direction(descending)
	if a.NullN() == 0 {
		return func(i, j int) int { return sign * cmp(i, j) }
	}
	return func(i, j int) int {
		ni, nj := a.IsNull(i), a.IsNull(j)
		if ni || nj {
			return nullAwareCompare(ni, nj, 0, descending)
		}
		return sign * cmp(i, j)
	}
}

func direction(descending bool) int {
	if descending {
		return -1
	}
	return 1
}

// nullAwareCompare composes a null-last policy with a value comparator
// and an optional direction flip. Nulls always sort last regardless of
// direction — the flip only applies to value-vs-value comparisons.
//
// Semantics:
//
//	both null      → 0
//	left null      → +1  (null sorts after non-null)
//	right null     → -1
//	neither null   → cmpVal, negated if descending
func nullAwareCompare(ni, nj bool, cmpVal int, descending bool) int {
	switch {
	case ni && nj:
		return 0
	case ni:
		return +1
	case nj:
		return -1
	}
	if descending {
		return -cmpVal
	}
	return cmpVal
}

func isNaN(f float64) bool { return f != f }

func cmpBool(a, b bool) int {
	switch {
	case !a && b:
		return -1
	case a && !b:
		return +1
	}
	return 0
}

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

// STRDefaultLeafSize is the target row-group size when
// SortBySTR is called with leafSize <= 0. Matches typical
// GeoParquet row-group defaults (5000).
const STRDefaultLeafSize = 5000

// SortBySTR returns a new Frame with rows reordered using the
// Sort-Tile-Recursive (STR) leaf-level ordering. Alternative to
// SortByHilbert with different locality tradeoffs.
//
// STR partitions the N centroids into ⌈√(N/leafSize)⌉ vertical
// strips sorted by x, then sorts each strip's centroids by y —
// producing groups of leafSize consecutive rows that share both a
// tight X range (they're in the same strip) AND a tight Y range
// (they're consecutive-in-y within the strip). Sub-groups of
// leafSize consecutive rows come out spatially rectangular.
//
// leafSize is the target row-group size (typically the same value
// you'll pass to WriteOptions.RowGroupRows). A value <= 0 falls
// back to STRDefaultLeafSize.
//
// **When to prefer STR over Hilbert:**
//
//   - Axis-aligned AOI queries (rectangular bboxes with sides
//     parallel to X/Y): STR row groups are rectangles, so an
//     axis-aligned AOI cleanly overlaps at most a strip's worth
//     of them. Hilbert curves cross tile boundaries at odd
//     angles, so a rectangular AOI can straddle more row groups.
//   - Static datasets partitioned into predictable strips
//     (latitude bands, admin regions, time-series windows).
//
// **When to stick with Hilbert:**
//
//   - Point queries or diagonal-aligned AOIs — Hilbert's curve
//     preserves 2D locality symmetrically; STR privileges the X
//     axis over Y (arbitrary but structural).
//   - Multi-file / cross-partition sorts — Hilbert indices in a
//     shared reference frame merge cleanly, whereas STR needs a
//     custom merge policy.
//
// Sort is stable within a leaf. Null geometries sort last.
func (f *Frame) SortBySTR(geomCol string, leafSize int) (*Frame, error) {
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
	if leafSize <= 0 {
		leafSize = STRDefaultLeafSize
	}

	// Extract centroids + null mask.
	type row struct {
		idx  int
		x, y float64
		null bool
	}
	rows := make([]row, n)
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			rows[idx].idx = idx
			if bin.IsNull(i) {
				rows[idx].null = true
				idx++
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", idx, err)
			}
			c := g.Centroid()
			rows[idx].x = c.X
			rows[idx].y = c.Y
			idx++
		}
	}

	// Count non-null rows for strip-count computation.
	nonNull := 0
	for _, r := range rows {
		if !r.null {
			nonNull++
		}
	}

	// Number of vertical strips: ⌈√(N/leafSize)⌉ per the STR
	// algorithm. Ceiling ensures every strip has at most leafSize
	// worth of rows once we further sort by y.
	numStrips := 1
	if nonNull > leafSize {
		numStrips = int(math.Ceil(math.Sqrt(float64(nonNull) / float64(leafSize))))
	}
	if numStrips < 1 {
		numStrips = 1
	}
	rowsPerStrip := (nonNull + numStrips - 1) / numStrips

	// Pass 1: stable-sort by (null, x) so null rows sit at the end
	// and non-nulls form left-to-right strips.
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].null != rows[b].null {
			return !rows[a].null
		}
		if rows[a].null {
			return false // stable within nulls
		}
		return rows[a].x < rows[b].x
	})

	// Pass 2: within each strip of rowsPerStrip non-nulls, stable-
	// sort by y.
	for start := 0; start < nonNull; start += rowsPerStrip {
		end := min(start+rowsPerStrip, nonNull)
		strip := rows[start:end]
		sort.SliceStable(strip, func(a, b int) bool {
			return strip[a].y < strip[b].y
		})
	}

	perm := make([]int, n)
	for i, r := range rows {
		perm[i] = r.idx
	}
	return f.take(perm)
}

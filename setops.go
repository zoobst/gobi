package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Concat is the package-level counterpart to Frame.Concat: stacks
// rows from every frame in-order, no dedupe. Useful when the caller
// holds a []*Frame and doesn't want the `frames[0].Concat(frames[1:]...)`
// dance the method form requires.
//
// Errors when frames is empty (there's nothing to return a schema
// from) or when any two frames have incompatible schemas — same
// rules as Frame.Concat.
func Concat(frames ...*Frame) (*Frame, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("gobi: Concat: no frames provided")
	}
	return frames[0].Concat(frames[1:]...)
}

// Concat returns a new Frame with rows from f followed by the rows
// of each frame in others, in the order given. No deduplication —
// this is a pure row stack (SQL UNION ALL / polars vstack / pandas
// `concat(axis=0)`).
//
// All frames must share the exact same schema: matching column
// count, order, names, and arrow types. Fingerprint mismatches
// return an error naming the offending column with both types so
// the caller can decide how to cast.
//
// Each frame's Arrow buffers are kept alive as separate chunks of
// the output — no memcpy of the underlying data. The resulting
// Frame's columns are multi-chunk; downstream ops that assume
// single-chunk (like some numeric fast paths) will fall back to
// the general path. Call `.Coalesce()` (planned) if you need a
// single-chunk output.
func (f *Frame) Concat(others ...*Frame) (*Frame, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: Frame.Concat on nil frame")
	}
	if len(others) == 0 {
		return f, nil
	}
	// Schema-compat check against every frame up front so we fail
	// fast without partial work.
	for i, o := range others {
		if o == nil {
			return nil, fmt.Errorf("gobi: Frame.Concat: others[%d] is nil", i)
		}
		if err := schemasCompatible(f.schema, o.schema); err != nil {
			return nil, fmt.Errorf("gobi: Frame.Concat: schema mismatch at frame %d: %w", i+1, err)
		}
	}
	// Build per-column chunk lists spanning f + each other.
	ncols := len(f.series)
	cols := make([]arrow.Column, ncols)
	for ci := 0; ci < ncols; ci++ {
		field := f.series[ci].field
		var chunks []arrow.Array
		for _, chunk := range f.series[ci].col.Data().Chunks() {
			chunk.Retain()
			chunks = append(chunks, chunk)
		}
		for _, o := range others {
			for _, chunk := range o.series[ci].col.Data().Chunks() {
				chunk.Retain()
				chunks = append(chunks, chunk)
			}
		}
		chunked := arrow.NewChunked(field.Type, chunks)
		// arrow.NewColumn takes ownership of `chunked`; the retains
		// above balance the future Release when the column drops.
		cols[ci] = *arrow.NewColumn(field, chunked)
		chunked.Release()
		for _, c := range chunks {
			c.Release()
		}
	}
	return NewFrame(f.schema, cols)
}

// Union returns rows in either f or other, deduplicated over cols.
// When cols is nil or empty, uniqueness is determined over every
// column. Uses the same composite-key encoding as GroupBy so
// null-vs-null comparisons collide (nulls are treated as equal for
// set membership — pandas / polars semantics, not SQL semantics).
//
// Both frames must share the exact same schema — see Concat for
// the compatibility rules and error format.
//
// Equivalent to `f.Concat(other).Unique(cols...)` but bundled for
// clarity of intent.
func (f *Frame) Union(other *Frame, cols ...string) (*Frame, error) {
	stacked, err := f.Concat(other)
	if err != nil {
		return nil, err
	}
	return stacked.Unique(cols...)
}

// Intersect returns rows that exist in both f and other,
// deduplicated over cols. cols nil/empty → all columns. Nulls are
// treated as equal (see Union for the semantics rationale).
//
// The result draws rows + column values from f (not from other) —
// so if other has a row with the same key columns but different
// non-key values, you get f's version. This matches SQL INTERSECT
// and polars behavior.
//
// Both frames must share the exact same schema.
func (f *Frame) Intersect(other *Frame, cols ...string) (*Frame, error) {
	return f.setOp(other, cols, true /* keepInOther */)
}

// Difference returns rows in f that do not appear in other,
// deduplicated over cols. cols nil/empty → all columns. Nulls are
// treated as equal.
//
// Equivalent to SQL EXCEPT / polars' `df.filter(~col.is_in(other))`
// generalized to whole-row membership.
//
// Both frames must share the exact same schema.
func (f *Frame) Difference(other *Frame, cols ...string) (*Frame, error) {
	return f.setOp(other, cols, false /* keepInOther */)
}

// setOp is the shared kernel for Intersect / Difference. keepInOther
// selects Intersect (rows whose key IS in other) vs Difference
// (rows whose key is NOT in other). Deduplication over cols happens
// on the f side — result rows keep first-occurrence order from f.
func (f *Frame) setOp(other *Frame, cols []string, keepInOther bool) (*Frame, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: Frame set op on nil frame")
	}
	if other == nil {
		return nil, fmt.Errorf("gobi: Frame set op: other is nil")
	}
	if err := schemasCompatible(f.schema, other.schema); err != nil {
		return nil, fmt.Errorf("gobi: schema mismatch: %w", err)
	}
	if len(cols) == 0 {
		cols = f.ColumnNames()
	}

	// Resolve key column series from f and other, verify hashable.
	fkeys := make([]Series, len(cols))
	okeys := make([]Series, len(cols))
	for i, name := range cols {
		fs, err := f.Column(name)
		if err != nil {
			return nil, err
		}
		os, err := other.Column(name)
		if err != nil {
			return nil, err
		}
		if !isHashable(fs.DataType()) {
			return nil, fmt.Errorf("gobi: set op: column %q has non-hashable type %s",
				name, fs.DataType())
		}
		fkeys[i] = fs
		okeys[i] = os
	}

	// Hash other's rows into a set. Composite-key byte encoding is
	// deterministic across the two frames because they share schema.
	otherKeys := make(map[string]struct{})
	var scratch []byte
	for row := 0; row < other.NumRows(); row++ {
		buf, err := composeCompositeKeyInto(scratch[:0], okeys, row)
		if err != nil {
			return nil, err
		}
		scratch = buf
		otherKeys[string(buf)] = struct{}{}
	}

	// Walk f, keep first-occurrence of rows meeting the membership
	// test, dedupe.
	fSeen := make(map[string]struct{})
	keep := make([]int, 0)
	for row := 0; row < f.NumRows(); row++ {
		buf, err := composeCompositeKeyInto(scratch[:0], fkeys, row)
		if err != nil {
			return nil, err
		}
		scratch = buf
		_, inOther := otherKeys[string(buf)]
		if inOther != keepInOther {
			continue
		}
		if _, dup := fSeen[string(buf)]; dup {
			continue
		}
		fSeen[string(buf)] = struct{}{}
		keep = append(keep, row)
	}
	return f.take(keep)
}

// Concat returns a new Series with s's values followed by each
// other's values, in order. All series must share the same arrow
// type; type mismatches return an error naming both sides.
//
// The result is a multi-chunk column — each input becomes a chunk.
// Zero-copy: no memcpy of the underlying arrow buffers.
func (s Series) Concat(others ...Series) (Series, error) {
	if s.col == nil {
		return Series{}, fmt.Errorf("gobi: Series.Concat on empty series")
	}
	if len(others) == 0 {
		return s, nil
	}
	for i, o := range others {
		if o.col == nil {
			return Series{}, fmt.Errorf("gobi: Series.Concat: others[%d] is empty", i)
		}
		if s.DataType().Fingerprint() != o.DataType().Fingerprint() {
			return Series{}, fmt.Errorf(
				"gobi: Series.Concat: type mismatch at others[%d]: left=%s, right=%s",
				i, s.DataType(), o.DataType())
		}
	}
	var chunks []arrow.Array
	for _, chunk := range s.col.Data().Chunks() {
		chunk.Retain()
		chunks = append(chunks, chunk)
	}
	for _, o := range others {
		for _, chunk := range o.col.Data().Chunks() {
			chunk.Retain()
			chunks = append(chunks, chunk)
		}
	}
	chunked := arrow.NewChunked(s.DataType(), chunks)
	col := arrow.NewColumn(s.field, chunked)
	chunked.Release()
	for _, c := range chunks {
		c.Release()
	}
	return NewSeries(col), nil
}

// Union returns the distinct non-null values in either s or other,
// in first-occurrence order. Types must match. Nulls are treated
// as equal for set membership — matching Frame.Union — but the null
// value itself is not emitted in the output (parity with
// Series.Unique which drops nulls).
func (s Series) Union(other Series) (Series, error) {
	concat, err := s.Concat(other)
	if err != nil {
		return Series{}, err
	}
	return concat.Unique()
}

// Intersect returns the distinct non-null values present in both s
// and other, in first-occurrence order. Types must match.
func (s Series) Intersect(other Series) (Series, error) {
	return s.seriesSetOp(other, true /* keepInOther */)
}

// Difference returns the distinct non-null values in s that are
// absent from other, in first-occurrence order. Types must match.
func (s Series) Difference(other Series) (Series, error) {
	return s.seriesSetOp(other, false /* keepInOther */)
}

// seriesSetOp is the shared kernel for Series.Intersect and
// Series.Difference. Symmetry with Frame.setOp — same encoding,
// same null-equal semantics, same first-occurrence ordering.
func (s Series) seriesSetOp(other Series, keepInOther bool) (Series, error) {
	if s.col == nil {
		return Series{}, fmt.Errorf("gobi: Series set op on empty series")
	}
	if other.col == nil {
		return Series{}, fmt.Errorf("gobi: Series set op: other is empty")
	}
	if s.DataType().Fingerprint() != other.DataType().Fingerprint() {
		return Series{}, fmt.Errorf(
			"gobi: Series set op: type mismatch: left=%s, right=%s",
			s.DataType(), other.DataType())
	}
	if !isHashable(s.DataType()) {
		return Series{}, fmt.Errorf("gobi: Series set op: type %s is not hashable",
			s.DataType())
	}

	// Build other's key set.
	otherKeys := make(map[string]struct{})
	var scratch []byte
	for row := 0; row < other.Len(); row++ {
		null, err := isNullAtSeries(other, row)
		if err != nil {
			return Series{}, err
		}
		if null {
			continue
		}
		buf, err := keyOfAppend(scratch[:0], other, row)
		if err != nil {
			return Series{}, err
		}
		scratch = buf
		otherKeys[string(buf)] = struct{}{}
	}

	// Walk s.
	seen := make(map[string]struct{})
	keep := make([]int, 0)
	for row := 0; row < s.Len(); row++ {
		null, err := isNullAtSeries(s, row)
		if err != nil {
			return Series{}, err
		}
		if null {
			continue
		}
		buf, err := keyOfAppend(scratch[:0], s, row)
		if err != nil {
			return Series{}, err
		}
		scratch = buf
		_, inOther := otherKeys[string(buf)]
		if inOther != keepInOther {
			continue
		}
		if _, dup := seen[string(buf)]; dup {
			continue
		}
		seen[string(buf)] = struct{}{}
		keep = append(keep, row)
	}

	// Materialize output from s at the kept indices. Reuse the same
	// takeArray path Frame ops use.
	arr, err := takeArray(memory.DefaultAllocator, s, keep)
	if err != nil {
		return Series{}, err
	}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	arr.Release()
	col := arrow.NewColumn(s.field, chunked)
	return NewSeries(col), nil
}

// schemasCompatible verifies two schemas match column count, order,
// name, and arrow-type fingerprint. Nullability differences are
// allowed since concat may widen a non-null column into a nullable
// one if the peer permits nulls at that position — that's the
// intent of "compatible" rather than "identical."
func schemasCompatible(a, b *arrow.Schema) error {
	if a.NumFields() != b.NumFields() {
		return fmt.Errorf("column count differs: %d vs %d",
			a.NumFields(), b.NumFields())
	}
	for i := 0; i < a.NumFields(); i++ {
		fa := a.Field(i)
		fb := b.Field(i)
		if fa.Name != fb.Name {
			return fmt.Errorf("column %d name differs: %q vs %q",
				i, fa.Name, fb.Name)
		}
		if fa.Type.Fingerprint() != fb.Type.Fingerprint() {
			return fmt.Errorf(
				"column %q type mismatch: left=%s, right=%s; cast one side first with WithColumn",
				fa.Name, fa.Type, fb.Type)
		}
	}
	return nil
}

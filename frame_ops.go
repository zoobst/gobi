package gobi

import (
	"context"
	"fmt"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/compute"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// ErrMaskNotBoolean is returned when Filter receives a non-boolean Series.
var ErrMaskNotBoolean = fmt.Errorf("gobi: filter mask must be a boolean series")

// Filter returns a new Frame containing only the rows where mask is true.
// Null mask entries are treated as false.
//
// The mask length must equal the frame's row count.
func (f *Frame) Filter(mask Series) (*Frame, error) {
	if mask.DataType() == nil || mask.DataType().ID() != arrow.BOOL {
		return nil, ErrMaskNotBoolean
	}
	if mask.Len() != f.NumRows() {
		return nil, fmt.Errorf("%w: mask %d vs frame %d",
			ErrColumnLenMismatch, mask.Len(), f.NumRows())
	}
	// Collect row indexes to keep.
	keep := make([]int, 0, mask.Len())
	offset := 0
	for _, chunk := range mask.col.Data().Chunks() {
		b := chunk.(*array.Boolean)
		for i := range b.Len() {
			if !b.IsNull(i) && b.Value(i) {
				keep = append(keep, offset+i)
			}
		}
		offset += b.Len()
	}
	return f.take(keep)
}

// Take returns a new Frame consisting of rows selected by the given indexes,
// in the order given. Duplicates are allowed. Out-of-range indexes produce
// an error.
func (f *Frame) Take(indexes []int) (*Frame, error) {
	for _, i := range indexes {
		if i < 0 || i >= f.NumRows() {
			return nil, fmt.Errorf("%w: %d not in [0,%d)",
				ErrRowOutOfRange, i, f.NumRows())
		}
	}
	return f.take(indexes)
}

func (f *Frame) take(indexes []int) (*Frame, error) {
	pool := memory.DefaultAllocator
	cols := make([]arrow.Column, len(f.series))
	for i, s := range f.series {
		newArr, err := takeArray(pool, s, indexes)
		if err != nil {
			return nil, err
		}
		chunked := arrow.NewChunked(newArr.DataType(), []arrow.Array{newArr})
		cols[i] = *arrow.NewColumn(s.field, chunked)
		newArr.Release()
		chunked.Release()
	}
	return NewFrame(f.schema, cols)
}

// takeArray builds a new Arrow array from s by copying values at the given
// row indexes. Handles the common Arrow primitive types plus String and
// Binary. Nulls are preserved.
//
// Single-chunk columns take the fast path: the underlying primitive slice
// is extracted once and indexed directly, avoiding the per-row chunk walk
// and type-assertion that dominated the previous implementation. Multi-
// chunk columns fall back to the row-by-row path.
func takeArray(pool memory.Allocator, s Series, indexes []int) (arrow.Array, error) {
	chunks := s.col.Data().Chunks()
	if len(chunks) == 1 {
		return takeArrayFast(pool, chunks[0], indexes)
	}
	return takeArraySlow(pool, s, indexes)
}

// takeArrayFast handles a single Arrow array (one chunk) by bulk-gathering
// into a fresh output. This path is roughly an order of magnitude faster
// than the multi-chunk fallback for large index slices.
func takeArrayFast(pool memory.Allocator, chunk arrow.Array, indexes []int) (arrow.Array, error) {
	switch a := chunk.(type) {
	case *array.Int64:
		vals := a.Int64Values()
		out := make([]int64, len(indexes))
		b := array.NewInt64Builder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for i, idx := range indexes {
				out[i] = vals[idx]
			}
			b.AppendValues(out, nil)
			return b.NewArray(), nil
		}
		validity := make([]bool, len(indexes))
		for i, idx := range indexes {
			if !a.IsNull(idx) {
				out[i] = vals[idx]
				validity[i] = true
			}
		}
		b.AppendValues(out, validity)
		return b.NewArray(), nil
	case *array.Int32:
		vals := a.Int32Values()
		out := make([]int32, len(indexes))
		b := array.NewInt32Builder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for i, idx := range indexes {
				out[i] = vals[idx]
			}
			b.AppendValues(out, nil)
			return b.NewArray(), nil
		}
		validity := make([]bool, len(indexes))
		for i, idx := range indexes {
			if !a.IsNull(idx) {
				out[i] = vals[idx]
				validity[i] = true
			}
		}
		b.AppendValues(out, validity)
		return b.NewArray(), nil
	case *array.Float64:
		vals := a.Float64Values()
		out := make([]float64, len(indexes))
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for i, idx := range indexes {
				out[i] = vals[idx]
			}
			b.AppendValues(out, nil)
			return b.NewArray(), nil
		}
		validity := make([]bool, len(indexes))
		for i, idx := range indexes {
			if !a.IsNull(idx) {
				out[i] = vals[idx]
				validity[i] = true
			}
		}
		b.AppendValues(out, validity)
		return b.NewArray(), nil
	case *array.Float32:
		vals := a.Float32Values()
		out := make([]float32, len(indexes))
		b := array.NewFloat32Builder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for i, idx := range indexes {
				out[i] = vals[idx]
			}
			b.AppendValues(out, nil)
			return b.NewArray(), nil
		}
		validity := make([]bool, len(indexes))
		for i, idx := range indexes {
			if !a.IsNull(idx) {
				out[i] = vals[idx]
				validity[i] = true
			}
		}
		b.AppendValues(out, validity)
		return b.NewArray(), nil
	case *array.Boolean:
		out := make([]bool, len(indexes))
		b := array.NewBooleanBuilder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for i, idx := range indexes {
				out[i] = a.Value(idx)
			}
			b.AppendValues(out, nil)
			return b.NewArray(), nil
		}
		validity := make([]bool, len(indexes))
		for i, idx := range indexes {
			if !a.IsNull(idx) {
				out[i] = a.Value(idx)
				validity[i] = true
			}
		}
		b.AppendValues(out, validity)
		return b.NewArray(), nil
	case *array.String:
		b := array.NewStringBuilder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for _, idx := range indexes {
				b.Append(a.Value(idx))
			}
			return b.NewArray(), nil
		}
		for _, idx := range indexes {
			if a.IsNull(idx) {
				b.AppendNull()
				continue
			}
			b.Append(a.Value(idx))
		}
		return b.NewArray(), nil
	case *array.Binary:
		b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
		defer b.Release()
		if a.NullN() == 0 {
			for _, idx := range indexes {
				b.Append(a.Value(idx))
			}
			return b.NewArray(), nil
		}
		for _, idx := range indexes {
			if a.IsNull(idx) {
				b.AppendNull()
				continue
			}
			b.Append(a.Value(idx))
		}
		return b.NewArray(), nil
	case *array.Uint64:
		b := array.NewUint64Builder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for _, idx := range indexes {
				b.Append(a.Value(idx))
			}
			return b.NewArray(), nil
		}
		for _, idx := range indexes {
			if a.IsNull(idx) {
				b.AppendNull()
				continue
			}
			b.Append(a.Value(idx))
		}
		return b.NewArray(), nil
	case *array.Uint32:
		b := array.NewUint32Builder(pool)
		defer b.Release()
		if a.NullN() == 0 {
			for _, idx := range indexes {
				b.Append(a.Value(idx))
			}
			return b.NewArray(), nil
		}
		for _, idx := range indexes {
			if a.IsNull(idx) {
				b.AppendNull()
				continue
			}
			b.Append(a.Value(idx))
		}
		return b.NewArray(), nil
	case *array.Timestamp:
		b := array.NewTimestampBuilder(pool, a.DataType().(*arrow.TimestampType))
		defer b.Release()
		if a.NullN() == 0 {
			for _, idx := range indexes {
				b.Append(a.Value(idx))
			}
			return b.NewArray(), nil
		}
		for _, idx := range indexes {
			if a.IsNull(idx) {
				b.AppendNull()
				continue
			}
			b.Append(a.Value(idx))
		}
		return b.NewArray(), nil
	case *array.List:
		lt := a.DataType().(*arrow.ListType)
		lb := array.NewListBuilder(pool, lt.Elem())
		defer lb.Release()
		for _, idx := range indexes {
			if err := appendListRowFromArray(lb, a, idx); err != nil {
				return nil, err
			}
		}
		return lb.NewArray(), nil
	}
	return takeCompute(pool, chunk, indexes, false)
}

// takeCompute gathers indexes from vals with arrow-go's compute.Take,
// which covers every Arrow type — zoned timestamps, dates, durations,
// small ints, decimals, large strings, dictionaries, structs. The
// hand-written paths above stay for the common types they're tuned
// for; this is the fallback for the rest. With negIsNull, a negative
// index emits a null row (the outer-join convention). Any other index
// outside vals is ErrRowOutOfRange.
func takeCompute(pool memory.Allocator, vals arrow.Array, indexes []int, negIsNull bool) (arrow.Array, error) {
	ib := array.NewInt64Builder(pool)
	defer ib.Release()
	ib.Reserve(len(indexes))
	for _, idx := range indexes {
		if idx < 0 && negIsNull {
			ib.AppendNull()
			continue
		}
		if idx < 0 || idx >= vals.Len() {
			return nil, fmt.Errorf("%w: %d not in [0,%d)", ErrRowOutOfRange, idx, vals.Len())
		}
		ib.Append(int64(idx))
	}
	idxArr := ib.NewArray()
	defer idxArr.Release()
	out, err := compute.TakeArray(compute.WithAllocator(context.Background(), pool), vals, idxArr)
	if err != nil {
		return nil, fmt.Errorf("%w: take %s: %v", ErrColumnTypeMismatch, vals.DataType(), err)
	}
	return out, nil
}

// takeComputeSeries is takeCompute over every chunk of s.
func takeComputeSeries(pool memory.Allocator, s Series, indexes []int, negIsNull bool) (arrow.Array, error) {
	return takeChunks(pool, s.DataType(), s.col.Data().Chunks(), indexes, negIsNull)
}

// takeChunks gathers indexes — positions across chunks laid end to end
// — without first concatenating the chunks into one column-sized copy:
// each referenced chunk is gathered on its own, and one more take puts
// those output-sized pieces in index order. Extra memory scales with
// the output, not the column. Every chunk must be exactly dt.
func takeChunks(pool memory.Allocator, dt arrow.DataType, chunks []arrow.Array, indexes []int, negIsNull bool) (arrow.Array, error) {
	for _, c := range chunks {
		if !arrow.TypeEqual(c.DataType(), dt) {
			return nil, fmt.Errorf("%w: take: chunk of %s in a %s column", ErrColumnTypeMismatch, c.DataType(), dt)
		}
	}
	if len(chunks) == 1 {
		return takeCompute(pool, chunks[0], indexes, negIsNull)
	}
	ends := make([]int, len(chunks)) // ends[c]: one past chunk c's last global row
	total := 0
	for i, c := range chunks {
		total += c.Len()
		ends[i] = total
	}
	locals := make([][]int, len(chunks)) // per chunk, its local rows in output order
	where := make([]int, len(indexes))   // output i → position in the concatenated pieces, or -1
	chunkOf := make([]int, len(indexes))
	for i, idx := range indexes {
		if idx < 0 && negIsNull {
			where[i] = -1
			continue
		}
		if idx < 0 || idx >= total {
			return nil, fmt.Errorf("%w: %d not in [0,%d)", ErrRowOutOfRange, idx, total)
		}
		c, _ := slices.BinarySearch(ends, idx+1)
		start := ends[c] - chunks[c].Len()
		chunkOf[i] = c
		where[i] = len(locals[c])
		locals[c] = append(locals[c], idx-start)
	}

	var pieces []arrow.Array
	defer func() {
		for _, p := range pieces {
			p.Release()
		}
	}()
	base := make([]int, len(chunks)) // offset of chunk c's piece in the concatenation
	n := 0
	for c, loc := range locals {
		if len(loc) == 0 {
			continue
		}
		p, err := takeCompute(pool, chunks[c], loc, false)
		if err != nil {
			return nil, err
		}
		pieces = append(pieces, p)
		base[c] = n
		n += len(loc)
	}
	switch {
	case len(pieces) == 0:
		return array.MakeArrayOfNull(pool, dt, len(indexes)), nil
	case len(pieces) == 1 && n == len(indexes):
		// Every row from one chunk and no nulls: that piece is the answer.
		pieces[0].Retain()
		return pieces[0], nil
	}
	all := pieces[0]
	if len(pieces) > 1 {
		cat, err := array.Concatenate(pieces, pool)
		if err != nil {
			return nil, fmt.Errorf("%w: take %s: %v", ErrColumnTypeMismatch, dt, err)
		}
		defer cat.Release()
		all = cat
	}
	for i := range where {
		if where[i] >= 0 {
			where[i] += base[chunkOf[i]]
		}
	}
	return takeCompute(pool, all, where, true)
}

// takeArraySlow is the multi-chunk path. The common types append row
// by row into a builder, locating each row's chunk by binary search
// (chunkLocator); that keeps peak memory at the output alone. Other
// types go through takeChunks.
func takeArraySlow(pool memory.Allocator, s Series, indexes []int) (arrow.Array, error) {
	dt := s.DataType()
	loc := newChunkLocator(s)
	switch dt.ID() {
	case arrow.INT64:
		b := array.NewInt64Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.INT32:
		b := array.NewInt32Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT64:
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT32:
		b := array.NewFloat32Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BOOL:
		b := array.NewBooleanBuilder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.STRING:
		b := array.NewStringBuilder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BINARY:
		b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
		defer b.Release()
		for _, idx := range indexes {
			if err := loc.append(idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.LIST:
		lt := dt.(*arrow.ListType)
		lb := array.NewListBuilder(pool, lt.Elem())
		defer lb.Release()
		chunks := s.col.Data().Chunks()
		for _, idx := range indexes {
			chunk, local, ok := locateRowInChunks(chunks, idx)
			if !ok {
				return nil, fmt.Errorf("%w: list row %d unreachable",
					ErrRowOutOfRange, idx)
			}
			la, ok := chunk.(*array.List)
			if !ok {
				return nil, fmt.Errorf("%w: list chunk not *array.List (%T)",
					ErrColumnTypeMismatch, chunk)
			}
			if err := appendListRowFromArray(lb, la, local); err != nil {
				return nil, err
			}
		}
		return lb.NewArray(), nil
	default:
		return takeComputeSeries(pool, s, indexes, false)
	}
}

// appendPrimitiveAt appends the value at row idx from s to b. Handles nulls
// and the same primitive types as takeArray.
func appendPrimitiveAt(s Series, idx int, b array.Builder) error {
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if idx < offset+chunk.Len() {
			return appendFromChunk(chunk, idx-offset, b)
		}
		offset += chunk.Len()
	}
	return fmt.Errorf("%w: index %d unreachable", ErrRowOutOfRange, idx)
}

// appendFromChunk appends row local of chunk to b (null-aware).
func appendFromChunk(chunk arrow.Array, local int, b array.Builder) error {
	if chunk.IsNull(local) {
		b.AppendNull()
		return nil
	}
	switch a := chunk.(type) {
	case *array.Int64:
		b.(*array.Int64Builder).Append(a.Value(local))
	case *array.Int32:
		b.(*array.Int32Builder).Append(a.Value(local))
	case *array.Uint64:
		b.(*array.Uint64Builder).Append(a.Value(local))
	case *array.Uint32:
		b.(*array.Uint32Builder).Append(a.Value(local))
	case *array.Float64:
		b.(*array.Float64Builder).Append(a.Value(local))
	case *array.Float32:
		b.(*array.Float32Builder).Append(a.Value(local))
	case *array.Boolean:
		b.(*array.BooleanBuilder).Append(a.Value(local))
	case *array.String:
		b.(*array.StringBuilder).Append(a.Value(local))
	case *array.LargeString:
		b.(*array.LargeStringBuilder).Append(a.Value(local))
	case *array.Binary:
		b.(*array.BinaryBuilder).Append(a.Value(local))
	case *array.Timestamp:
		b.(*array.TimestampBuilder).Append(a.Value(local))
	default:
		return fmt.Errorf("%w: unsupported chunk type %T",
			ErrColumnTypeMismatch, chunk)
	}
	return nil
}

// chunkLocator finds a row's chunk by binary search over the chunks'
// end offsets, so a take over many chunks costs O(rows × log chunks)
// rather than walking the chunk list for every row.
type chunkLocator struct {
	chunks []arrow.Array
	ends   []int // ends[c]: one past chunk c's last row
}

func newChunkLocator(s Series) chunkLocator {
	chunks := s.col.Data().Chunks()
	ends := make([]int, len(chunks))
	total := 0
	for i, c := range chunks {
		total += c.Len()
		ends[i] = total
	}
	return chunkLocator{chunks: chunks, ends: ends}
}

// append appends row idx to b.
func (l chunkLocator) append(idx int, b array.Builder) error {
	c, _ := slices.BinarySearch(l.ends, idx+1)
	if idx < 0 || c >= len(l.chunks) {
		return fmt.Errorf("%w: index %d unreachable", ErrRowOutOfRange, idx)
	}
	return appendFromChunk(l.chunks[c], idx-(l.ends[c]-l.chunks[c].Len()), b)
}

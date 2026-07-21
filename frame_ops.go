package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
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
	}
	return nil, fmt.Errorf("%w: take not implemented for %T", ErrColumnTypeMismatch, chunk)
}

// takeArraySlow is the general per-row path for multi-chunk columns.
func takeArraySlow(pool memory.Allocator, s Series, indexes []int) (arrow.Array, error) {
	dt := s.DataType()
	switch dt.ID() {
	case arrow.INT64:
		b := array.NewInt64Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.INT32:
		b := array.NewInt32Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT64:
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT32:
		b := array.NewFloat32Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BOOL:
		b := array.NewBooleanBuilder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.STRING:
		b := array.NewStringBuilder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BINARY:
		b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
		defer b.Release()
		for _, idx := range indexes {
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	default:
		return nil, fmt.Errorf("%w: take not implemented for %s",
			ErrColumnTypeMismatch, dt)
	}
}

// appendPrimitiveAt appends the value at row idx from s to b. Handles nulls
// and the same primitive types as takeArray.
func appendPrimitiveAt(s Series, idx int, b array.Builder) error {
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if idx < offset+chunk.Len() {
			local := idx - offset
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
		offset += chunk.Len()
	}
	return fmt.Errorf("%w: index %d unreachable", ErrRowOutOfRange, idx)
}

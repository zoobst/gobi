package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// -----------------------------------------------------------------------------
// Typed extraction — pull column values out of a Series into a fresh
// []T Go slice, walking chunks internally.
//
// Every method:
//   - Returns an error if the Series' arrow type doesn't match the
//     method's declared T. No implicit widening; cast explicitly if
//     you need it (a future gobi.Cast expression will help here).
//   - Returns a NEW slice that owns its memory — safe to mutate,
//     safe to hold past the source Frame's Release.
//   - Represents nulls as the type's zero value. Pair with Nulls()
//     to distinguish null-0 from explicit-0. Callers that don't
//     care about nulls can ignore this and just read the values.
//
// Sits between the arrow chunk API (fast but requires chunk-walking
// + type-asserting per Series) and ToStructs (needs a matching Go
// struct). Use these when you know the column type and want a
// self-contained []T ready for range loops.
// -----------------------------------------------------------------------------

// Int64s returns the Series values as []int64. Nulls come through as 0.
func (s Series) Int64s() ([]int64, error) {
	return extractInts(s, arrow.INT64, func(a arrow.Array, i int) int64 {
		return a.(*array.Int64).Value(i)
	})
}

// Int32s returns the Series values as []int32. Nulls come through as 0.
func (s Series) Int32s() ([]int32, error) {
	return extractInts(s, arrow.INT32, func(a arrow.Array, i int) int32 {
		return a.(*array.Int32).Value(i)
	})
}

// Uint64s returns the Series values as []uint64. Nulls come through as 0.
func (s Series) Uint64s() ([]uint64, error) {
	return extractInts(s, arrow.UINT64, func(a arrow.Array, i int) uint64 {
		return a.(*array.Uint64).Value(i)
	})
}

// Uint32s returns the Series values as []uint32. Nulls come through as 0.
func (s Series) Uint32s() ([]uint32, error) {
	return extractInts(s, arrow.UINT32, func(a arrow.Array, i int) uint32 {
		return a.(*array.Uint32).Value(i)
	})
}

// Float64s returns the Series values as []float64. Nulls come through as 0.
func (s Series) Float64s() ([]float64, error) {
	return extractInts(s, arrow.FLOAT64, func(a arrow.Array, i int) float64 {
		return a.(*array.Float64).Value(i)
	})
}

// Float32s returns the Series values as []float32. Nulls come through as 0.
func (s Series) Float32s() ([]float32, error) {
	return extractInts(s, arrow.FLOAT32, func(a arrow.Array, i int) float32 {
		return a.(*array.Float32).Value(i)
	})
}

// Strings returns the Series values as []string. Nulls come through as "".
// Works for both String and LargeString columns.
func (s Series) Strings() ([]string, error) {
	if s.col == nil {
		return nil, fmt.Errorf("Series.Strings: nil column")
	}
	got := s.DataType().ID()
	if got != arrow.STRING && got != arrow.LARGE_STRING {
		return nil, fmt.Errorf("%w: Series.Strings requires String/LargeString, got %s",
			ErrColumnTypeMismatch, s.DataType())
	}
	out := make([]string, s.Len())
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		n := chunk.Len()
		switch a := chunk.(type) {
		case *array.String:
			for i := range n {
				if !a.IsNull(i) {
					out[idx+i] = a.Value(i)
				}
			}
		case *array.LargeString:
			for i := range n {
				if !a.IsNull(i) {
					out[idx+i] = a.Value(i)
				}
			}
		default:
			return nil, fmt.Errorf("Series.Strings: unexpected chunk type %T", chunk)
		}
		idx += n
	}
	return out, nil
}

// Bools returns the Series values as []bool. Nulls come through as false.
func (s Series) Bools() ([]bool, error) {
	if s.col == nil {
		return nil, fmt.Errorf("Series.Bools: nil column")
	}
	if s.DataType().ID() != arrow.BOOL {
		return nil, fmt.Errorf("%w: Series.Bools requires Boolean, got %s",
			ErrColumnTypeMismatch, s.DataType())
	}
	out := make([]bool, s.Len())
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		a := chunk.(*array.Boolean)
		n := a.Len()
		for i := range n {
			if !a.IsNull(i) {
				out[idx+i] = a.Value(i)
			}
		}
		idx += n
	}
	return out, nil
}

// Timestamps returns the Series values as []arrow.Timestamp (int64
// under the hood). Nulls come through as 0. The unit and timezone are
// preserved in the source column's arrow type; use DataType() to
// recover them if you need to interpret the values.
func (s Series) Timestamps() ([]arrow.Timestamp, error) {
	if s.col == nil {
		return nil, fmt.Errorf("Series.Timestamps: nil column")
	}
	if s.DataType().ID() != arrow.TIMESTAMP {
		return nil, fmt.Errorf("%w: Series.Timestamps requires Timestamp, got %s",
			ErrColumnTypeMismatch, s.DataType())
	}
	out := make([]arrow.Timestamp, s.Len())
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		a := chunk.(*array.Timestamp)
		n := a.Len()
		for i := range n {
			if !a.IsNull(i) {
				out[idx+i] = a.Value(i)
			}
		}
		idx += n
	}
	return out, nil
}

// Nulls returns a []bool where true means "row i is null". Zero-length
// series → empty slice, no error. Use alongside a value extractor to
// distinguish arrow-null from a zero-valued row.
//
//	vals, _ := s.Int64s()
//	nulls := s.Nulls()
//	for i, v := range vals {
//	    if nulls[i] { continue } // skip nulls
//	    // ... use v
//	}
//
// Fast path: chunks with no nulls (per arrow's cached NullN) skip the
// bitmap walk entirely — leave those output slots at their zero
// value. Chunks with nulls read the raw validity bitmap once per
// row rather than the (offset-recomputing) IsNull call. Fully-null
// Series still allocate the []bool.
func (s Series) Nulls() []bool {
	if s.col == nil {
		return nil
	}
	out := make([]bool, s.Len())
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		n := chunk.Len()
		if chunk.NullN() == 0 {
			idx += n
			continue
		}
		nulls := chunk.NullBitmapBytes()
		if len(nulls) == 0 {
			for i := range n {
				if chunk.IsNull(i) {
					out[idx+i] = true
				}
			}
			idx += n
			continue
		}
		off := chunk.Data().Offset()
		for i := range n {
			bit := off + i
			if nulls[bit>>3]&(1<<uint(bit&7)) == 0 {
				out[idx+i] = true
			}
		}
		idx += n
	}
	return out
}

// extractInts is the shared numeric-extractor path. Generic over the
// output slice element type; each caller passes its arrow.Type.ID and
// a per-index value accessor. Nulls set the corresponding slot to the
// zero value of T.
func extractInts[T any](s Series, want arrow.Type, at func(arrow.Array, int) T) ([]T, error) {
	if s.col == nil {
		return nil, fmt.Errorf("Series: nil column")
	}
	if got := s.DataType().ID(); got != want {
		return nil, fmt.Errorf("%w: expected %s, got %s",
			ErrColumnTypeMismatch, want, s.DataType())
	}
	out := make([]T, s.Len())
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		n := chunk.Len()
		for i := range n {
			if !chunk.IsNull(i) {
				out[idx+i] = at(chunk, i)
			}
		}
		idx += n
	}
	return out, nil
}

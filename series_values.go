package gobi

import (
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// -----------------------------------------------------------------------------
// Zero-copy typed value accessors.
//
// Return (slice, true) when the Series is single-chunk and the
// requested type matches — the slice aliases the underlying arrow
// buffer, so writes are aliased writes into arrow-owned memory and
// the slice becomes invalid when the column is Released. On
// multi-chunk Series or type mismatch, return (nil, false); callers
// should either concat via Rechunk or use the type-generic
// Series.numericAt walker.
//
// Motivating use case: users writing their own SIMD-style loops
// (e.g. the future goexperiment.simd rewrites) or hooking into
// existing Go-native numeric libraries want the raw slice, not a
// per-row Value(i) accessor. The arrow-go Array types already
// expose zero-copy slices at the array level (arr.Int64Values());
// this surface is the Series-level equivalent that folds the
// single-chunk assertion into the return.
// -----------------------------------------------------------------------------

// Float64Values returns a zero-copy view of the underlying float64
// slice when s is a single-chunk Float64 Series. Returns (nil, false)
// on multi-chunk or type mismatch. Null rows still occupy positions
// in the slice — combine with Nulls() to filter them.
func (s Series) Float64Values() ([]float64, bool) {
	if s.col == nil {
		return nil, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false
	}
	a, ok := chunks[0].(*array.Float64)
	if !ok {
		return nil, false
	}
	return a.Float64Values(), true
}

// Int64Values returns a zero-copy view of the underlying int64 slice
// when s is a single-chunk Int64 Series. Returns (nil, false) on
// multi-chunk or type mismatch.
func (s Series) Int64Values() ([]int64, bool) {
	if s.col == nil {
		return nil, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false
	}
	a, ok := chunks[0].(*array.Int64)
	if !ok {
		return nil, false
	}
	return a.Int64Values(), true
}

// Float32Values returns a zero-copy view of the underlying float32
// slice when s is a single-chunk Float32 Series. Returns (nil, false)
// on multi-chunk or type mismatch.
func (s Series) Float32Values() ([]float32, bool) {
	if s.col == nil {
		return nil, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false
	}
	a, ok := chunks[0].(*array.Float32)
	if !ok {
		return nil, false
	}
	return a.Float32Values(), true
}

// Int32Values returns a zero-copy view of the underlying int32 slice
// when s is a single-chunk Int32 Series. Returns (nil, false) on
// multi-chunk or type mismatch.
func (s Series) Int32Values() ([]int32, bool) {
	if s.col == nil {
		return nil, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false
	}
	a, ok := chunks[0].(*array.Int32)
	if !ok {
		return nil, false
	}
	return a.Int32Values(), true
}

// Uint64Values returns a zero-copy view of the underlying uint64
// slice when s is a single-chunk Uint64 Series.
func (s Series) Uint64Values() ([]uint64, bool) {
	if s.col == nil {
		return nil, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false
	}
	a, ok := chunks[0].(*array.Uint64)
	if !ok {
		return nil, false
	}
	return a.Uint64Values(), true
}

// Uint32Values returns a zero-copy view of the underlying uint32
// slice when s is a single-chunk Uint32 Series.
func (s Series) Uint32Values() ([]uint32, bool) {
	if s.col == nil {
		return nil, false
	}
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false
	}
	a, ok := chunks[0].(*array.Uint32)
	if !ok {
		return nil, false
	}
	return a.Uint32Values(), true
}

// -----------------------------------------------------------------------------
// Vectorized null access.
// -----------------------------------------------------------------------------

// HasNulls reports whether the Series has any null rows. Cheap —
// consults arrow's cached NullN metadata per chunk without touching
// the bitmap.
func (s Series) HasNulls() bool {
	if s.col == nil {
		return false
	}
	for _, chunk := range s.col.Data().Chunks() {
		if chunk.NullN() > 0 {
			return true
		}
	}
	return false
}

// NullCount returns the total number of null rows across all chunks.
// Cheap — sums each chunk's cached NullN.
func (s Series) NullCount() int {
	if s.col == nil {
		return 0
	}
	var n int
	for _, chunk := range s.col.Data().Chunks() {
		n += chunk.NullN()
	}
	return n
}

// Ensure the arrow import is used even when the package is compiled
// standalone (arrow.Field appears in Series but not directly here).
var _ = arrow.PrimitiveTypes.Float64

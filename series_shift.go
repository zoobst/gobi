package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Shift returns a new Series with values shifted by n positions.
// Positive n shifts values forward (down): the first n positions
// become null, and each output position i gets the source value
// from position i-n when that index is in bounds.
//
// Negative n shifts backward (up): the last |n| positions become
// null; output position i gets the source value from position i+|n|.
//
// n=0 is a no-op that still returns a fresh Series (new arrow array,
// same values). Extreme |n| >= Len() returns an all-null Series of
// the same type + name — same as pandas / polars.
//
// Nulls in the source at the source index propagate to the output.
// The output preserves the source's arrow type, name, and field
// metadata (including the geometry tag, if any).
func (s Series) Shift(n int) (Series, error) {
	if s.col == nil {
		return Series{}, fmt.Errorf("gobi: Series.Shift on empty series")
	}
	length := s.Len()
	// Build src[i]: source row index for output row i. -1 = emit null.
	src := make([]int, length)
	for i := range src {
		j := i - n // positive n → look "back"; negative n → look "forward"
		if j < 0 || j >= length {
			src[i] = -1
			continue
		}
		src[i] = j
	}
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, s.DataType())
	if err != nil {
		return Series{}, fmt.Errorf("gobi: Series.Shift: %w", err)
	}
	defer b.Release()
	for _, j := range src {
		if j < 0 {
			b.AppendNull()
			continue
		}
		if err := appendPrimitiveAt(s, j, b); err != nil {
			return Series{}, err
		}
	}
	arr := b.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	// Preserve the source's field (name, metadata, nullable flag) —
	// nullability becomes irrelevant since we've almost certainly
	// introduced nulls at the edges.
	f := s.field
	f.Nullable = true
	col := arrow.NewColumn(f, chunked)
	return NewSeries(col), nil
}

// Diff returns a Series holding the element-wise difference between
// s and its n-lagged self: `out[i] = s[i] - s[i-n]`. The first n
// positions are null (no previous value to subtract). n must be
// positive; n=0 is disallowed (would trivially be all zeros); use
// Shift(-n) if you want a negative-lag first-difference.
//
// Only numeric types are supported (matching the underlying
// Series.Sub). Non-numeric callers should compose Shift + a
// domain-specific comparison instead.
//
// This is the columnar equivalent of pandas / polars `diff(n)`
// used for period-over-period changes, discrete derivatives, and
// change-point detection.
func (s Series) Diff(n int) (Series, error) {
	if n <= 0 {
		return Series{}, fmt.Errorf("gobi: Series.Diff: n must be positive, got %d", n)
	}
	if !s.isNumeric() {
		return Series{}, fmt.Errorf("%w: Series.Diff requires a numeric series, got %s",
			ErrNotNumeric, s.DataType())
	}
	prev, err := s.Shift(n)
	if err != nil {
		return Series{}, err
	}
	return s.Sub(prev)
}

// builderForTypeIsGeneric documents that the primitive builder
// helpers used by Shift live in groupby.go (builderForType) and
// frame_ops.go (appendPrimitiveAt). Kept here for grep-ability if
// someone searches for "Shift" and wants to see the type coverage.
var _ = array.NewInt64Builder

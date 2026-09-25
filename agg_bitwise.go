package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// isBitwiseAgg reports whether k is one of the bitwise reductions.
func isBitwiseAgg(k AggKind) bool {
	return k == AggBitOr || k == AggBitAnd || k == AggBitXor
}

// preservesSourceType reports whether k's output column takes the
// source column's arrow type (rather than Float64 / Int64). Min / Max
// additionally preserve Timestamp sources; callers handle that case
// separately since it depends on the source type.
func (k AggKind) preservesSourceType() bool {
	switch k {
	case AggFirst, AggLast, AggMode, AggBitOr, AggBitAnd, AggBitXor:
		return true
	}
	return false
}

// isBitwiseInput reports whether dt is an integer type the bitwise
// aggregations accept.
func isBitwiseInput(dt arrow.DataType) bool {
	switch dt.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64:
		return true
	}
	return false
}

// checkBitwiseInput returns an ErrNotNumeric-wrapped error when a
// bitwise aggregation targets a non-integer column.
func checkBitwiseInput(kind AggKind, column string, dt arrow.DataType) error {
	if isBitwiseInput(dt) {
		return nil
	}
	return fmt.Errorf("%w: %s on %q requires an integer column, got %s",
		ErrNotNumeric, kind, column, dt)
}

// bitAcc folds a group's non-null integer values with OR / AND / XOR.
//
// Values are folded as their sign-extended uint64 bit pattern and cast
// back to the source width in Finalize. Bitwise ops commute with
// truncation, so the low bits come out the same as folding at the
// source width, and one accumulator serves every integer type.
//
// Shared by the eager GroupBy path, the streaming executor, and Over.
// Output type is the source column's; an all-null or empty group
// finalizes to nil (null), matching Min / Max.
type bitAcc struct {
	kind AggKind
	bits uint64
	seen bool
	dt   arrow.DataType // source type, set on the first Update
}

func newBitAcc(kind AggKind) *bitAcc {
	a := &bitAcc{kind: kind}
	if kind == AggBitAnd {
		a.bits = ^uint64(0) // identity for AND
	}
	return a
}

func (a *bitAcc) Update(col Series, rows []int) error {
	dt := col.DataType()
	if err := checkBitwiseInput(a.kind, col.Name(), dt); err != nil {
		return err
	}
	if a.dt == nil {
		a.dt = dt
	}
	chunks := col.col.Data().Chunks()
	if len(chunks) == 1 {
		a.foldArray(chunks[0], rows)
		return nil
	}
	cur := newChunkCursor(col)
	one := []int{0}
	for _, r := range rows {
		chunk, local, err := cur.locate(r)
		if err != nil {
			return err
		}
		one[0] = local
		a.foldArray(chunk, one)
	}
	return nil
}

// foldArray folds arr[rows] (rows local to arr), skipping nulls.
func (a *bitAcc) foldArray(arr arrow.Array, rows []int) {
	switch t := arr.(type) {
	case *array.Int8:
		foldBits(a, arr, t.Int8Values(), rows)
	case *array.Int16:
		foldBits(a, arr, t.Int16Values(), rows)
	case *array.Int32:
		foldBits(a, arr, t.Int32Values(), rows)
	case *array.Int64:
		foldBits(a, arr, t.Int64Values(), rows)
	case *array.Uint8:
		foldBits(a, arr, t.Uint8Values(), rows)
	case *array.Uint16:
		foldBits(a, arr, t.Uint16Values(), rows)
	case *array.Uint32:
		foldBits(a, arr, t.Uint32Values(), rows)
	case *array.Uint64:
		foldBits(a, arr, t.Uint64Values(), rows)
	}
}

type bitwiseInt interface {
	~int8 | ~int16 | ~int32 | ~int64 | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

func foldBits[T bitwiseInt](a *bitAcc, arr arrow.Array, vals []T, rows []int) {
	for _, r := range rows {
		if arr.IsNull(r) {
			continue
		}
		v := uint64(vals[r])
		switch a.kind {
		case AggBitOr:
			a.bits |= v
		case AggBitAnd:
			a.bits &= v
		case AggBitXor:
			a.bits ^= v
		}
		a.seen = true
	}
}

// Finalize returns the folded value as the source column's Go type
// (int8 … uint64), or nil when the group had no non-null values.
func (a *bitAcc) Finalize() any {
	if !a.seen {
		return nil
	}
	switch a.dt.ID() {
	case arrow.INT8:
		return int8(a.bits)
	case arrow.INT16:
		return int16(a.bits)
	case arrow.INT32:
		return int32(a.bits)
	case arrow.INT64:
		return int64(a.bits)
	case arrow.UINT8:
		return uint8(a.bits)
	case arrow.UINT16:
		return uint16(a.bits)
	case arrow.UINT32:
		return uint32(a.bits)
	}
	return a.bits
}

// OutputType is the source column's type once known. Before any
// Update (type inference, zero-row input) it falls back to Int64;
// callers that know the source type use it directly, as for Mode.
func (a *bitAcc) OutputType() arrow.DataType {
	if a.dt != nil {
		return a.dt
	}
	return arrow.PrimitiveTypes.Int64
}

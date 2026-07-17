package gobi

import (
	"fmt"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// JoinType selects the join behavior.
type JoinType uint8

const (
	// JoinInner returns rows where the key exists on both sides.
	JoinInner JoinType = iota
	// JoinLeft returns every row from the left frame, with nulls where the
	// right side has no matching key.
	JoinLeft
)

// Join returns a new Frame formed by combining rows of f (the left frame)
// and right, where the left column named leftKey equals the right column
// named rightKey. The join key must be a hashable type (String, Int64,
// Int32, Bool).
//
// The result contains all columns from the left frame followed by all
// columns from the right frame except the join key. Right-side columns
// whose names collide with left-side columns are renamed with a "_right"
// suffix.
func (f *Frame) Join(right *Frame, leftKey, rightKey string, kind JoinType) (*Frame, error) {
	lKey, err := f.Column(leftKey)
	if err != nil {
		return nil, err
	}
	rKey, err := right.Column(rightKey)
	if err != nil {
		return nil, err
	}
	if !isHashable(lKey.DataType()) {
		return nil, fmt.Errorf("gobi: left key type %s is not hashable", lKey.DataType())
	}
	if lKey.DataType().ID() != rKey.DataType().ID() {
		return nil, fmt.Errorf("%w: %s vs %s", ErrColumnTypeMismatch,
			lKey.DataType(), rKey.DataType())
	}

	// Build a hash of key → []rowIdx on the right frame.
	rightIndex := map[string][]int{}
	for row := 0; row < right.NumRows(); row++ {
		k, err := keyOf(rKey, row)
		if err != nil {
			return nil, err
		}
		if k[0] == 0x00 {
			continue // right-side nulls never match
		}
		rightIndex[string(k)] = append(rightIndex[string(k)], row)
	}

	// Walk the left frame, emitting one row per match (inner) or per row
	// when unmatched left-side rows should be kept (left).
	var (
		leftIdxs  []int
		rightIdxs []int // -1 means "no match" (used only for left join)
	)
	for lRow := 0; lRow < f.NumRows(); lRow++ {
		k, err := keyOf(lKey, lRow)
		if err != nil {
			return nil, err
		}
		if k[0] == 0x00 {
			if kind == JoinLeft {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, -1)
			}
			continue
		}
		matches := rightIndex[string(k)]
		if len(matches) == 0 {
			if kind == JoinLeft {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, -1)
			}
			continue
		}
		for _, rRow := range matches {
			leftIdxs = append(leftIdxs, lRow)
			rightIdxs = append(rightIdxs, rRow)
		}
	}

	// Emit output columns: left columns in order, then right columns except
	// rightKey. Rename right-side columns that collide with left-side names.
	leftNames := f.ColumnNames()
	leftNameSet := map[string]struct{}{}
	for _, n := range leftNames {
		leftNameSet[n] = struct{}{}
	}

	pool := memory.DefaultAllocator
	outFields := make([]arrow.Field, 0, len(f.series)+len(right.series))
	outColumns := make([]arrow.Column, 0, cap(outFields))

	// Left-side columns.
	for _, s := range f.series {
		arr, err := takeArray(pool, s, leftIdxs)
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, s.field)
		outColumns = append(outColumns, *arrow.NewColumn(s.field, chunked))
	}

	// Right-side columns.
	for _, s := range right.series {
		if s.name == rightKey {
			continue
		}
		arr, err := takeArrayWithNulls(pool, s, rightIdxs)
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		field := s.field
		if _, clash := leftNameSet[s.name]; clash {
			field.Name = s.name + "_right"
		}
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, field)
		outColumns = append(outColumns, *arrow.NewColumn(field, chunked))
	}

	schema := arrow.NewSchema(outFields, nil)
	return NewFrame(schema, outColumns)
}

// takeArrayWithNulls is like takeArray but permits -1 in indexes to mean
// "emit null" — used to support left joins where the right side may have
// no match.
func takeArrayWithNulls(pool memory.Allocator, s Series, indexes []int) (arrow.Array, error) {
	dt := s.DataType()
	switch dt.ID() {
	case arrow.INT64:
		b := array.NewInt64Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.INT32:
		b := array.NewInt32Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT64:
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT32:
		b := array.NewFloat32Builder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BOOL:
		b := array.NewBooleanBuilder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.STRING:
		b := array.NewStringBuilder(pool)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BINARY:
		b := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
		defer b.Release()
		for _, idx := range indexes {
			if idx < 0 {
				b.AppendNull()
				continue
			}
			if err := appendPrimitiveAt(s, idx, b); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	default:
		return nil, fmt.Errorf("%w: join not implemented for %s",
			ErrColumnTypeMismatch, dt)
	}
}

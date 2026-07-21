package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// JoinType selects the join behavior.
type JoinType uint8

const (
	// JoinInner returns rows where the key exists on both sides.
	JoinInner JoinType = iota
	// JoinLeft returns every row from the left frame, with nulls where the
	// right side has no matching key.
	JoinLeft
	// JoinRight returns every row from the right frame, with nulls where
	// the left side has no matching key. Row order follows the right
	// frame's row order.
	JoinRight
	// JoinFull (a.k.a. FULL OUTER) returns the union of JoinLeft and
	// JoinRight: every row from both frames, matched where possible and
	// null-padded on the missing side otherwise. Left rows come first
	// (in left-row order), followed by unmatched right rows in right-row
	// order.
	JoinFull
	// JoinSemi returns each left row that has at least one match on the
	// right, without duplication when there are multiple matches. Only
	// left-side columns are emitted.
	JoinSemi
	// JoinAnti returns each left row that has NO match on the right.
	// Only left-side columns are emitted. Left rows with a null key are
	// treated as unmatched.
	JoinAnti
)

// Join returns a new Frame formed by combining rows of f (the left frame)
// and right, where the left column named leftKey equals the right column
// named rightKey. The join key must be a hashable type (String, Int64,
// Int32, Bool, Uint32, Uint64, Float64, Timestamp).
//
// The result contains all columns from the left frame followed by all
// columns from the right frame except the join key. Right-side columns
// whose names collide with left-side columns are renamed with a
// "_right" suffix. For JoinSemi and JoinAnti, only left-side columns
// appear in the output.
//
// Null keys never match (SQL semantics). In JoinLeft, JoinFull, and
// JoinAnti a null-keyed left row still appears in the output; in
// JoinRight and JoinFull a null-keyed right row still appears; in
// JoinSemi and JoinInner it is filtered out.
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

	switch kind {
	case JoinInner, JoinLeft, JoinFull, JoinSemi, JoinAnti:
		return f.joinHashRight(right, leftKey, rightKey, lKey, rKey, kind)
	case JoinRight:
		return f.joinHashLeft(right, leftKey, rightKey, lKey, rKey)
	default:
		return nil, fmt.Errorf("gobi: unknown join kind %d", kind)
	}
}

// joinHashRight builds a hash of the right frame's key column and walks
// the left frame. Handles Inner, Left, Full, Semi, and Anti — every join
// kind that iterates the left frame as the outer loop.
func (f *Frame) joinHashRight(right *Frame, leftKey, rightKey string,
	lKey, rKey Series, kind JoinType,
) (*Frame, error) {
	rightIndex, err := buildKeyIndex(rKey, right.NumRows())
	if err != nil {
		return nil, err
	}

	// Track which right rows have been matched so JoinFull can emit
	// unmatched right rows afterwards.
	var rightMatched []bool
	if kind == JoinFull {
		rightMatched = make([]bool, right.NumRows())
	}

	var leftIdxs, rightIdxs []int
	for lRow := range f.NumRows() {
		k, err := keyOf(lKey, lRow)
		if err != nil {
			return nil, err
		}
		var matches []int
		if k[0] != 0x00 {
			matches = rightIndex[string(k)]
		}

		switch kind {
		case JoinSemi:
			if len(matches) > 0 {
				leftIdxs = append(leftIdxs, lRow)
			}
		case JoinAnti:
			if len(matches) == 0 {
				leftIdxs = append(leftIdxs, lRow)
			}
		case JoinInner:
			for _, rRow := range matches {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, rRow)
			}
		case JoinLeft:
			if len(matches) == 0 {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, -1)
				continue
			}
			for _, rRow := range matches {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, rRow)
			}
		case JoinFull:
			if len(matches) == 0 {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, -1)
				continue
			}
			for _, rRow := range matches {
				leftIdxs = append(leftIdxs, lRow)
				rightIdxs = append(rightIdxs, rRow)
				rightMatched[rRow] = true
			}
		}
	}

	// Full outer: append right rows that never matched, with left-null
	// placeholders. Right rows with a null key are never in rightIndex
	// (buildKeyIndex skips them) so they're picked up here as unmatched.
	if kind == JoinFull {
		for rRow := range right.NumRows() {
			if rightMatched[rRow] {
				continue
			}
			leftIdxs = append(leftIdxs, -1)
			rightIdxs = append(rightIdxs, rRow)
		}
	}

	if kind == JoinSemi || kind == JoinAnti {
		return f.buildLeftOnlyOutput(leftIdxs)
	}
	return f.buildTwoSidedOutput(right, leftKey, rightKey, rKey, leftIdxs, rightIdxs)
}

// joinHashLeft is the mirror of joinHashRight for JoinRight: it builds a
// hash of the LEFT frame's key column and walks the right frame. The
// output column order still puts left columns first — this is a right
// outer join in the SQL sense, not a "swap sides" alias.
func (f *Frame) joinHashLeft(right *Frame, leftKey, rightKey string,
	lKey, rKey Series,
) (*Frame, error) {
	leftIndex, err := buildKeyIndex(lKey, f.NumRows())
	if err != nil {
		return nil, err
	}

	var leftIdxs, rightIdxs []int
	for rRow := range right.NumRows() {
		k, err := keyOf(rKey, rRow)
		if err != nil {
			return nil, err
		}
		var matches []int
		if k[0] != 0x00 {
			matches = leftIndex[string(k)]
		}
		if len(matches) == 0 {
			leftIdxs = append(leftIdxs, -1)
			rightIdxs = append(rightIdxs, rRow)
			continue
		}
		for _, lRow := range matches {
			leftIdxs = append(leftIdxs, lRow)
			rightIdxs = append(rightIdxs, rRow)
		}
	}

	return f.buildTwoSidedOutput(right, leftKey, rightKey, rKey, leftIdxs, rightIdxs)
}

// buildKeyIndex returns a map of hashed-key → row indices for keyCol.
// Null-keyed rows are skipped: they never match anything.
func buildKeyIndex(keyCol Series, n int) (map[string][]int, error) {
	idx := make(map[string][]int, n)
	for row := range n {
		k, err := keyOf(keyCol, row)
		if err != nil {
			return nil, err
		}
		if k[0] == 0x00 {
			continue
		}
		idx[string(k)] = append(idx[string(k)], row)
	}
	return idx, nil
}

// buildLeftOnlyOutput materializes just the left frame's columns at the
// given indexes. Used by JoinSemi and JoinAnti, which never emit
// right-side columns. leftIdxs must not contain -1.
func (f *Frame) buildLeftOnlyOutput(leftIdxs []int) (*Frame, error) {
	pool := memory.DefaultAllocator
	outFields := make([]arrow.Field, 0, len(f.series))
	outColumns := make([]arrow.Column, 0, len(f.series))

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
	schema := arrow.NewSchema(outFields, nil)
	return NewFrame(schema, outColumns)
}

// buildTwoSidedOutput materializes left + right (minus the right join
// key) at the paired indexes. Either side's indices may contain -1 to
// mean "emit a null in this row" — needed for outer joins.
//
// The join key column (leftKey on the left frame) is coalesced: for
// rows where the left index is -1 but the right has a match, we pull
// the key value from rKey instead of emitting null. Matches pandas
// merge / SQL COALESCE(left.key, right.key) — a right or full outer
// join otherwise loses the key value for unmatched-right rows because
// the right-side key column is filtered out of the output.
func (f *Frame) buildTwoSidedOutput(right *Frame, leftKey, rightKey string,
	rKey Series, leftIdxs, rightIdxs []int,
) (*Frame, error) {
	leftNames := f.ColumnNames()
	leftNameSet := make(map[string]struct{}, len(leftNames))
	for _, n := range leftNames {
		leftNameSet[n] = struct{}{}
	}

	pool := memory.DefaultAllocator
	outFields := make([]arrow.Field, 0, len(f.series)+len(right.series))
	outColumns := make([]arrow.Column, 0, cap(outFields))

	for _, s := range f.series {
		var (
			arr arrow.Array
			err error
		)
		if s.name == leftKey {
			arr, err = takeCoalescedKey(pool, s, rKey, leftIdxs, rightIdxs)
		} else {
			arr, err = takeArrayWithNulls(pool, s, leftIdxs)
		}
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, s.field)
		outColumns = append(outColumns, *arrow.NewColumn(s.field, chunked))
	}

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

// takeCoalescedKey materializes the join key column, pulling from
// primary at primaryIdxs and falling back to fallback at fallbackIdxs
// whenever the primary index is -1. Only the join key columns need
// this: for right / full outer joins, unmatched-right rows have -1
// on the left side but a valid right index, and the user still wants
// the key value visible in the output.
//
// primary and fallback must share the same arrow type ID (checked
// higher up in Join). Both index slices must be the same length as
// each other and describe the output row order.
func takeCoalescedKey(pool memory.Allocator, primary, fallback Series,
	primaryIdxs, fallbackIdxs []int,
) (arrow.Array, error) {
	dt := primary.DataType()
	appendFrom := func(b array.Builder, i int) error {
		pi := primaryIdxs[i]
		if pi >= 0 {
			return appendPrimitiveAt(primary, pi, b)
		}
		fi := fallbackIdxs[i]
		if fi >= 0 {
			return appendPrimitiveAt(fallback, fi, b)
		}
		b.AppendNull()
		return nil
	}
	switch dt.ID() {
	case arrow.INT64:
		b := array.NewInt64Builder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.INT32:
		b := array.NewInt32Builder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.UINT64:
		b := array.NewUint64Builder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.UINT32:
		b := array.NewUint32Builder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.FLOAT64:
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.BOOL:
		b := array.NewBooleanBuilder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.STRING:
		b := array.NewStringBuilder(pool)
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	case arrow.TIMESTAMP:
		b := array.NewTimestampBuilder(pool, dt.(*arrow.TimestampType))
		defer b.Release()
		for i := range primaryIdxs {
			if err := appendFrom(b, i); err != nil {
				return nil, err
			}
		}
		return b.NewArray(), nil
	default:
		return nil, fmt.Errorf("%w: coalesced join key not implemented for %s",
			ErrColumnTypeMismatch, dt)
	}
}

// takeArrayWithNulls is like takeArray but permits -1 in indexes to mean
// "emit null" — used to support left / right / full joins where one
// side may have no match.
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

package gobi

import (
	"fmt"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// -----------------------------------------------------------------------------
// Set aggregators — collect distinct non-null values per group into a
// List<T> column. Output is sorted per group for a stable, equality-
// friendly representation. Nulls are skipped.
//
// Polars parity: `.list.unique()` / `.agg(pl.col("x").unique())`.
// Spark parity: `collect_set(x)`.
//
// One concrete per input type (String / Int64 / Uint64 / Int32 / Uint32).
// Users pass via `Aggregation{Column: "x", Fn: gobi.NewStringSetAggregator()}`.
// Adding a type: implement the `extract` + `less` closures and add a
// `NewFooSetAggregator` constructor. `appendCustomListValue` in
// [groupby.go] already dispatches every typed slice this file emits.
// -----------------------------------------------------------------------------

// setAggregator is the shared implementation. T is the arrow element
// type's Go representation (string, int64, uint64, ...). Aggregator's
// non-generic interface means we can't hand back `*setAggregator[T]`
// directly from a purely-generic constructor and still have Merge do
// the peer type assertion — so the exported surface is one concrete
// constructor per supported T.
type setAggregator[T comparable] struct {
	seen     map[T]struct{}
	elemType arrow.DataType
	// extract reads the value at chunk[i], returning (value, notNull, error).
	// Nil chunks or type-mismatches surface via error; nulls via ok=false.
	extract func(chunk arrow.Array, i int) (T, bool, error)
	less    func(a, b T) bool
	name    string
}

func (a *setAggregator[T]) Aggregate(s Series, rows []int) (any, error) {
	// Reset per group — eager engine reuses one instance across groups.
	a.seen = make(map[T]struct{}, len(rows))
	chunks := s.col.Data().Chunks()
	for _, r := range rows {
		chunk, local, ok := locateRowInChunks(chunks, r)
		if !ok {
			return nil, fmt.Errorf("%s: row %d out of range", a.name, r)
		}
		v, notNull, err := a.extract(chunk, local)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.name, err)
		}
		if !notNull {
			continue
		}
		a.seen[v] = struct{}{}
	}
	return a.snapshot(), nil
}

func (a *setAggregator[T]) Merge(other Aggregator) error {
	o, ok := other.(*setAggregator[T])
	if !ok {
		return fmt.Errorf("%s.Merge: peer is %T", a.name, other)
	}
	if a.seen == nil {
		a.seen = make(map[T]struct{}, len(o.seen))
	}
	for k := range o.seen {
		a.seen[k] = struct{}{}
	}
	return nil
}

func (a *setAggregator[T]) Type() arrow.DataType { return arrow.ListOf(a.elemType) }
func (a *setAggregator[T]) Name() string         { return a.name }

// snapshot renders the current set as a sorted []T. Sorted so
// downstream consumers see a stable, equality-friendly representation.
func (a *setAggregator[T]) snapshot() []T {
	out := make([]T, 0, len(a.seen))
	for k := range a.seen {
		out = append(out, k)
	}
	if a.less != nil {
		sort.Slice(out, func(i, j int) bool { return a.less(out[i], out[j]) })
	}
	return out
}

// locateRowInChunks resolves a frame-global row index to
// (chunk, local-index) by walking chunk offsets. Returns ok=false when
// row is beyond the total length.
func locateRowInChunks(chunks []arrow.Array, row int) (arrow.Array, int, bool) {
	offset := 0
	for _, c := range chunks {
		if row < offset+c.Len() {
			return c, row - offset, true
		}
		offset += c.Len()
	}
	return nil, 0, false
}

// -----------------------------------------------------------------------------
// Per-type constructors
// -----------------------------------------------------------------------------

// NewStringSetAggregator returns an Aggregator that collects distinct
// non-null string values per group into a `List<String>` column.
// Input column must be `arrow.STRING`.
func NewStringSetAggregator() Aggregator {
	return &setAggregator[string]{
		elemType: arrow.BinaryTypes.String,
		name:     "string_set",
		extract: func(c arrow.Array, i int) (string, bool, error) {
			sa, ok := c.(*array.String)
			if !ok {
				return "", false, fmt.Errorf("expected *array.String, got %T", c)
			}
			if sa.IsNull(i) {
				return "", false, nil
			}
			return sa.Value(i), true, nil
		},
		less: func(a, b string) bool { return a < b },
	}
}

// NewInt64SetAggregator returns an Aggregator that collects distinct
// non-null int64 values per group into a `List<Int64>` column.
// Input column must be `arrow.INT64`.
func NewInt64SetAggregator() Aggregator {
	return &setAggregator[int64]{
		elemType: arrow.PrimitiveTypes.Int64,
		name:     "int64_set",
		extract: func(c arrow.Array, i int) (int64, bool, error) {
			ia, ok := c.(*array.Int64)
			if !ok {
				return 0, false, fmt.Errorf("expected *array.Int64, got %T", c)
			}
			if ia.IsNull(i) {
				return 0, false, nil
			}
			return ia.Value(i), true, nil
		},
		less: func(a, b int64) bool { return a < b },
	}
}

// NewInt32SetAggregator returns an Aggregator that collects distinct
// non-null int32 values per group into a `List<Int32>` column.
// Input column must be `arrow.INT32`.
func NewInt32SetAggregator() Aggregator {
	return &setAggregator[int32]{
		elemType: arrow.PrimitiveTypes.Int32,
		name:     "int32_set",
		extract: func(c arrow.Array, i int) (int32, bool, error) {
			ia, ok := c.(*array.Int32)
			if !ok {
				return 0, false, fmt.Errorf("expected *array.Int32, got %T", c)
			}
			if ia.IsNull(i) {
				return 0, false, nil
			}
			return ia.Value(i), true, nil
		},
		less: func(a, b int32) bool { return a < b },
	}
}

// NewUint64SetAggregator returns an Aggregator that collects distinct
// non-null uint64 values per group into a `List<Uint64>` column.
// Input column must be `arrow.UINT64`. Common h3 cell-id shape.
func NewUint64SetAggregator() Aggregator {
	return &setAggregator[uint64]{
		elemType: arrow.PrimitiveTypes.Uint64,
		name:     "uint64_set",
		extract: func(c arrow.Array, i int) (uint64, bool, error) {
			ia, ok := c.(*array.Uint64)
			if !ok {
				return 0, false, fmt.Errorf("expected *array.Uint64, got %T", c)
			}
			if ia.IsNull(i) {
				return 0, false, nil
			}
			return ia.Value(i), true, nil
		},
		less: func(a, b uint64) bool { return a < b },
	}
}

// NewUint32SetAggregator returns an Aggregator that collects distinct
// non-null uint32 values per group into a `List<Uint32>` column.
// Input column must be `arrow.UINT32`.
func NewUint32SetAggregator() Aggregator {
	return &setAggregator[uint32]{
		elemType: arrow.PrimitiveTypes.Uint32,
		name:     "uint32_set",
		extract: func(c arrow.Array, i int) (uint32, bool, error) {
			ia, ok := c.(*array.Uint32)
			if !ok {
				return 0, false, fmt.Errorf("expected *array.Uint32, got %T", c)
			}
			if ia.IsNull(i) {
				return 0, false, nil
			}
			return ia.Value(i), true, nil
		},
		less: func(a, b uint32) bool { return a < b },
	}
}

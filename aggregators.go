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
	if err := a.Update(s, rows); err != nil {
		return nil, err
	}
	return a.snapshot(), nil
}

// Update adds col[rows] to the receiver's distinct-value set.
// Implements IncrementalAggregator — enables the streaming executor
// to route this aggregator without buffering all input into one Frame.
func (a *setAggregator[T]) Update(col Series, rows []int) error {
	if a.seen == nil {
		a.seen = make(map[T]struct{}, len(rows))
	}
	chunks := col.col.Data().Chunks()
	for _, r := range rows {
		chunk, local, ok := locateRowInChunks(chunks, r)
		if !ok {
			return fmt.Errorf("%s: row %d out of range", a.name, r)
		}
		v, notNull, err := a.extract(chunk, local)
		if err != nil {
			return fmt.Errorf("%s: %w", a.name, err)
		}
		if !notNull {
			continue
		}
		a.seen[v] = struct{}{}
	}
	return nil
}

// Finalize returns the group's collected set as a sorted []T. Safe to
// call repeatedly; state isn't cleared here (Clone provides fresh
// state for the next group).
func (a *setAggregator[T]) Finalize() any {
	return a.snapshot()
}

// Clone returns a fresh setAggregator[T] with empty per-group state
// and the same type-dispatch closures as the receiver. Used by the
// streaming executor to give each group its own instance.
func (a *setAggregator[T]) Clone() IncrementalAggregator {
	return &setAggregator[T]{
		elemType: a.elemType,
		name:     a.name,
		extract:  a.extract,
		less:     a.less,
	}
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

package gobi

import (
	"bytes"
	"cmp"
	"fmt"
	"sort"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// SortKey names a column to sort by and its direction. Compose multiple
// SortKeys in a single SortBy call for lexicographic (a-then-b-then-c)
// ordering — earlier keys have priority; later keys break ties.
//
// Nulls sort last regardless of Descending, matching pandas / polars
// default behavior. NaN floats also sort last (numpy semantics).
type SortKey struct {
	Column     string
	Descending bool
}

// SortBy returns a new Frame with rows arranged according to keys. The
// sort is stable: rows that compare equal on every key retain their
// input order.
//
// Key columns can be any integer, float (Float16/32/64), Decimal128/256,
// Bool, String / LargeString / StringView, Binary / LargeBinary /
// BinaryView / FixedSizeBinary (byte order), Timestamp, Date32/64,
// Time32/64, Duration or Null column, or a dictionary of any of those
// (sorted by the dictionary values, not the indices). Strings compare
// by byte order. A multi-chunk key column is concatenated into one
// array for the sort. Nulls sort last.
//
// Example:
//
//	// Chronologically by date; break ties by value descending.
//	out, err := df.SortBy(
//	    gobi.SortKey{Column: "date"},
//	    gobi.SortKey{Column: "value", Descending: true},
//	)
func (f *Frame) SortBy(keys ...SortKey) (*Frame, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("gobi: SortBy requires at least one key")
	}
	cmps := make([]rowComparator, len(keys))
	for i, k := range keys {
		s, err := f.Column(k.Column)
		if err != nil {
			return nil, err
		}
		cmp, release, err := newRowComparator(s, k.Descending)
		if err != nil {
			return nil, fmt.Errorf("gobi: SortBy key %q: %w", k.Column, err)
		}
		defer release()
		cmps[i] = cmp
	}

	n := f.NumRows()
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(a, b int) bool {
		ra, rb := perm[a], perm[b]
		for _, cmp := range cmps {
			c := cmp(ra, rb)
			if c != 0 {
				return c < 0
			}
		}
		return false
	})

	return f.take(perm)
}

// rowComparator returns -1 / 0 / +1 comparing rows i and j on a single
// key column. Null-last and direction (ascending/descending) are baked
// into the comparator at construction time so the hot loop doesn't
// branch on either.
type rowComparator func(i, j int) int

// newRowComparator returns the comparator for key column s. A
// multi-chunk key is concatenated into one array first (the key column
// only, not the frame); release frees that copy and must be called once
// the comparator is no longer used.
func newRowComparator(s Series, descending bool) (cmp rowComparator, release func(), err error) {
	release = func() {}
	chunks := s.col.Data().Chunks()
	var arr arrow.Array
	switch len(chunks) {
	case 0:
		arr = array.MakeArrayOfNull(memory.DefaultAllocator, s.DataType(), 0)
		release = arr.Release
	case 1:
		arr = chunks[0]
	default:
		cat, err := array.Concatenate(chunks, memory.DefaultAllocator)
		if err != nil {
			return nil, release, fmt.Errorf("concatenate %d-chunk sort key: %w", len(chunks), err)
		}
		arr, release = cat, cat.Release
	}
	cmp, err = arrayComparator(arr, descending)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return cmp, release, nil
}

// arrayComparator dispatches on arr's concrete type. Fixed-width
// numeric types compare their raw value slices; the rest go through
// the type's Value accessor.
func arrayComparator(arr arrow.Array, descending bool) (rowComparator, error) {
	switch a := arr.(type) {
	case *array.Int64:
		return ordered(a, a.Int64Values(), descending), nil
	case *array.Int32:
		return ordered(a, a.Int32Values(), descending), nil
	case *array.Int16:
		return ordered(a, a.Int16Values(), descending), nil
	case *array.Int8:
		return ordered(a, a.Int8Values(), descending), nil
	case *array.Uint64:
		return ordered(a, a.Uint64Values(), descending), nil
	case *array.Uint32:
		return ordered(a, a.Uint32Values(), descending), nil
	case *array.Uint16:
		return ordered(a, a.Uint16Values(), descending), nil
	case *array.Uint8:
		return ordered(a, a.Uint8Values(), descending), nil
	case *array.Timestamp:
		return ordered(a, a.TimestampValues(), descending), nil
	case *array.Date32:
		return ordered(a, a.Date32Values(), descending), nil
	case *array.Date64:
		return ordered(a, a.Date64Values(), descending), nil
	case *array.Time32:
		return ordered(a, a.Time32Values(), descending), nil
	case *array.Time64:
		return ordered(a, a.Time64Values(), descending), nil
	case *array.Duration:
		return ordered(a, a.DurationValues(), descending), nil
	case *array.Float64:
		return floats(a, a.Float64Values(), descending), nil
	case *array.Float32:
		return floats(a, a.Float32Values(), descending), nil
	case *array.Float16:
		vals := a.Values()
		return floatsBy(a, func(i int) float64 { return float64(vals[i].Float32()) }, descending), nil
	case *array.String:
		return byIndex(a, func(i, j int) int { return strings.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.LargeString:
		return byIndex(a, func(i, j int) int { return strings.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.StringView:
		return byIndex(a, func(i, j int) int { return strings.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.Binary:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.LargeBinary:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.BinaryView:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.FixedSizeBinary:
		return byIndex(a, func(i, j int) int { return bytes.Compare(a.Value(i), a.Value(j)) }, descending), nil
	case *array.Boolean:
		return byIndex(a, func(i, j int) int { return cmpBool(a.Value(i), a.Value(j)) }, descending), nil
	case *array.Decimal128:
		return byIndex(a, func(i, j int) int { return a.Value(i).Cmp(a.Value(j)) }, descending), nil
	case *array.Decimal256:
		return byIndex(a, func(i, j int) int { return a.Value(i).Cmp(a.Value(j)) }, descending), nil
	case *array.Dictionary:
		// Compare the dictionary entries the indices point at. The inner
		// comparator applies direction and puts null entries last.
		inner, err := arrayComparator(a.Dictionary(), descending)
		if err != nil {
			return nil, err
		}
		return func(i, j int) int {
			ni, nj := a.IsNull(i), a.IsNull(j)
			if ni || nj {
				return nullAwareCompare(ni, nj, 0, descending)
			}
			return inner(a.GetValueIndex(i), a.GetValueIndex(j))
		}, nil
	case *array.Null:
		return func(i, j int) int { return 0 }, nil
	}
	return nil, fmt.Errorf("unsupported sort key type %s", arr.DataType())
}

// ordered compares a fixed-width column through its value slice. Kept
// generic over the element type rather than going through byIndex so
// the hot numeric case is one closure call per comparison.
func ordered[T cmp.Ordered](a arrow.Array, vals []T, descending bool) rowComparator {
	sign := direction(descending)
	if a.NullN() == 0 {
		return func(i, j int) int { return sign * cmp.Compare(vals[i], vals[j]) }
	}
	return func(i, j int) int {
		ni, nj := a.IsNull(i), a.IsNull(j)
		if ni || nj {
			return nullAwareCompare(ni, nj, 0, descending)
		}
		return sign * cmp.Compare(vals[i], vals[j])
	}
}

// floats compares a float column with NaN treated like null: last in
// either direction.
func floats[T float32 | float64](a arrow.Array, vals []T, descending bool) rowComparator {
	return floatsBy(a, func(i int) float64 { return float64(vals[i]) }, descending)
}

func floatsBy(a arrow.Array, at func(int) float64, descending bool) rowComparator {
	sign := direction(descending)
	return func(i, j int) int {
		ni, nj := a.IsNull(i), a.IsNull(j)
		if ni || nj {
			return nullAwareCompare(ni, nj, 0, descending)
		}
		x, y := at(i), at(j)
		xNaN, yNaN := isNaN(x), isNaN(y)
		if xNaN || yNaN {
			return nullAwareCompare(xNaN, yNaN, 0, descending)
		}
		return sign * cmp.Compare(x, y)
	}
}

// byIndex wraps a value comparator over non-null rows with the null-last
// policy and direction.
func byIndex(a arrow.Array, cmp func(i, j int) int, descending bool) rowComparator {
	sign := direction(descending)
	if a.NullN() == 0 {
		return func(i, j int) int { return sign * cmp(i, j) }
	}
	return func(i, j int) int {
		ni, nj := a.IsNull(i), a.IsNull(j)
		if ni || nj {
			return nullAwareCompare(ni, nj, 0, descending)
		}
		return sign * cmp(i, j)
	}
}

func direction(descending bool) int {
	if descending {
		return -1
	}
	return 1
}

// nullAwareCompare composes a null-last policy with a value comparator
// and an optional direction flip. Nulls always sort last regardless of
// direction — the flip only applies to value-vs-value comparisons.
//
// Semantics:
//
//	both null      → 0
//	left null      → +1  (null sorts after non-null)
//	right null     → -1
//	neither null   → cmpVal, negated if descending
func nullAwareCompare(ni, nj bool, cmpVal int, descending bool) int {
	switch {
	case ni && nj:
		return 0
	case ni:
		return +1
	case nj:
		return -1
	}
	if descending {
		return -cmpVal
	}
	return cmpVal
}

func isNaN(f float64) bool { return f != f }

func cmpBool(a, b bool) int {
	switch {
	case !a && b:
		return -1
	case a && !b:
		return +1
	}
	return 0
}

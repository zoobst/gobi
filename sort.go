package gobi

import (
	"fmt"
	"sort"

	"github.com/apache/arrow-go/v18/arrow/array"
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
// Supported key column types: String, Bool, Int32, Int64, Uint32,
// Uint64, Float64, Float32, Timestamp. Nulls sort last.
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
		cmp, err := newRowComparator(s, k.Descending)
		if err != nil {
			return nil, fmt.Errorf("gobi: SortBy key %q: %w", k.Column, err)
		}
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

// newRowComparator dispatches on the Series' arrow type and returns a
// comparator that indexes directly into the underlying typed array.
func newRowComparator(s Series, descending bool) (rowComparator, error) {
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, fmt.Errorf("multi-chunk sort keys not yet supported")
	}
	switch a := chunks[0].(type) {
	case *array.Int64:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpOrd(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.Int32:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpOrd(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.Uint64:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpOrd(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.Uint32:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpOrd(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.Float64:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpFloat(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.Float32:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpFloat(float64(a.Value(i)), float64(a.Value(j))), descending)
		}, nil
	case *array.Boolean:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpBool(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.String:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpString(a.Value(i), a.Value(j)), descending)
		}, nil
	case *array.Timestamp:
		return func(i, j int) int {
			return nullAwareCompare(a.IsNull(i), a.IsNull(j),
				cmpOrd(int64(a.Value(i)), int64(a.Value(j))), descending)
		}, nil
	}
	return nil, fmt.Errorf("unsupported sort key type %s", s.DataType())
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

// cmpOrd is the generic three-way comparator for any ordered numeric
// type. Signed and unsigned integers cover the group-by key set.
func cmpOrd[T int64 | int32 | uint64 | uint32](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return +1
	}
	return 0
}

// cmpFloat treats NaN as null-equivalent — NaN sorts after every
// non-NaN value, matching numpy / pandas behavior.
func cmpFloat(a, b float64) int {
	aNaN, bNaN := isNaN(a), isNaN(b)
	switch {
	case aNaN && bNaN:
		return 0
	case aNaN:
		return +1
	case bNaN:
		return -1
	case a < b:
		return -1
	case a > b:
		return +1
	}
	return 0
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

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return +1
	}
	return 0
}

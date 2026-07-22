package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Unique returns a Frame with duplicate rows removed. Uniqueness is
// determined by the columns listed in cols; other columns come along
// for the ride from the first occurrence of each distinct key.
//
// When cols is empty, distinctness is computed over every column
// (SQL `DISTINCT *`). Any listed column must be a hashable arrow
// type (String, LargeString, Int64, Int32, Uint64, Uint32, Float64,
// Bool, Timestamp) — the same set GroupBy accepts.
//
// Semantics match pandas' `df.drop_duplicates(subset=cols)` and
// polars' `df.unique(subset=cols)`: the result preserves the full
// input schema and first-occurrence order. If you only want the
// distinct values of a single column, use `df.Column(name).Unique()`
// which returns a Series.
//
// This is the whole-frame `distinct` primitive. For per-group
// distinct counts, use `GroupBy(...).Agg(Aggregation{Kind: AggNUnique})`.
func (f *Frame) Unique(cols ...string) (*Frame, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: Frame.Unique on nil frame")
	}
	if len(cols) == 0 {
		cols = f.ColumnNames()
	}
	keys := make([]Series, len(cols))
	for i, name := range cols {
		s, err := f.Column(name)
		if err != nil {
			return nil, err
		}
		if !isHashable(s.DataType()) {
			return nil, fmt.Errorf("gobi: Frame.Unique: column %q has non-hashable type %s",
				name, s.DataType())
		}
		keys[i] = s
	}

	rows := f.NumRows()
	// Reusable scratch: one composite-key encode per row.
	var scratch []byte
	seen := make(map[string]struct{})
	keep := make([]int, 0, rows) // first-occurrence indices, preserves order

	for row := 0; row < rows; row++ {
		buf, err := composeCompositeKeyInto(scratch[:0], keys, row)
		if err != nil {
			return nil, err
		}
		scratch = buf
		if _, ok := seen[string(buf)]; ok {
			continue
		}
		seen[string(buf)] = struct{}{}
		keep = append(keep, row)
	}
	return f.take(keep)
}

// Unique returns a Series of distinct non-null values in first-
// occurrence order. The result's arrow type matches the input's.
//
// Nulls are dropped (matches pandas / polars `unique` semantics).
// Callers that want a "did I see any null?" signal should combine
// with `Series.NullCount()`.
func (s Series) Unique() (Series, error) {
	if s.col == nil {
		return Series{}, fmt.Errorf("gobi: Series.Unique on empty series")
	}
	if !isHashable(s.DataType()) {
		return Series{}, fmt.Errorf("gobi: Series.Unique: type %s is not hashable", s.DataType())
	}
	n := s.Len()
	var scratch []byte
	seen := make(map[string]struct{})
	keep := make([]int, 0, n)
	for row := 0; row < n; row++ {
		null, err := isNullAtSeries(s, row)
		if err != nil {
			return Series{}, err
		}
		if null {
			continue
		}
		buf, err := keyOfAppend(scratch[:0], s, row)
		if err != nil {
			return Series{}, err
		}
		scratch = buf
		if _, ok := seen[string(buf)]; ok {
			continue
		}
		seen[string(buf)] = struct{}{}
		keep = append(keep, row)
	}

	pool := memory.DefaultAllocator
	arr, err := takeArray(pool, s, keep)
	if err != nil {
		return Series{}, err
	}
	// Wrap the new array into a chunked column with the original
	// field so downstream schema-sensitive callers see the same
	// metadata (name, type, nullability, geometry tag).
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	arr.Release()
	col := arrow.NewColumn(s.field, chunked)
	return NewSeries(col), nil
}

// ValueCounts returns a two-column Frame of (value, count) pairs
// counting the occurrences of each distinct value of col, sorted
// descending by count. Ties are broken by the value itself
// ascending — makes the output stable / deterministic even when
// several distinct values share a frequency.
//
// The output columns are named `col` (matching the input) and
// `count` (Int64). Null values in the source column are counted
// as their own group under the null value slot — the count row for
// null contains a null in the value column and its count in the
// count column.
//
// Equivalent to pandas' `Series.value_counts()` / polars'
// `Series.value_counts()`. For per-group value counts (e.g. count
// of role per department), you want a nested output that gobi
// doesn't have a schema for yet — build it via two-level GroupBy
// today or wait for list-typed columns.
func (f *Frame) ValueCounts(col string) (*Frame, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: Frame.ValueCounts on nil frame")
	}
	if _, err := f.Column(col); err != nil {
		return nil, err
	}
	gb, err := f.GroupBy(col)
	if err != nil {
		return nil, err
	}
	// Alias the count column to "count" (rather than the default
	// "<col>_count") to match the pandas / polars convention.
	counted, err := gb.Agg(Aggregation{Kind: AggCount, Alias: "count"})
	if err != nil {
		return nil, err
	}
	// Desc by count; break ties by value ascending so output is
	// deterministic across runs. For non-hashable + non-sortable
	// value columns this SortBy will surface an error, which is
	// fine — you shouldn't be counting them anyway.
	return counted.SortBy(
		SortKey{Column: "count", Descending: true},
		SortKey{Column: col, Descending: false},
	)
}

// NUnique returns the count of distinct non-null values in s.
//
// Equivalent to `s.Unique().Len()` but skips the arrow array
// construction. Uses the same byte-encoding as GroupBy so numeric
// bit-equal values collapse identically. O(n) time, O(distinct)
// memory.
func (s Series) NUnique() (int64, error) {
	if s.col == nil {
		return 0, nil
	}
	if !isHashable(s.DataType()) {
		return 0, fmt.Errorf("gobi: Series.NUnique: type %s is not hashable", s.DataType())
	}
	n := s.Len()
	var scratch []byte
	seen := make(map[string]struct{})
	for row := 0; row < n; row++ {
		null, err := isNullAtSeries(s, row)
		if err != nil {
			return 0, err
		}
		if null {
			continue
		}
		buf, err := keyOfAppend(scratch[:0], s, row)
		if err != nil {
			return 0, err
		}
		scratch = buf
		if _, ok := seen[string(buf)]; !ok {
			seen[string(buf)] = struct{}{}
		}
	}
	return int64(len(seen)), nil
}

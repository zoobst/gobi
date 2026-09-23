package gobi

import (
	"fmt"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// chunkCursor maps a global row index to (chunk, local index) for one
// column. Build it once per column and reuse it across a row loop.
//
// readScalarAt / isNullAtSeries walk the chunk list from the start on
// every call, which is O(chunks) per cell. That's free on the
// single-chunk columns most operators produce, but parquet files
// written by Spark / Trino carry one chunk per row group (often
// hundreds), and Concat output is multi-chunk by construction, so a
// row-by-row read costs rows × fields × chunks. The cursor holds the
// cumulative chunk offsets, checks the last chunk it hit first (a
// sequential scan stays inside one chunk for long runs, so this is
// O(1) amortized), and falls back to a binary search on a miss.
//
// Not safe for concurrent use: the last-hit hint is mutable state.
type chunkCursor struct {
	chunks []arrow.Array
	starts []int // starts[i] = global row index of chunks[i]'s first row
	total  int
	last   int // index into chunks of the most recent hit
}

// newChunkCursor builds a cursor over s. A nil column yields an empty
// cursor: nothing to read on a zero-row frame, and any read reports
// out of range rather than failing construction.
func newChunkCursor(s Series) *chunkCursor {
	if s.col == nil {
		return &chunkCursor{}
	}
	chunks := s.col.Data().Chunks()
	starts := make([]int, len(chunks))
	total := 0
	for i, c := range chunks {
		starts[i] = total
		total += c.Len()
	}
	return &chunkCursor{chunks: chunks, starts: starts, total: total}
}

// locate returns the chunk holding global row `row` and the row's
// index within it.
func (c *chunkCursor) locate(row int) (arrow.Array, int, error) {
	if row < 0 || row >= c.total {
		return nil, 0, fmt.Errorf("row %d out of range [0,%d)", row, c.total)
	}
	if i := c.last; i < len(c.chunks) {
		if local := row - c.starts[i]; local >= 0 && local < c.chunks[i].Len() {
			return c.chunks[i], local, nil
		}
	}
	// Largest i with starts[i] <= row. Zero-length chunks share a start
	// with their successor; the "largest" rule skips past them.
	i := sort.Search(len(c.starts), func(i int) bool { return c.starts[i] > row }) - 1
	c.last = i
	return c.chunks[i], row - c.starts[i], nil
}

// scalarAt is readScalarAt without the per-call chunk walk.
func (c *chunkCursor) scalarAt(row int) (any, error) {
	chunk, local, err := c.locate(row)
	if err != nil {
		return nil, err
	}
	return readArrayScalar(chunk, local)
}

// isNull is isNullAtSeries without the per-call chunk walk. Also
// reports a dictionary cell as null when its index is valid but the
// dictionary entry it points at is null.
func (c *chunkCursor) isNull(row int) (bool, error) {
	chunk, local, err := c.locate(row)
	if err != nil {
		return false, err
	}
	return isNullArr(chunk, local), nil
}

// readArrayScalar reads element i of arr as a Go value. Nulls return
// (nil, nil). Dictionary-encoded arrays (what Spark and Trino write
// for low-cardinality string columns) resolve through the dictionary,
// so callers see the same value type a plain column would give them.
func readArrayScalar(arr arrow.Array, i int) (any, error) {
	arr, i = resolveDictionary(arr, i)
	if arr.IsNull(i) {
		return nil, nil
	}
	switch a := arr.(type) {
	case *array.Int64:
		return a.Value(i), nil
	case *array.Int32:
		return a.Value(i), nil
	case *array.Uint64:
		return a.Value(i), nil
	case *array.Uint32:
		return a.Value(i), nil
	case *array.Float64:
		return a.Value(i), nil
	case *array.Float32:
		return a.Value(i), nil
	case *array.Boolean:
		return a.Value(i), nil
	case *array.String:
		return a.Value(i), nil
	case *array.LargeString:
		return a.Value(i), nil
	case *array.Timestamp:
		return a.Value(i), nil
	}
	return nil, fmt.Errorf("readScalarAt: unsupported type %T", arr)
}

// isNullArr reports whether element i of arr is null, resolving
// dictionary indices so a valid index pointing at a null dictionary
// entry counts as null.
func isNullArr(arr arrow.Array, i int) bool {
	arr, i = resolveDictionary(arr, i)
	return arr.IsNull(i)
}

// resolveDictionary maps element i of a dictionary-encoded array to
// (values array, value index); non-dictionary arrays pass through.
// A null index returns the index array's position unchanged, so the
// caller's IsNull check still sees it.
func resolveDictionary(arr arrow.Array, i int) (arrow.Array, int) {
	for {
		d, ok := arr.(*array.Dictionary)
		if !ok || d.IsNull(i) {
			return arr, i
		}
		arr, i = d.Dictionary(), d.GetValueIndex(i)
	}
}

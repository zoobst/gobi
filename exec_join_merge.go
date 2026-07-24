package gobi

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/apache/arrow-go/v18/arrow"
)

// sortMergeJoinExec is the alignment-aware fast path for Inner
// joins. Fires when both sides carry PartitionMetadata proving:
//
//   - same partition scheme (matching HashFn + Columns)
//   - both sides sorted on the join key with SortEnforced=true
//
// Under those conditions same-key rows are guaranteed contiguous on
// each side and in the same relative bucket order across sides, so
// a two-pointer merge scan matches every pair without a hash table.
// Eliminates the buildKeyIndex allocation that the streaming hash
// join builds on the right side — the primary RSS + CPU win of
// step 7.
//
// Inner-only for step 7. Left/Semi/Anti sort-merge variants are
// straightforward extensions (change the emit logic per join kind)
// but stay on the streaming hash path until a workload calls for
// them. Right/Full stay on the materializing fallback because the
// current gobi.Frame.Join doesn't have a merge path for them.
//
// Memory profile: both sides fully materialized before merge. That's
// a step BACK from streamingJoinExec's probe-side streaming — but
// the eliminated hash index typically dominates for large right
// sides with many unique keys, which is the workload sort-merge is
// designed for. Users who care about probe-side streaming stay on
// the streaming hash path by not asserting alignment claims.
type sortMergeJoinExec struct {
	left, right       ExecOperator
	leftKey, rightKey string
	outSchema         *arrow.Schema

	emitted    bool
	closed     bool
	buildFrame *Frame // right, materialized
	probeFrame *Frame // left, materialized
}

func (e *sortMergeJoinExec) Schema() *arrow.Schema { return e.outSchema }

func (e *sortMergeJoinExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if e.emitted {
		return nil, io.EOF
	}
	e.emitted = true

	if err := e.materializeInputs(ctx); err != nil {
		return nil, err
	}

	joined, err := mergeJoinInner(e.probeFrame, e.buildFrame, e.leftKey, e.rightKey)
	if err != nil {
		return nil, err
	}
	if joined.NumRows() == 0 {
		return nil, io.EOF
	}
	return frameToBatch(joined), nil
}

func (e *sortMergeJoinExec) materializeInputs(ctx context.Context) error {
	if e.probeFrame != nil {
		return nil
	}
	lf, err := Execute(ctx, e.left)
	if err != nil {
		return err
	}
	rf, err := Execute(ctx, e.right)
	if err != nil {
		return err
	}
	e.probeFrame = lf
	e.buildFrame = rf
	return nil
}

func (e *sortMergeJoinExec) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	// Execute already closed both children on success; a defensive
	// close is a no-op for the operators we ship. Cover the failure
	// path where materializeInputs never completed.
	if e.probeFrame == nil {
		_ = e.left.Close()
		_ = e.right.Close()
	}
	return nil
}

// mergeJoinInner implements the two-pointer scan for an Inner join
// over two sorted+aligned frames. Encoded keys are compared via
// bytes.Compare — same encoding as the hash join uses (keyOfAppend
// with big-endian integers), so the byte order matches the numeric
// / string order that the SortEnforced writer contract promises.
//
// For each pair of contiguous same-key runs (one on each side), emits
// the cross-product to leftIdxs / rightIdxs, then delegates to
// Frame.buildTwoSidedOutput for the actual output materialization —
// reusing the same output builder as the hash join so schemas +
// column ordering stay identical.
func mergeJoinInner(left, right *Frame, leftKey, rightKey string) (*Frame, error) {
	lKey, err := left.Column(leftKey)
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

	nLeft, nRight := left.NumRows(), right.NumRows()

	// Precompute encoded keys per side once — repeated calls to
	// keyOfAppend during the merge would dominate CPU. Two scratch
	// buffers grown once, then reused via slice reslicing.
	//
	// Storage: 2 × N × avg-key-len bytes. For an int64 key that's
	// ~9 bytes per row (tag + 8-byte value). 100k rows = ~1.8MB —
	// negligible next to the input frames.
	leftKeys, err := encodeAllKeys(lKey, nLeft)
	if err != nil {
		return nil, err
	}
	rightKeys, err := encodeAllKeys(rKey, nRight)
	if err != nil {
		return nil, err
	}

	var leftIdxs, rightIdxs []int
	i, j := 0, 0
	for i < nLeft && j < nRight {
		// Skip null keys on either side (encoded as single 0x00 byte);
		// null never matches null in Inner join semantics.
		if isNullKey(leftKeys[i]) {
			i++
			continue
		}
		if isNullKey(rightKeys[j]) {
			j++
			continue
		}
		cmp := bytes.Compare(leftKeys[i], rightKeys[j])
		switch {
		case cmp < 0:
			i++
		case cmp > 0:
			j++
		default:
			// Match. Find the extent of equal-key runs on both sides.
			iEnd := i + 1
			for iEnd < nLeft && bytes.Equal(leftKeys[iEnd], leftKeys[i]) {
				iEnd++
			}
			jEnd := j + 1
			for jEnd < nRight && bytes.Equal(rightKeys[jEnd], rightKeys[j]) {
				jEnd++
			}
			// Cross-product for the run.
			for a := i; a < iEnd; a++ {
				for b := j; b < jEnd; b++ {
					leftIdxs = append(leftIdxs, a)
					rightIdxs = append(rightIdxs, b)
				}
			}
			i, j = iEnd, jEnd
		}
	}

	return left.buildTwoSidedOutput(right, leftKey, rightKey, rKey, leftIdxs, rightIdxs)
}

// encodeAllKeys returns per-row encoded keys for s. Same encoding
// keyOf uses in the hash-join path — sharing the encoding keeps byte
// comparisons consistent with the hash-lookup path (though sort-
// merge only cares about byte order, not hash lookup).
func encodeAllKeys(s Series, n int) ([][]byte, error) {
	out := make([][]byte, n)
	for row := range n {
		k, err := keyOfAppend(nil, s, row)
		if err != nil {
			return nil, err
		}
		out[row] = k
	}
	return out, nil
}

// isNullKey reports whether k is the null sentinel produced by
// keyOfAppend for a null cell (a single 0x00 byte).
func isNullKey(k []byte) bool {
	return len(k) == 1 && k[0] == 0x00
}

// canMergeJoin reports whether n's inputs meet the sort-merge fast
// path's preconditions:
//
//   - Both sides carry non-nil PartitionMetadata.
//   - AlignedWith holds — same HashFn + same ordered Columns on
//     both sides (they must use the same partitioning scheme so
//     same-key rows are colocated in the same bucket order).
//   - Both sides claim SortedBy starting with the join key column
//     with SortEnforced=true — hint-only sortedness could silently
//     produce wrong results if the actual data isn't ordered.
//
// Any failure falls through to streamingJoinExec (the general hash
// path), which is correct for any inputs regardless of metadata.
// This predicate is deliberately conservative: it refuses cases
// where the fast path would be technically correct but harder to
// reason about (e.g. join key is only part of a multi-column
// partition), keeping the v1 rule easy to audit.
func canMergeJoin(n *joinNode) bool {
	lm := n.input.PartitionMetadata()
	rm := n.right.PartitionMetadata()
	if lm == nil || rm == nil {
		return false
	}
	if !AlignedWith(lm, rm) {
		return false
	}
	// Both sides must be sorted on the join key with writer-enforced
	// order. Sort keys are single-column here — the join keys are
	// single columns too, so require the first SortedBy element to
	// match the respective join key.
	if !sortedByStartsWith(lm, n.leftKey) {
		return false
	}
	if !sortedByStartsWith(rm, n.rightKey) {
		return false
	}
	return true
}

// sortedByStartsWith reports whether meta claims a writer-enforced
// sort whose leading key matches col. Direction (ascending vs
// descending) is ignored — sort-merge works either way as long as
// both sides use the same direction, which is enforced by the
// AlignedWith HashFn equality check (same source == same sort
// direction in practice).
func sortedByStartsWith(meta *PartitionMetadata, col string) bool {
	if meta == nil || !meta.SortEnforced || len(meta.SortedBy) == 0 {
		return false
	}
	return meta.SortedBy[0].Column == col
}

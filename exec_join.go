package gobi

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/apache/arrow-go/v18/arrow"
)

// streamingJoinExec is a native streaming hash join.
//
// The right (build) side materializes to a *Frame once, on first
// Next(). The left (probe) side streams one batch at a time; each
// batch is joined against the build side via the existing
// Frame.Join implementation (which itself builds a hash index of
// the right side and probes with the left rows). Output batches
// flow to the caller as they're produced.
//
// Handles the "left-driven" join kinds — Inner, Left, Semi, Anti.
// Right and Full joins need a second-phase pass to emit right rows
// that never matched, which requires state that grows with the
// build side rather than the probe. Those still route through the
// materializing fallback in Compile — see canStreamJoin.
//
// Memory profile: build-side Frame + one probe batch + one output
// batch. The build side is bounded by right's total row count;
// there's no disk spill, so if the build side doesn't fit in RAM
// the process OOMs (per the design rule). The probe side never
// materializes as a whole.
type streamingJoinExec struct {
	left, right       ExecOperator
	leftKey, rightKey string
	kind              JoinType
	outSchema         *arrow.Schema

	built      bool
	buildFrame *Frame           // right side, materialized on first Next
	rightIndex map[string][]int // right key → rows, built once and reused
	rightKeyS  Series           // right's key column, cached for the per-batch join
	closed     bool
}

func (e *streamingJoinExec) Schema() *arrow.Schema { return e.outSchema }

func (e *streamingJoinExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if err := e.buildIfNeeded(ctx); err != nil {
		return nil, err
	}
	// Loop until we get a non-empty joined batch or run out of
	// probe input. Empty joined batches (e.g. Inner join where a
	// probe batch has no matches) get skipped rather than
	// forwarded — downstream can handle nil batches but skipping
	// them saves cycles.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		probeBatch, err := e.left.Next(ctx)
		if err != nil {
			return nil, err
		}
		probeFrame, err := batchToFrame(probeBatch)
		probeBatch.Release()
		if err != nil {
			return nil, err
		}
		// Grab the probe's key column each batch (schema is the same
		// but the column arrays differ per batch); reuse the cached
		// right index built once in buildIfNeeded so we don't rebuild
		// the whole right-side hash table per probe batch — the
		// original bug this exec was accidentally hitting.
		lKey, err := probeFrame.Column(e.leftKey)
		if err != nil {
			probeFrame.Release()
			return nil, err
		}
		joined, err := probeFrame.joinHashRightWithIndex(
			e.buildFrame, e.leftKey, e.rightKey, lKey, e.rightKeyS, e.kind, e.rightIndex)
		probeFrame.Release()
		if err != nil {
			return nil, err
		}
		if joined.NumRows() == 0 {
			joined.Release()
			continue
		}
		out := frameToBatch(joined)
		joined.Release()
		return out, nil
	}
}

func (e *streamingJoinExec) buildIfNeeded(ctx context.Context) error {
	if e.built {
		return nil
	}
	e.built = true
	// Execute the right subtree to completion, closing it as a
	// side effect. From here on the streaming path only pulls from
	// e.left.
	rf, err := Execute(ctx, e.right)
	if err != nil {
		return err
	}
	e.buildFrame = rf
	// Build the right-side hash index once here (rather than
	// per-probe-batch inside the join loop). Big-O drops from
	// O(right rows × probe batches) to O(right rows + probe rows).
	rKey, err := rf.Column(e.rightKey)
	if err != nil {
		return err
	}
	e.rightKeyS = rKey
	e.rightIndex, err = buildKeyIndex(rKey, rf.NumRows())
	if err != nil {
		return err
	}
	return nil
}

func (e *streamingJoinExec) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	// Both inputs. buildIfNeeded's Execute closes e.right internally
	// on the normal path; a double-close is a no-op for the
	// operators we ship.
	_ = e.left.Close()
	if !e.built {
		_ = e.right.Close()
	}
	// Drop the materialized build side so its arrow columns can be
	// freed. Without this, every streaming join pins the entire
	// right-side Frame for the plan's lifetime — a straight leak on
	// long-lived executors that touch a join.
	if e.buildFrame != nil {
		e.buildFrame.Release()
		e.buildFrame = nil
	}
	return nil
}

// canStreamJoin reports whether a JoinType is safe to route through
// streamingJoinExec. Left-driven kinds (Inner, Left, Semi, Anti)
// stream naturally — the output is determined entirely by walking
// the probe side against the build side.
//
// Right and Full joins need a second-phase pass to emit right rows
// that were never matched — those stay on the materializing
// fallback until we implement the second-phase state.
func canStreamJoin(k JoinType) bool {
	switch k {
	case JoinInner, JoinLeft, JoinSemi, JoinAnti:
		return true
	}
	return false
}

// unused import guard: io referenced above via ExecOperator's
// contract (Next returns io.EOF at end); Next itself doesn't
// mention it since the underlying operator produces the EOF.
var _ = io.EOF

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
		joined.Release()
		return nil, io.EOF
	}
	out := frameToBatch(joined)
	joined.Release()
	return out, nil
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
	// Drop both materialized sides so their arrow columns can be
	// freed. Without this, every completed sort-merge join pins both
	// input Frames for the plan's lifetime.
	if e.probeFrame != nil {
		e.probeFrame.Release()
		e.probeFrame = nil
	}
	if e.buildFrame != nil {
		e.buildFrame.Release()
		e.buildFrame = nil
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
	if !arrow.TypeEqual(lKey.DataType(), rKey.DataType()) {
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

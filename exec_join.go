package gobi

import (
	"context"
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
	buildFrame *Frame // right side, materialized on first Next
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
		joined, err := probeFrame.Join(e.buildFrame, e.leftKey, e.rightKey, e.kind)
		if err != nil {
			return nil, err
		}
		if joined.NumRows() == 0 {
			continue
		}
		return frameToBatch(joined), nil
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

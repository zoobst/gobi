package gobi

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/apache/arrow-go/v18/arrow"
)

// parallelScanFileExec is the multi-worker counterpart to
// scanFileExec. It's given N sub-callbacks (one per worker,
// typically each covering a disjoint range of parquet row-groups)
// and spawns N producer goroutines that fan batches into a shared
// channel. Next pops from the channel.
//
// Batches from different workers arrive in unspecified order —
// blocking operators downstream (Sort, Tail) must handle that. The
// streaming operators we ship (Filter, Project, WithColumn, Drop,
// Limit, Aggregate) are order-independent, so this composes with
// the rest of Layer 6 without extra coordination.
//
// Memory: bounded to `2 * len(subs)` in-flight batches (the channel
// buffer) plus per-worker source state (arrow-go parquet reader,
// Snappy decoder scratch). No disk spill; if the aggregate
// downstream can't keep up and the buffer fills, workers block on
// send — natural backpressure.
type parallelScanFileExec struct {
	schema  *arrow.Schema
	subs    []func(cb func(*Frame) error) error
	batches chan arrow.RecordBatch
	errs    chan error
	done    chan struct{}

	startOnce sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
}

// newParallelScanFileExec builds the operator without starting any
// goroutines — workers spin up lazily on the first Next call. If
// the caller Closes before Next, the callbacks never execute.
func newParallelScanFileExec(schema *arrow.Schema, subs []func(cb func(*Frame) error) error) *parallelScanFileExec {
	if len(subs) == 0 {
		// Producer of the operator should have fallen back to
		// scanFileExec; being defensive here so a bug upstream
		// doesn't panic — we just return io.EOF immediately.
		return &parallelScanFileExec{
			schema:  schema,
			batches: closedBatchChan(),
			errs:    make(chan error, 1),
			done:    make(chan struct{}),
		}
	}
	// Buffer sized generously so producers can keep decoding while
	// the consumer works. 2 per worker matches DuckDB's morsel
	// pipelining rule of thumb — enough to hide per-batch jitter
	// without holding much memory.
	buf := 2 * len(subs)
	return &parallelScanFileExec{
		schema:  schema,
		subs:    subs,
		batches: make(chan arrow.RecordBatch, buf),
		errs:    make(chan error, len(subs)),
		done:    make(chan struct{}),
	}
}

// closedBatchChan returns a batches channel that's already closed,
// so Next returns io.EOF on first call. Used as the "no work"
// escape hatch when newParallelScanFileExec is constructed with an
// empty subs slice.
func closedBatchChan() chan arrow.RecordBatch {
	c := make(chan arrow.RecordBatch)
	close(c)
	return c
}

func (e *parallelScanFileExec) Schema() *arrow.Schema { return e.schema }

func (e *parallelScanFileExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	e.startOnce.Do(e.startWorkers)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case batch, ok := <-e.batches:
		if !ok {
			// All producers finished. Check for a terminal error.
			select {
			case err := <-e.errs:
				return nil, err
			default:
				return nil, io.EOF
			}
		}
		return batch, nil
	}
}

func (e *parallelScanFileExec) startWorkers() {
	for _, sub := range e.subs {
		sub := sub
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			err := sub(func(f *Frame) error {
				// Same lifetime dance as scanFileExec: retain the
				// frame's buffers so they survive the callback
				// return; convert to a batch (which retains the
				// arrays it wraps); release our extra ref so the
				// batch is the sole owner.
				f.Retain()
				batch := frameToBatch(f)
				f.Release()
				select {
				case e.batches <- batch:
					return nil
				case <-e.done:
					batch.Release()
					return errScanClosed
				}
			})
			if err != nil && !errors.Is(err, errScanClosed) {
				// Non-blocking send: only the first error is kept.
				// Subsequent workers see done close via the fan-in
				// closer goroutine below.
				select {
				case e.errs <- err:
					close(e.done) // signal sibling workers to stop
				default:
				}
			}
		}()
	}
	// Closer goroutine: once every worker exits, close the batches
	// channel so Next observes EOF.
	go func() {
		e.wg.Wait()
		close(e.batches)
	}()
}

func (e *parallelScanFileExec) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	e.closeOnce.Do(func() {
		// Signal producers to stop. Guarded by closeOnce to keep
		// close(done) safe against startWorkers' error path (which
		// also closes done on the first worker error).
		select {
		case <-e.done:
			// Already closed by an errored worker.
		default:
			close(e.done)
		}
		// Drain any queued batches so producers blocked on send
		// can wake up and exit.
		for b := range e.batches {
			b.Release()
		}
	})
	return nil
}

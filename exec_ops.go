package gobi

import (
	"context"
	"errors"
	"io"

	"github.com/apache/arrow-go/v18/arrow"
)

// -----------------------------------------------------------------------------
// scanFrameExec: leaf source over an in-memory *Frame.
//
// Splits the Frame into batches of at most defaultBatchRows rows.
// Zero-row Frames yield io.EOF immediately.
// -----------------------------------------------------------------------------

type scanFrameExec struct {
	frame     *Frame
	batchRows int
	offset    int
	closed    bool
}

func newScanFrameExec(f *Frame, batchRows int) *scanFrameExec {
	if batchRows <= 0 {
		batchRows = defaultBatchRows
	}
	return &scanFrameExec{frame: f, batchRows: batchRows}
}

func (e *scanFrameExec) Schema() *arrow.Schema { return e.frame.Schema() }

func (e *scanFrameExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if e.closed {
		return nil, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	total := e.frame.NumRows()
	if e.offset >= total {
		return nil, io.EOF
	}
	end := min(e.offset+e.batchRows, total)
	slice := e.frame.slice(int64(e.offset), int64(end))
	e.offset = end
	return frameToBatch(slice), nil
}

func (e *scanFrameExec) Close() error {
	e.closed = true
	return nil
}

// -----------------------------------------------------------------------------
// filterExec: streams input through a predicate.
//
// Delegates the actual filtering to the existing eager Frame.Filter
// path (per batch). Skips batches that end up empty rather than
// forwarding them — downstream operators handle nils but empty
// batches waste cycles.
// -----------------------------------------------------------------------------

type filterExecOp struct {
	input ExecOperator
	cond  Expr
}

func (e *filterExecOp) Schema() *arrow.Schema { return e.input.Schema() }

func (e *filterExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := e.input.Next(ctx)
		if err != nil {
			return nil, err
		}
		frame, err := batchToFrame(batch)
		batch.Release()
		if err != nil {
			return nil, err
		}
		filtered, err := frame.FilterExpr(e.cond)
		if err != nil {
			return nil, err
		}
		if filtered.NumRows() == 0 {
			continue // pull the next batch
		}
		return frameToBatch(filtered), nil
	}
}

func (e *filterExecOp) Close() error { return e.input.Close() }

// -----------------------------------------------------------------------------
// projectExec: applies a set of expressions to each batch.
//
// Reuses the eager `executeSelect` helper — Layer 6 slice 1 pattern:
// wire streaming shape, delegate compute to existing kernels.
// -----------------------------------------------------------------------------

type projectExecOp struct {
	input     ExecOperator
	exprs     []Expr
	outSchema *arrow.Schema
}

func (e *projectExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *projectExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch, err := e.input.Next(ctx)
	if err != nil {
		return nil, err
	}
	frame, err := batchToFrame(batch)
	batch.Release()
	if err != nil {
		return nil, err
	}
	projected, err := executeSelect(frame, e.exprs)
	if err != nil {
		return nil, err
	}
	return frameToBatch(projected), nil
}

func (e *projectExecOp) Close() error { return e.input.Close() }

// -----------------------------------------------------------------------------
// withColumnExec: adds or replaces one column per batch.
// -----------------------------------------------------------------------------

type withColumnExecOp struct {
	input     ExecOperator
	name      string
	expr      Expr
	outSchema *arrow.Schema
}

func (e *withColumnExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *withColumnExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch, err := e.input.Next(ctx)
	if err != nil {
		return nil, err
	}
	frame, err := batchToFrame(batch)
	batch.Release()
	if err != nil {
		return nil, err
	}
	out, err := frame.WithColumnExpr(e.name, e.expr)
	if err != nil {
		return nil, err
	}
	return frameToBatch(out), nil
}

func (e *withColumnExecOp) Close() error { return e.input.Close() }

// -----------------------------------------------------------------------------
// dropExec: removes one column per batch.
// -----------------------------------------------------------------------------

type dropExecOp struct {
	input     ExecOperator
	name      string
	outSchema *arrow.Schema
}

func (e *dropExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *dropExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch, err := e.input.Next(ctx)
	if err != nil {
		return nil, err
	}
	frame, err := batchToFrame(batch)
	batch.Release()
	if err != nil {
		return nil, err
	}
	out, err := frame.DropColumn(e.name)
	if err != nil {
		return nil, err
	}
	return frameToBatch(out), nil
}

func (e *dropExecOp) Close() error { return e.input.Close() }

// -----------------------------------------------------------------------------
// limitExec: caps the total row count across batches, short-circuits
// its upstream once satisfied.
// -----------------------------------------------------------------------------

type limitExecOp struct {
	input     ExecOperator
	remaining int
}

func (e *limitExecOp) Schema() *arrow.Schema { return e.input.Schema() }

func (e *limitExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if e.remaining <= 0 {
		return nil, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch, err := e.input.Next(ctx)
	if err != nil {
		return nil, err
	}
	rows := int(batch.NumRows())
	if rows <= e.remaining {
		e.remaining -= rows
		return batch, nil
	}
	// Need only a prefix — slice to remaining rows.
	sliced := batch.NewSlice(0, int64(e.remaining))
	batch.Release()
	e.remaining = 0
	return sliced, nil
}

func (e *limitExecOp) Close() error { return e.input.Close() }

// -----------------------------------------------------------------------------
// emptyExec: yields nothing. Used by the compiler for emptyNode.
// -----------------------------------------------------------------------------

type emptyExecOp struct {
	schema *arrow.Schema
}

func (e *emptyExecOp) Schema() *arrow.Schema                         { return e.schema }
func (e *emptyExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) { return nil, io.EOF }
func (e *emptyExecOp) Close() error                                  { return nil }

// -----------------------------------------------------------------------------
// materializeExec: fallback for blocking operators (Sort, Aggregate,
// Join, Tail) that cannot stream. Pulls its upstream to completion,
// hands the resulting Frame to a user-supplied compute function, and
// then yields the result as one or more batches.
//
// The compute function is the existing eager op — Frame.SortBy,
// GroupBy.Agg, Frame.Join, etc. Layer 6 keeps them as-is; a later
// slice may re-implement them as true streaming operators
// (streaming hash aggregate, external merge sort) but that's a
// separate project.
// -----------------------------------------------------------------------------

type materializeExecOp struct {
	input     ExecOperator
	compute   func(*Frame) (*Frame, error)
	outSchema *arrow.Schema
	yielded   bool
	// resolved output Frame + batch position for chunked re-emission.
	out    *Frame
	offset int
}

func (e *materializeExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *materializeExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if !e.yielded {
		if err := e.materialize(ctx); err != nil {
			return nil, err
		}
		e.yielded = true
	}
	if e.out == nil || e.offset >= e.out.NumRows() {
		return nil, io.EOF
	}
	end := min(e.offset+defaultBatchRows, e.out.NumRows())
	slice := e.out.slice(int64(e.offset), int64(end))
	e.offset = end
	return frameToBatch(slice), nil
}

func (e *materializeExecOp) materialize(ctx context.Context) error {
	// Pull all upstream batches into one Frame.
	inSchema := e.input.Schema()
	var batches []arrow.RecordBatch
	defer func() {
		for _, b := range batches {
			b.Release()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := e.input.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		batches = append(batches, batch)
	}
	in, err := concatBatchesToFrame(inSchema, batches)
	if err != nil {
		return err
	}
	out, err := e.compute(in)
	if err != nil {
		return err
	}
	e.out = out
	return nil
}

func (e *materializeExecOp) Close() error {
	return e.input.Close()
}

// -----------------------------------------------------------------------------
// scanFileExec: streams batches from a source-package callback API
// (parquetio.ReadFileChunksFunc, csvio.ReadFileChunksFunc).
//
// Bridges push (callback) → pull (Next) via a channel. A background
// goroutine drives the callback and enqueues each Frame as a batch;
// Next pops from the channel. Cancellation flows via context; Close
// stops the producer via a done channel.
//
// This is the operator that makes Layer 6's bounded-memory promise
// real: multi-GB parquet inputs never materialize into a single Frame.
// -----------------------------------------------------------------------------

type scanFileExec struct {
	schema *arrow.Schema
	// batches carries produced batches. Sender closes on completion.
	batches chan arrow.RecordBatch
	// errs carries a single terminal error, if any.
	errs chan error
	// done signals the producer to stop (Close() closes it).
	done   chan struct{}
	closed bool
}

// newScanFileExec launches a producer goroutine that calls fn (the
// source-package's ReadFileChunksFunc) with a callback that ships
// each Frame to the batches channel. fn is expected to return when
// the source is exhausted OR when the callback returns a non-nil
// error (which happens when Close() fires while the callback is
// running — see closedErr below).
//
// schema is the source's declared output schema; used by downstream
// operators before any batch has been produced.
func newScanFileExec(schema *arrow.Schema, fn func(cb func(*Frame) error) error) *scanFileExec {
	e := &scanFileExec{
		schema:  schema,
		batches: make(chan arrow.RecordBatch, 2),
		errs:    make(chan error, 1),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(e.batches)
		err := fn(func(f *Frame) error {
			// Callers retain the batch's arrow buffers past the
			// callback's return — the source package's contract
			// says the Frame is released when the callback returns,
			// so we bump the ref count to keep it alive on the
			// consumer side.
			f.Retain()
			batch := frameToBatch(f)
			// Release our extra ref: frameToBatch retained the
			// arrays it wrapped, so the batch owns them now.
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
			e.errs <- err
		}
	}()
	return e
}

// errScanClosed is the sentinel a scanFileExec's callback returns to
// stop the producer when Close() has fired. Not exposed — swallowed
// by the goroutine so it doesn't leak out as a "real" error.
var errScanClosed = errors.New("gobi: scan closed")

func (e *scanFileExec) Schema() *arrow.Schema { return e.schema }

func (e *scanFileExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case batch, ok := <-e.batches:
		if !ok {
			// Producer finished. Check for a terminal error.
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

func (e *scanFileExec) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	close(e.done)
	// Drain any pending batches so the producer goroutine exits.
	for b := range e.batches {
		b.Release()
	}
	return nil
}

package gobi

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
)

// -----------------------------------------------------------------------------
// scanFrameExec: leaf source over an in-memory *Frame.
//
// Emits batches at chunk-aligned boundaries so every column's slice
// stays inside a single underlying chunk — the invariant
// frameToBatch enforces. batchRows caps the batch size; smaller
// batches are emitted when a chunk boundary falls before the cap.
// Zero-row Frames yield io.EOF immediately.
// -----------------------------------------------------------------------------

type scanFrameExec struct {
	frame     *Frame
	batchRows int
	// boundaries is a sorted list of row offsets where batches split.
	// Constructed so that between any two adjacent entries every
	// column's arrow.NewColumnSlice returns a single-chunk view (see
	// chunkAlignedBoundaries). Empty when the Frame has zero rows.
	boundaries []int64
	idx        int
	closed     bool
}

func newScanFrameExec(f *Frame, batchRows int) *scanFrameExec {
	if batchRows <= 0 {
		batchRows = defaultBatchRows
	}
	return &scanFrameExec{
		frame:      f,
		batchRows:  batchRows,
		boundaries: chunkAlignedBoundaries(f, batchRows),
	}
}

func (e *scanFrameExec) Schema() *arrow.Schema { return e.frame.Schema() }

func (e *scanFrameExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if e.closed {
		return nil, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.idx+1 >= len(e.boundaries) {
		return nil, io.EOF
	}
	start, end := e.boundaries[e.idx], e.boundaries[e.idx+1]
	e.idx++
	slice := e.frame.slice(start, end)
	batch := frameToBatch(slice)
	slice.Release()
	return batch, nil
}

func (e *scanFrameExec) Close() error {
	e.closed = true
	return nil
}

// chunkAlignedBoundaries returns the sorted row offsets at which
// scanFrameExec splits batches. The boundary set is the union of
// every column's chunk-end offsets — guaranteeing that any adjacent
// pair (a, b) sits entirely within one underlying chunk for every
// column — plus batchRows-spaced cuts inserted into spans still
// longer than the cap. Both 0 and totalRows are always present.
//
// This is the load-bearing invariant behind frameToBatch: because
// (a, b) never crosses a chunk boundary in any column, the sliced
// column collapses to a single chunk after arrow.NewColumnSlice,
// and frameToBatch's chunks[0] read is the whole column.
func chunkAlignedBoundaries(f *Frame, batchRows int) []int64 {
	total := int64(f.NumRows())
	if total == 0 {
		return nil
	}
	seen := map[int64]struct{}{0: {}, total: {}}
	for _, s := range f.series {
		if s.col == nil {
			continue
		}
		var off int64
		for _, chunk := range s.col.Data().Chunks() {
			off += int64(chunk.Len())
			if off > 0 && off < total {
				seen[off] = struct{}{}
			}
		}
	}
	ordered := make([]int64, 0, len(seen))
	for k := range seen {
		ordered = append(ordered, k)
	}
	slices.Sort(ordered)
	if batchRows <= 0 {
		return ordered
	}
	// Insert batchRows-spaced cuts into any remaining span > batchRows.
	// A cut inside a single-chunk span keeps both halves single-chunk,
	// so the invariant survives.
	out := make([]int64, 0, len(ordered))
	for i := range len(ordered) - 1 {
		out = append(out, ordered[i])
		for cut := ordered[i] + int64(batchRows); cut < ordered[i+1]; cut += int64(batchRows) {
			out = append(out, cut)
		}
	}
	out = append(out, ordered[len(ordered)-1])
	return out
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
		frame.Release()
		if err != nil {
			return nil, err
		}
		if filtered.NumRows() == 0 {
			filtered.Release()
			continue // pull the next batch
		}
		out := frameToBatch(filtered)
		filtered.Release()
		return out, nil
	}
}

func (e *filterExecOp) Close() error { return e.input.Close() }

// ApplyToFrame runs the filter against f without the batch↔Frame
// bridging. Used by the fusedStreamExecOp path (adjacent streaming
// batch-transform ops applied in one Frame conversion cycle).
func (e *filterExecOp) ApplyToFrame(f *Frame) (*Frame, error) {
	return f.FilterExpr(e.cond)
}

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
	frame.Release()
	if err != nil {
		return nil, err
	}
	out := frameToBatch(projected)
	projected.Release()
	return out, nil
}

func (e *projectExecOp) Close() error { return e.input.Close() }

// ApplyToFrame — see filterExecOp.ApplyToFrame.
func (e *projectExecOp) ApplyToFrame(f *Frame) (*Frame, error) {
	return executeSelect(f, e.exprs)
}

// -----------------------------------------------------------------------------
// withColumnExec: adds or replaces one column per batch.
// -----------------------------------------------------------------------------

type withColumnExecOp struct {
	input     ExecOperator
	name      string
	expr      Expr
	outSchema *arrow.Schema
	// inputMeta is the partition claim carried by the input plan
	// node at Compile time. Attached to the per-batch Frame before
	// expression eval so consumers like Over can check alignment.
	// Nil = no claim (unaligned path).
	inputMeta *PartitionMetadata
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
	if e.inputMeta != nil {
		frame.WithPartitionMeta(e.inputMeta)
	}
	out, err := frame.WithColumnExpr(e.name, e.expr)
	frame.Release()
	if err != nil {
		return nil, err
	}
	batchOut := frameToBatch(out)
	out.Release()
	return batchOut, nil
}

func (e *withColumnExecOp) Close() error { return e.input.Close() }

// ApplyToFrame — see filterExecOp.ApplyToFrame. Also attaches the
// captured input partition metadata before Eval, matching the
// per-batch Next path.
func (e *withColumnExecOp) ApplyToFrame(f *Frame) (*Frame, error) {
	if e.inputMeta != nil {
		f.WithPartitionMeta(e.inputMeta)
	}
	return f.WithColumnExpr(e.name, e.expr)
}

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
	frame.Release()
	if err != nil {
		return nil, err
	}
	batchOut := frameToBatch(out)
	out.Release()
	return batchOut, nil
}

func (e *dropExecOp) Close() error { return e.input.Close() }

// ApplyToFrame — see filterExecOp.ApplyToFrame.
func (e *dropExecOp) ApplyToFrame(f *Frame) (*Frame, error) {
	return f.DropColumn(e.name)
}

// -----------------------------------------------------------------------------
// renameExec: relabels one column per batch. Streaming — pass-through
// batch with a rebuilt schema; underlying arrow arrays unchanged.
// -----------------------------------------------------------------------------

type renameExecOp struct {
	input     ExecOperator
	old, new  string
	outSchema *arrow.Schema
}

func (e *renameExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *renameExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
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
	out, err := frame.Rename(e.old, e.new)
	if err != nil {
		frame.Release()
		return nil, err
	}
	// Rename short-circuits to f itself on old == new. Avoid a
	// double-Release in that case by only Releasing frame when it's
	// a distinct Frame from out.
	if out != frame {
		frame.Release()
	}
	batchOut := frameToBatch(out)
	out.Release()
	return batchOut, nil
}

func (e *renameExecOp) Close() error { return e.input.Close() }

// ApplyToFrame — see filterExecOp.ApplyToFrame.
func (e *renameExecOp) ApplyToFrame(f *Frame) (*Frame, error) {
	return f.Rename(e.old, e.new)
}

// -----------------------------------------------------------------------------
// explodeExec: per-batch multi-part → single-part expansion. Streams
// even though the row count grows — each input batch's Explode is
// independent, no cross-batch dependency.
//
// Output batches may exceed defaultBatchRows when a batch contains
// dense multi-part geometries or long lists. That's a soft cap, not a
// hard one; downstream operators handle whatever size they receive.
// -----------------------------------------------------------------------------

type explodeExecOp struct {
	input     ExecOperator
	name      string
	outSchema *arrow.Schema
}

func (e *explodeExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *explodeExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
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
	out, err := frame.Explode(e.name)
	frame.Release()
	if err != nil {
		return nil, err
	}
	batchOut := frameToBatch(out)
	out.Release()
	return batchOut, nil
}

func (e *explodeExecOp) Close() error { return e.input.Close() }

// ApplyToFrame — see filterExecOp.ApplyToFrame.
func (e *explodeExecOp) ApplyToFrame(f *Frame) (*Frame, error) {
	return f.Explode(e.name)
}

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

func (e *emptyExecOp) Schema() *arrow.Schema                               { return e.schema }
func (e *emptyExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) { return nil, io.EOF }
func (e *emptyExecOp) Close() error                                        { return nil }

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
		// Streaming complete. Downstream batches already Retain the
		// arrow columns they need via frameToBatch, so our reference
		// to the full concat'd Frame is redundant now — drop it so
		// the arrow buffers can be freed while the rest of the plan
		// keeps running. Without this, a plan with N materialize
		// walls pins N full-frame copies in memory for the entire
		// Collect lifetime.
		e.releaseOut()
		return nil, io.EOF
	}
	end := min(e.offset+defaultBatchRows, e.out.NumRows())
	slice := e.out.slice(int64(e.offset), int64(end))
	e.offset = end
	batch := frameToBatch(slice)
	slice.Release()
	return batch, nil
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
	// `in` is a fresh concat Frame — compute either builds a new
	// Frame (in which case `in`'s columns are orphaned) or returns
	// `in` itself. Release unconditionally; NewFrame retains the
	// columns it keeps, so a same-column identity compute stays
	// alive via `out`.
	in.Release()
	if err != nil {
		return err
	}
	e.out = out
	return nil
}

// releaseOut drops the reference to the materialized Frame if it's
// still alive. Idempotent — safe to call from both the EOF path in
// Next and from Close.
func (e *materializeExecOp) releaseOut() {
	if e.out == nil {
		return
	}
	e.out.Release()
	e.out = nil
}

func (e *materializeExecOp) Close() error {
	// Defensive release for the paths where Next never reached EOF
	// (context cancel, downstream error, partial iteration). Next's
	// EOF branch already dropped e.out to nil, so this is a no-op
	// on the happy path.
	e.releaseOut()
	return e.input.Close()
}

// -----------------------------------------------------------------------------
// frameApplier + fusedStreamExecOp:
//
// A streaming batch-transform (filter / project / withColumn / drop /
// rename / explode) does `batch → Frame → apply → Frame → batch` per
// batch. Chained ops repeat the Frame↔batch conversion at each step,
// each round trip allocating per-column arrow.Array + arrow.Column
// headers plus a fresh arrow.RecordBatch.
//
// fusedStreamExecOp collapses adjacent frame-applier ops into one
// batch conversion cycle: pull batch → convert once → apply all ops
// on the running Frame → convert back once. Filter-in-the-middle
// short-circuits the remaining ops for that batch when the Frame
// reaches 0 rows.
// -----------------------------------------------------------------------------

// frameApplier is implemented by streaming batch-transform exec ops
// that can be fused into fusedStreamExecOp. Each op transforms one
// Frame into another with no cross-batch state.
type frameApplier interface {
	ExecOperator
	ApplyToFrame(*Frame) (*Frame, error)
}

type fusedStreamExecOp struct {
	input     ExecOperator
	ops       []frameApplier
	outSchema *arrow.Schema
}

func (e *fusedStreamExecOp) Schema() *arrow.Schema { return e.outSchema }

func (e *fusedStreamExecOp) Next(ctx context.Context) (arrow.RecordBatch, error) {
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
		dropped := false
		for _, op := range e.ops {
			next, err := op.ApplyToFrame(frame)
			if err != nil {
				frame.Release()
				return nil, err
			}
			// Each ApplyToFrame produces a new Frame (Filter/With/Drop
			// all go through Take or NewSeries). Release the prior
			// frame's refs before overwriting; without this, every
			// fused-op link leaks the intermediate frame's columns.
			if next != frame {
				frame.Release()
			}
			frame = next
			if frame.NumRows() == 0 {
				// Filter (or any op that produced no rows) — remaining
				// ops would just produce more empty output. Pull the
				// next batch instead.
				dropped = true
				break
			}
		}
		if dropped {
			frame.Release()
			continue
		}
		out := frameToBatch(frame)
		frame.Release()
		return out, nil
	}
}

func (e *fusedStreamExecOp) Close() error { return e.input.Close() }

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
			// frameToBatch installs an independent Retain on each
			// column via NewRecordBatch, so the batch's array refs
			// survive the source Frame's Release when the callback
			// returns. No extra f.Retain()/f.Release() dance needed
			// here — see exec.go frameToBatch docstring.
			batch := frameToBatch(f)
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

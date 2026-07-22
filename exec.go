package gobi

import (
	"context"
	"fmt"
	"io"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// ExecOperator is a pull-based iterator over arrow record batches.
// Each call to Next produces the next batch of rows (or io.EOF when
// the operator has exhausted its input); the returned batch is
// owned by the caller and must be Released when no longer needed.
//
// Operators can be composed into a tree: `filterExec` wraps another
// ExecOperator as its input and yields filtered batches; `scanFrameExec`
// is a leaf that produces batches from a source Frame. The plan
// compiler (Compile) turns a LogicalPlan into such a tree.
//
// Layer 6 (vectorized execution) is the migration from the whole-
// Frame-per-node dispatch in lazy.go's collectPlan to batch-streaming
// through operators. Slice 1 lands the interface + streaming filter /
// project / limit / with-column / drop and a materializing fallback
// for blocking ops (Sort, Aggregate, Join, Tail); those still route
// to the eager engine internally but see one input Frame instead of
// a per-step re-materialization.
type ExecOperator interface {
	// Next returns the next batch. When no more batches are available
	// it returns (nil, io.EOF). Callers must Release the returned
	// batch when done.
	Next(ctx context.Context) (arrow.RecordBatch, error)

	// Schema is the operator's output schema. Stable across all
	// batches; safe to call before Next has been called.
	Schema() *arrow.Schema

	// Close releases resources held by the operator AND its upstream
	// operators. Idempotent — safe to call multiple times. Called by
	// Execute at the end of a plan, so hand-written test drivers
	// usually don't need to call it explicitly.
	Close() error
}

// defaultBatchRows is the batch size used by streaming source
// operators when they need to split a Frame into chunks. Matches
// parquetio.DefaultChunkRows for symmetry across the read path.
const defaultBatchRows = 64 * 1024

// Execute drives op to EOF, gathers every batch, and assembles them
// into a single-chunk *Frame. Closes op unconditionally.
//
// This is the terminal entry point for the streaming executor:
// LazyFrame.Collect calls Compile + Execute. Callers that want to
// consume batches directly (streaming ETL) should walk op.Next in a
// loop themselves — but that's a lower-level API not yet exposed.
func Execute(ctx context.Context, op ExecOperator) (*Frame, error) {
	defer op.Close()

	schema := op.Schema()
	batches := make([]arrow.RecordBatch, 0, 8)
	defer func() {
		for _, b := range batches {
			b.Release()
		}
	}()

	for {
		batch, err := op.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if batch == nil {
			continue
		}
		if batch.NumRows() == 0 {
			batch.Release()
			continue
		}
		batches = append(batches, batch)
	}

	return concatBatchesToFrame(schema, batches)
}

// concatBatchesToFrame stitches record batches into one Frame with a
// single Arrow chunk per column. Concatenates via array.Concatenate.
//
// An empty batch list produces an empty (zero-row) Frame with the
// operator's declared schema — matches the emptyNode behavior so
// pipelines that prune all input still return a well-formed Frame.
func concatBatchesToFrame(schema *arrow.Schema, batches []arrow.RecordBatch) (*Frame, error) {
	if len(batches) == 0 {
		return emptyFrame(schema)
	}
	pool := memory.DefaultAllocator
	numCols := len(schema.Fields())
	outCols := make([]arrow.Column, numCols)

	for i := range numCols {
		chunks := make([]arrow.Array, 0, len(batches))
		for _, b := range batches {
			chunks = append(chunks, b.Column(i))
		}
		combined, err := array.Concatenate(chunks, pool)
		if err != nil {
			return nil, fmt.Errorf("gobi: Execute concat col %d: %w", i, err)
		}
		field := schema.Field(i)
		chunked := arrow.NewChunked(combined.DataType(), []arrow.Array{combined})
		outCols[i] = *arrow.NewColumn(field, chunked)
		combined.Release()
	}
	return NewFrame(schema, outCols)
}

// -----------------------------------------------------------------------------
// Adapters between Frame (batch-of-1) and arrow.RecordBatch. Both
// used internally by the streaming operators to bridge the existing
// eager engine (which speaks Frame) and the executor's batch bus.
// -----------------------------------------------------------------------------

// frameToBatch produces a RecordBatch view of f's columns. Assumes
// each Series has a single chunk (which is true for Frames produced
// by the eager engine's fast paths). The returned batch shares
// buffers with f — do not release both.
func frameToBatch(f *Frame) arrow.RecordBatch {
	schema := f.Schema()
	arrs := make([]arrow.Array, f.NumCols())
	for i, s := range f.series {
		chunks := s.col.Data().Chunks()
		arrs[i] = chunks[0]
		arrs[i].Retain() // NewRecord's contract requires a live ref
	}
	return array.NewRecord(schema, arrs, int64(f.NumRows()))
}

// batchToFrame wraps a RecordBatch as a Frame — inverse of
// frameToBatch. Uses arrow.NewColumnFromArr so the Frame owns
// independent refs; batch and Frame can be Released separately.
func batchToFrame(batch arrow.RecordBatch) (*Frame, error) {
	schema := batch.Schema()
	numCols := int(batch.NumCols())
	cols := make([]arrow.Column, numCols)
	for i := range numCols {
		cols[i] = arrow.NewColumnFromArr(schema.Field(i), batch.Column(i))
	}
	return NewFrame(schema, cols)
}

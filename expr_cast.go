package gobi

import (
	"context"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/compute"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Cast returns an expression that converts the inner column to the
// given arrow data type. Numeric-to-numeric conversions are supported
// today; string / boolean / date-time conversions are follow-ups.
//
// Supported source→target combinations (v1, numeric-to-numeric only):
//
//   - Float64 ← {Float32, Int64, Int32, Uint64, Uint32}
//   - Int64   ← {Float64, Float32, Int32, Uint32}
//   - Float32 ← {Float64, Int64, Int32}
//   - Int32   ← {Int64, Float64, Float32}
//   - Uint64  ← {Int64, Uint32}
//   - Uint32  ← {Uint64, Int64}
//
// Same-type is a no-op (returns the input). Unsupported combinations
// error at Eval with a clear message.
//
// Numeric widening carries values exactly. Narrowing (Int64→Int32,
// Float64→Int64, etc.) truncates without overflow-checking — mirrors
// Go's numeric conversion semantics. Nulls propagate: a null row in
// the source produces a null row in the output.
//
// Primary motivating use case is unlocking numeric widening in
// If / Coalesce, which require exact type match otherwise:
//
//	gobi.If(cond, gobi.Col("int_col").Cast(arrow.PrimitiveTypes.Float64),
//	              gobi.Lit(1.5))
//
// # Implementation
//
// Since v0.3.9 the numeric-cast kernel is arrow-go's
// `arrow.compute.CastArray`, which on arm64 dispatches to
// hand-written NEON SIMD (`internal/kernels/cast_numeric_neon_arm64.s`)
// and on amd64 to hand-written AVX2/SSE4 SIMD. Measured 13.9× faster
// than the previous hand-rolled scalar builder loops on a
// 10M-row Float64→Int64 cast (arm64, Apple M3 Pro). See the
// `compute` package docstring for the arrow-go vs gobi kernel
// overlap map and the benchmark that produced the number.
//
// gobi wraps the arrow-go call to preserve the previous surface:
// multi-chunk columns cast per chunk and reassemble; overflow modes
// match Go's built-in conversion (silent wrap for int narrowing,
// silent truncate for float→int); the whitelist of accepted target
// types stays limited to numeric so unrelated arrow-go cast
// capabilities (String↔Int, Date↔Timestamp) don't silently
// activate — those are opt-in follow-ups.
func (e Expr) Cast(target arrow.DataType) Expr {
	return Expr{node: &castNode{inner: e.node, target: target}}
}

type castNode struct {
	inner  ExprNode
	target arrow.DataType
}

func (n *castNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: Cast on nil inner expression")
	}
	if n.target == nil {
		return Series{}, fmt.Errorf("gobi: Cast target type is nil")
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return castSeries(s, n.target)
}

func (n *castNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.target == nil {
		return nil, fmt.Errorf("gobi: Cast target type is nil")
	}
	if _, err := n.inner.Type(schema); err != nil {
		return nil, err
	}
	return n.target, nil
}

func (n *castNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *castNode) String() string {
	return fmt.Sprintf("%s.cast(%s)", n.inner, n.target)
}

// castSeries converts s to target via arrow-go's SIMD-accelerated
// compute.CastArray. Same-type is a short-circuit no-op. Multi-
// chunk columns cast per chunk then reassemble into a chunked
// output; single-chunk (the common case for streaming batches)
// hits the fast one-pass path.
func castSeries(s Series, target arrow.DataType) (Series, error) {
	if arrow.TypeEqual(s.DataType(), target) {
		return s, nil
	}
	// Whitelist: only numeric targets. arrow-go's cast surface is
	// broader (String↔Int, Date↔Timestamp, etc.) but the previous
	// gobi.Cast contract stopped at numeric-to-numeric; keep that
	// contract stable until each additional path is deliberately
	// exposed with its own semantic pass.
	switch target.ID() {
	case arrow.FLOAT64, arrow.FLOAT32,
		arrow.INT64, arrow.INT32,
		arrow.UINT64, arrow.UINT32:
	default:
		return Series{}, fmt.Errorf(
			"%w: Cast: unsupported target type %s (v1 covers Float32/64, Int32/64, Uint32/64)",
			ErrExprTypeMismatch, target)
	}

	// TODO: thread ctx through castNode.Eval so an in-flight
	// arrow.compute.CastArray call can be cancelled with the
	// surrounding pipeline. Blocked on ExprNode.Eval not carrying
	// a context.Context today; mechanical follow-up.
	ctx := context.Background()
	pool := memory.DefaultAllocator
	opts := &compute.CastOptions{
		ToType:             target,
		AllowIntOverflow:   true, // matches Go's silent wrap on int narrowing
		AllowFloatTruncate: true, // matches Go's silent truncate on float→int
		AllowTimeTruncate:  true, // preserved for future timestamp targets
	}

	chunks := s.col.Data().Chunks()
	outChunks := make([]arrow.Array, 0, len(chunks))
	// Every branch below (error and success) must release the
	// CastArray outputs we own. arrow.NewChunked RETAINS each
	// input array (does not steal ownership) — so after passing
	// outChunks to NewChunked, we still own the initial CastArray
	// ref and need to release it. The deferred loop covers both
	// paths uniformly.
	defer func() {
		for _, c := range outChunks {
			c.Release()
		}
	}()
	for _, chunk := range chunks {
		out, err := compute.CastArray(ctx, chunk, opts)
		if err != nil {
			return Series{}, fmt.Errorf(
				"%w: Cast %s → %s: %v",
				ErrExprTypeMismatch, chunk.DataType(), target, err)
		}
		outChunks = append(outChunks, out)
	}
	chunked := arrow.NewChunked(target, outChunks)
	defer chunked.Release()
	arr, err := arrayFromChunked(chunked)
	if err != nil {
		return Series{}, fmt.Errorf(
			"%w: Cast %s → %s: assemble chunks: %v",
			ErrExprTypeMismatch, s.DataType(), target, err)
	}
	return arrayToSeries(pool, "cast", target, arr)
}

// arrayFromChunked returns the single concrete Array a chunked
// carries when it has exactly one chunk, else concatenates. Used
// by castSeries to produce an arrow.Array from
// compute.CastArray's per-chunk outputs. Single-chunk (the
// streaming batch case) avoids the copy.
//
// The returned array carries a refcount the caller must Release.
// Empty-input case still returns a valid (length-0) array of the
// target type — matches the "casting an empty column returns an
// empty column of the target type" contract.
func arrayFromChunked(c *arrow.Chunked) (arrow.Array, error) {
	if c.Len() == 0 {
		return array.MakeArrayOfNull(memory.DefaultAllocator, c.DataType(), 0), nil
	}
	if len(c.Chunks()) == 1 {
		a := c.Chunk(0)
		a.Retain() // outlives the chunked's release
		return a, nil
	}
	pool := memory.DefaultAllocator
	out, err := array.Concatenate(c.Chunks(), pool)
	if err != nil {
		return nil, err
	}
	return out, nil
}

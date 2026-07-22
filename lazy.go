package gobi

import (
	"context"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// LazyFrame is a Frame that hasn't been computed yet — a chain of
// operations expressed as a plan tree. Building a LazyFrame does no
// work; each fluent method appends a node to the plan. Call Collect()
// to actually run it.
//
// Today (Slice 1 of Layer 2-3) Collect() dispatches each plan node to
// the existing eager Frame methods. There is no optimizer yet — a
// LazyFrame chain runs at exactly the same speed as the equivalent
// eager code. The value here is API shape:
//
//   - Inspect a plan without running it: Explain() / Schema().
//   - Compose complex pipelines without naming intermediate Frames.
//   - Ready surface for Layer 4 optimizer passes and Layer 6
//     vectorized executor.
type LazyFrame struct {
	plan LogicalPlan
}

// Lazy wraps f in a LazyFrame anchored at a Scan[frame] leaf. Fluent
// operations on the returned LazyFrame append nodes to the plan;
// Collect() replays them against f.
func (f *Frame) Lazy() *LazyFrame {
	return &LazyFrame{plan: &scanFrameNode{frame: f}}
}

// NewLazyFrame constructs a LazyFrame from an arbitrary LogicalPlan.
// Reserved for advanced users building their own plan trees (custom
// scan nodes, plan-tree rewrites). Most callers should use
// Frame.Lazy() and the fluent builders.
func NewLazyFrame(plan LogicalPlan) *LazyFrame {
	return &LazyFrame{plan: plan}
}

// Plan returns the underlying plan tree. Used by tree walkers and by
// tests exercising plan structure directly.
func (lf *LazyFrame) Plan() LogicalPlan { return lf.plan }

// -----------------------------------------------------------------------------
// Fluent builders
// -----------------------------------------------------------------------------

// Filter appends a Filter node. cond must produce a Boolean Series
// when evaluated; the error surfaces at Collect() time, not here.
func (lf *LazyFrame) Filter(cond Expr) *LazyFrame {
	return &LazyFrame{plan: &filterNode{input: lf.plan, cond: cond}}
}

// Select appends a Project node — the resulting LazyFrame contains
// only the columns produced by exprs, in that order. Each expression's
// output column name comes from Namer.OutputName if the expression
// has one (Col("id") → "id", something.Alias("x") → "x"); otherwise
// a positional default ("expr_0", "expr_1", ...) is used.
//
// Zero expressions is a valid but empty Select — the resulting Frame
// has 0 columns. Add an Alias if you want a non-default output name
// for computed columns.
func (lf *LazyFrame) Select(exprs ...Expr) *LazyFrame {
	return &LazyFrame{plan: newProjectNode(lf.plan, exprs)}
}

// WithColumn appends a WithColumn node. If a column named name
// already exists at this point in the plan, it will be replaced;
// otherwise the new column is appended.
func (lf *LazyFrame) WithColumn(name string, e Expr) *LazyFrame {
	return &LazyFrame{plan: newWithColumnNode(lf.plan, name, e)}
}

// Limit appends a Limit node that keeps the first n rows. Negative
// or zero n produces an empty result at Collect() time.
func (lf *LazyFrame) Limit(n int) *LazyFrame {
	return &LazyFrame{plan: &limitNode{input: lf.plan, n: n}}
}

// SortBy appends a Sort node. Semantics match Frame.SortBy: multi-key
// stable, nulls-last, direction per-key.
func (lf *LazyFrame) SortBy(keys ...SortKey) *LazyFrame {
	return &LazyFrame{plan: &sortNode{input: lf.plan, keys: keys}}
}

// GroupBy returns a lazy group-by builder. Chain .Agg(aggs...) to get
// back a LazyFrame whose Collect() runs Frame.GroupBy(keys...).Agg(aggs...).
//
// The two-step shape mirrors the eager API — no ambiguity about what
// gb, err := lf.GroupBy(...) means (there's no error return here,
// missing keys surface at Collect).
func (lf *LazyFrame) GroupBy(keys ...string) *LazyGroupBy {
	return &LazyGroupBy{plan: lf.plan, keys: keys}
}

// Join appends a Join node combining lf and right on (leftKey,
// rightKey). Semantics match Frame.Join for all six JoinType values;
// see that method's documentation for null-key handling and key-
// coalescing on Right/Full joins.
func (lf *LazyFrame) Join(right *LazyFrame, leftKey, rightKey string, kind JoinType) *LazyFrame {
	return &LazyFrame{plan: newJoinNode(lf.plan, right.plan, leftKey, rightKey, kind)}
}

// DropColumn appends a Drop node. If the named column is missing at
// Collect time, the underlying Frame.DropColumn surfaces
// ErrColumnNotFound.
func (lf *LazyFrame) DropColumn(name string) *LazyFrame {
	return &LazyFrame{plan: newDropNode(lf.plan, name)}
}

// Head returns the first n rows. Alias for Limit — the plan node is
// the same. Kept as a separate method for pandas-shaped ergonomics.
func (lf *LazyFrame) Head(n int) *LazyFrame { return lf.Limit(n) }

// Tail returns the last n rows. Unlike Head, this cannot be
// implemented as a plain Limit — offset depends on the total row
// count, which isn't known until the input has been materialized.
// Dispatches to Frame.Tail at Collect time.
func (lf *LazyFrame) Tail(n int) *LazyFrame {
	return &LazyFrame{plan: &tailNode{input: lf.plan, n: n}}
}

// LazyGroupBy is the intermediate builder returned by
// LazyFrame.GroupBy. Its only method, Agg, closes the group-by out
// into a LazyFrame.
type LazyGroupBy struct {
	plan LogicalPlan
	keys []string
}

// Agg attaches the given aggregations and returns a LazyFrame whose
// output schema is [group keys...] + [agg outputs...] in the order
// given. Naming, type mapping, and null semantics match the eager
// GroupBy.Agg.
func (lg *LazyGroupBy) Agg(aggs ...Aggregation) *LazyFrame {
	return &LazyFrame{plan: newAggregateNode(lg.plan, lg.keys, aggs)}
}

// -----------------------------------------------------------------------------
// Introspection
// -----------------------------------------------------------------------------

// Schema returns the plan's output schema without executing anything.
// For plans that involve type inference (Project, WithColumn), fields
// whose expressions can't be statically typed are reported with a
// nil Type — Collect() will surface the underlying evaluation error.
func (lf *LazyFrame) Schema() *arrow.Schema {
	return lf.plan.Schema()
}

// Explain returns a human-readable representation of the plan tree.
// Deepest node first (which is how the tree evaluates: bottom-up).
// Handy for debugging what a chain of fluent methods actually built.
//
// Example:
//
//	Limit(1000)
//	  Project(col("id"), (col("value") * lit(1.08)) AS "usd")
//	    Filter((col("value") > lit(100)))
//	      Scan[frame](500 rows × 3 cols)
func (lf *LazyFrame) Explain() string {
	var sb strings.Builder
	explainPlan(lf.plan, &sb, 0)
	return sb.String()
}

func explainPlan(p LogicalPlan, sb *strings.Builder, depth int) {
	for range depth {
		sb.WriteString("  ")
	}
	sb.WriteString(p.String())
	sb.WriteByte('\n')
	for _, c := range p.Children() {
		explainPlan(c, sb, depth+1)
	}
}

// -----------------------------------------------------------------------------
// Collect: walk the plan bottom-up, dispatching each node to the
// eager engine. Errors from any node propagate up the walk.
// -----------------------------------------------------------------------------

// Collect materializes the LazyFrame into a concrete *Frame. Runs
// the default optimizer, compiles the plan into a tree of
// ExecOperators, and drives execution via the streaming executor.
//
// Streaming operators (Filter, Project, WithColumn, Drop, Limit)
// pull one batch at a time; blocking operators (Sort, Aggregate,
// Join, Tail) buffer their input to a Frame and delegate to the
// eager engine. Peak memory is bounded to one batch per streaming
// node plus the accumulated Frame at each blocking node.
//
// This is where errors surface: bad expressions, type mismatches,
// unknown columns, scan failures.
func (lf *LazyFrame) Collect() (*Frame, error) {
	op, err := Compile(Optimize(lf.plan))
	if err != nil {
		return nil, err
	}
	return Execute(context.Background(), op)
}

// CollectRaw skips both the optimizer AND the streaming executor,
// executing the plan tree via the bottom-up whole-Frame walker
// used before Layer 6. Useful for debugging optimizer bugs, for
// benchmarks that isolate the eager engine's cost, and as a
// correctness oracle for the executor's tests. Prefer Collect for
// real use.
func (lf *LazyFrame) CollectRaw() (*Frame, error) {
	return collectPlan(lf.plan)
}

// Optimize returns the optimizer-rewritten plan without executing.
// Inspection aid: pair with String() / Explain() to see what a chain
// of fluent methods reduces to.
func (lf *LazyFrame) Optimize() LogicalPlan { return Optimize(lf.plan) }

// ExplainOptimized returns the Explain output for the plan tree
// after the default rule set has been applied.
func (lf *LazyFrame) ExplainOptimized() string {
	var sb strings.Builder
	explainPlan(Optimize(lf.plan), &sb, 0)
	return sb.String()
}

func collectPlan(p LogicalPlan) (*Frame, error) {
	switch n := p.(type) {
	case *scanFrameNode:
		return n.frame, nil
	case *filterNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		return f.FilterExpr(n.cond)
	case *projectNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		return executeSelect(f, n.exprs)
	case *withColumnNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		return f.WithColumnExpr(n.name, n.expr)
	case *limitNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		if n.n <= 0 {
			// Frame.Head(0) treats 0 as the default (5), which is wrong
			// for a lazy Limit(0). Return a same-schema empty frame via
			// take-with-no-indexes instead.
			return f.take(nil)
		}
		return f.Head(n.n), nil
	case *sortNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		return f.SortBy(n.keys...)
	case *aggregateNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		gb, err := f.GroupBy(n.keys...)
		if err != nil {
			return nil, err
		}
		return gb.Agg(n.aggs...)
	case *joinNode:
		left, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		right, err := collectPlan(n.right)
		if err != nil {
			return nil, err
		}
		return left.Join(right, n.leftKey, n.rightKey, n.kind)
	case *dropNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		return f.DropColumn(n.name)
	case *tailNode:
		f, err := collectPlan(n.input)
		if err != nil {
			return nil, err
		}
		if n.n <= 0 {
			return f.Tail(0), nil
		}
		return f.Tail(n.n), nil
	case *scanFileNode:
		if n.read == nil {
			return nil, fmt.Errorf("gobi: scanFileNode has no read function")
		}
		return n.read()
	case *emptyNode:
		return emptyFrame(n.Schema())
	}
	return nil, fmt.Errorf("gobi: collectPlan: unknown node %T", p)
}

// emptyFrame constructs a zero-row Frame matching schema. Produced by
// Collect when an emptyNode is reached (a Filter(false) that the
// optimizer folded away).
func emptyFrame(schema *arrow.Schema) (*Frame, error) {
	pool := memory.DefaultAllocator
	fields := schema.Fields()
	cols := make([]arrow.Column, len(fields))
	for i, f := range fields {
		b := array.NewBuilder(pool, f.Type)
		defer b.Release()
		arr := b.NewArray()
		chunked := arrow.NewChunked(f.Type, []arrow.Array{arr})
		cols[i] = *arrow.NewColumn(f, chunked)
	}
	return NewFrame(schema, cols)
}

// executeSelect is the eager engine for a Project node. Evaluates
// each Expr against f, gathers the resulting Series as columns, and
// assembles a new Frame with just those columns.
//
// Naming rules match projectNode's schema computation: Namer nodes
// win, everything else falls back to expr_N positional.
func executeSelect(f *Frame, exprs []Expr) (*Frame, error) {
	if len(exprs) == 0 {
		// Consistent with an empty Project — no columns, same row count.
		schema := arrow.NewSchema(nil, schemaMetadataPtr(f.Schema()))
		return NewFrame(schema, nil)
	}
	outFields := make([]arrow.Field, len(exprs))
	outCols := make([]arrow.Column, len(exprs))
	for i, e := range exprs {
		if e.node == nil {
			return nil, fmt.Errorf("gobi: Select expression %d is nil", i)
		}
		s, err := e.node.Eval(f)
		if err != nil {
			return nil, fmt.Errorf("gobi: Select[%d]: %w", i, err)
		}
		name := exprOutputName(e, i)
		s = markNullable(renameSeries(s, name))
		outFields[i] = s.field
		outCols[i] = *arrow.NewColumn(s.field, s.col.Data())
	}
	schema := arrow.NewSchema(outFields, schemaMetadataPtr(f.Schema()))
	return NewFrame(schema, outCols)
}

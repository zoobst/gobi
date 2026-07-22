package gobi

import (
	"context"
	"fmt"
)

// Compile translates a LogicalPlan tree into a tree of ExecOperators
// ready for streaming execution via Execute.
//
// The mapping is one-to-one for streaming operators (Filter →
// filterExec, Project → projectExec, and so on). Blocking operators
// (Sort, Aggregate, Join, Tail) compile to a materializeExec that
// pulls its upstream to a Frame and delegates the actual computation
// to the existing eager engine. This keeps Layer 6 slice 1 focused
// on the execution model itself; later slices can replace each
// materializeExec with a native streaming implementation.
//
// Compile itself does no I/O and starts no goroutines. Scan
// operators that use a background producer (scanFileExec) start
// their goroutine on construction, so callers should always follow
// Compile with Execute or the operator's Close to avoid leaks.
func Compile(p LogicalPlan) (ExecOperator, error) {
	if p == nil {
		return nil, fmt.Errorf("gobi: Compile: nil plan")
	}
	switch n := p.(type) {
	case *scanFrameNode:
		return newScanFrameExec(n.frame, defaultBatchRows), nil

	case *scanFileNode:
		return compileScanFile(n)

	case *emptyNode:
		return &emptyExecOp{schema: n.Schema()}, nil

	case *filterNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		return &filterExecOp{input: child, cond: n.cond}, nil

	case *projectNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		return &projectExecOp{input: child, exprs: n.exprs, outSchema: n.outSchema}, nil

	case *withColumnNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		return &withColumnExecOp{
			input: child, name: n.name, expr: n.expr, outSchema: n.outSchema,
		}, nil

	case *dropNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		return &dropExecOp{input: child, name: n.name, outSchema: n.outSchema}, nil

	case *limitNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		return &limitExecOp{input: child, remaining: n.n}, nil

	// Blocking operators: pull upstream, materialize, delegate to
	// eager engine. Wrapped in materializeExec so downstream ops
	// still see a streaming source.

	case *sortNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		keys := n.keys
		return &materializeExecOp{
			input:     child,
			outSchema: n.Schema(),
			compute:   func(f *Frame) (*Frame, error) { return f.SortBy(keys...) },
		}, nil

	case *aggregateNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		keys, aggs := n.keys, n.aggs

		// Native streaming path: only for built-in Aggregation Kinds.
		// Custom Fn aggregators expect all rows at once via their
		// Aggregate(Series, []int) signature, which can't be
		// incrementalized without changing the interface.
		if allBuiltInAggs(aggs) {
			// Worker count for the partitioned build. resolveWorkers
			// returns >=1 and folds SetMaxParallelism + GOMAXPROCS in
			// the documented priority.
			return &streamingAggregateExec{
				input:     child,
				keys:      keys,
				aggs:      aggs,
				outSchema: n.outSchema,
				workers:   resolveWorkers(),
				keyMode:   pickKeyMode(n),
			}, nil
		}

		// Fallback: buffer the whole input, hand to eager engine.
		return &materializeExecOp{
			input:     child,
			outSchema: n.outSchema,
			compute: func(f *Frame) (*Frame, error) {
				gb, err := f.GroupBy(keys...)
				if err != nil {
					return nil, err
				}
				return gb.Agg(aggs...)
			},
		}, nil

	case *joinNode:
		left, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		right, err := Compile(n.right)
		if err != nil {
			return nil, err
		}
		// Left-driven kinds (Inner, Left, Semi, Anti) stream the
		// probe side against a materialized build. Right and Full
		// need a second-phase pass to emit unmatched right rows,
		// which requires state that grows with the build side;
		// those still route through the materializing fallback.
		if canStreamJoin(n.kind) {
			return &streamingJoinExec{
				left:      left,
				right:     right,
				leftKey:   n.leftKey,
				rightKey:  n.rightKey,
				kind:      n.kind,
				outSchema: n.outSchema,
			}, nil
		}
		// Fallback: materialize both sides, delegate to eager
		// Frame.Join.
		rightFrame, err := Execute(context.Background(), right)
		if err != nil {
			left.Close()
			return nil, err
		}
		leftKey, rightKey, kind := n.leftKey, n.rightKey, n.kind
		return &materializeExecOp{
			input:     left,
			outSchema: n.outSchema,
			compute: func(f *Frame) (*Frame, error) {
				return f.Join(rightFrame, leftKey, rightKey, kind)
			},
		}, nil

	case *tailNode:
		child, err := Compile(n.input)
		if err != nil {
			return nil, err
		}
		nRows := n.n
		return &materializeExecOp{
			input:     child,
			outSchema: n.Schema(),
			compute: func(f *Frame) (*Frame, error) {
				if nRows <= 0 {
					return f.take(nil)
				}
				return f.Tail(nRows), nil
			},
		}, nil
	}
	return nil, fmt.Errorf("gobi: Compile: unknown plan node %T", p)
}

// allBuiltInAggs reports whether every Aggregation uses a built-in
// Kind (no custom Fn). The streaming hash aggregate only supports
// built-ins; custom aggregators need the whole row set at once and
// fall back to the materializing path.
func allBuiltInAggs(aggs []Aggregation) bool {
	for _, a := range aggs {
		if a.Fn != nil {
			return false
		}
	}
	return true
}

// compileScanFile picks scan strategy in order of preference:
//
//  1. Parallel streaming (WithParallelStreamReads returning >1
//     sub-callbacks). Fan-in across worker goroutines.
//  2. Serial streaming (WithStreamRead). One producer goroutine
//     bridging callback→pull.
//  3. Materialize-then-batch (WithStreamRead absent, `read`
//     present). Reads the whole source into a Frame, then
//     re-batches — correct but not memory-bounded.
//
// Only #1 provides both bounded memory AND multi-core throughput.
// Sources that want the executor to use all cores should register
// WithParallelStreamReads (parquetio.ScanFile does).
func compileScanFile(n *scanFileNode) (ExecOperator, error) {
	if n.parallelStream != nil {
		subs := n.parallelStream()
		if len(subs) > 1 {
			return newParallelScanFileExec(n.Schema(), subs), nil
		}
	}
	if n.streamRead != nil {
		return newScanFileExec(n.Schema(), n.streamRead), nil
	}
	if n.read == nil {
		return nil, fmt.Errorf("gobi: scanFileNode has neither parallelStream, streamRead, nor read")
	}
	// Fallback: materialize then batch. Still correct, just not
	// memory-bounded. Sources that care about bounded memory should
	// register WithStreamRead.
	f, err := n.read()
	if err != nil {
		return nil, err
	}
	return newScanFrameExec(f, defaultBatchRows), nil
}

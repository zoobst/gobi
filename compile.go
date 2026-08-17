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
	op, err := compileNode(p)
	if err != nil {
		return nil, err
	}
	return fuseStreamChains(op), nil
}

// compileNode is the raw plan-to-exec translation. Compile wraps it
// with fuseStreamChains, a post-pass that coalesces adjacent
// streaming batch-transforms into a single fusedStreamExecOp — saves
// one batch↔Frame conversion cycle per fused op.
func compileNode(p LogicalPlan) (ExecOperator, error) {
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
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		// Over in a filter predicate would slice partitions across
		// batch boundaries. Force materialize before per-batch filter.
		if exprContainsOver(n.cond.node) {
			cond := n.cond
			return &materializeExecOp{
				input:     child,
				outSchema: n.Schema(),
				compute: func(f *Frame) (*Frame, error) {
					return f.FilterExpr(cond)
				},
			}, nil
		}
		return &filterExecOp{input: child, cond: n.cond}, nil

	case *projectNode:
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		// Same Over-crossing-batches concern for Select expressions.
		for _, e := range n.exprs {
			if exprContainsOver(e.node) {
				exprs := n.exprs
				return &materializeExecOp{
					input:     child,
					outSchema: n.outSchema,
					compute: func(f *Frame) (*Frame, error) {
						return executeSelect(f, exprs)
					},
				}, nil
			}
		}
		return &projectExecOp{input: child, exprs: n.exprs, outSchema: n.outSchema}, nil

	case *withColumnNode:
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		// If the expression contains an Over (scalar-agg or shape-
		// preserving), it needs to see the whole input Frame at once
		// — per-batch eval would slice partitions at batch boundaries
		// and produce wrong results. Route through materialize.
		if exprContainsOver(n.expr.node) {
			name, expr := n.name, n.expr
			inputMeta := n.input.PartitionMetadata()
			return &materializeExecOp{
				input:     child,
				outSchema: n.outSchema,
				compute: func(f *Frame) (*Frame, error) {
					if inputMeta != nil {
						f.WithPartitionMeta(inputMeta)
					}
					return f.WithColumnExpr(name, expr)
				},
			}, nil
		}
		return &withColumnExecOp{
			input:     child,
			name:      n.name,
			expr:      n.expr,
			outSchema: n.outSchema,
			// Capture the input's partition claim at Compile time so
			// per-batch expression Eval (Over in particular) can see
			// the alignment context via frame.PartitionMetadata().
			inputMeta: n.input.PartitionMetadata(),
		}, nil

	case *dropNode:
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		return &dropExecOp{input: child, name: n.name, outSchema: n.outSchema}, nil

	case *limitNode:
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		return &limitExecOp{input: child, remaining: n.n}, nil

	// Blocking operators: pull upstream, materialize, delegate to
	// eager engine. Wrapped in materializeExec so downstream ops
	// still see a streaming source.

	case *sortNode:
		child, err := compileNode(n.input)
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
		child, err := compileNode(n.input)
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
			exec := &streamingAggregateExec{
				input:     child,
				keys:      keys,
				aggs:      aggs,
				outSchema: n.outSchema,
				workers:   resolveWorkers(),
				keyMode:   pickKeyMode(n),
			}
			// New closes over an initial capacity so cold-cache
			// Gets return an already-sized *[]int. Actual per-
			// dispatch guess (nRows/workers+8) still grows the
			// slice on first use for over-guess batches; the
			// starting cap here is just a hint to reduce
			// per-append reallocs on the very first batch.
			exec.rowsPool.New = func() any {
				s := make([]int, 0, 128)
				return &s
			}
			return exec, nil
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
		left, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		right, err := compileNode(n.right)
		if err != nil {
			return nil, err
		}
		// Alignment-aware Inner fast path: when both sides carry
		// PartitionMetadata proving same-key rows are colocated AND
		// each side is sorted on the join key with SortEnforced=true,
		// swap in the sort-merge executor. Eliminates the hash-index
		// build entirely — see exec_join_merge.go for the mechanics.
		//
		// Inner-only for step 7. Left/Semi/Anti stay on the streaming
		// hash path even when the inputs would otherwise qualify.
		if n.kind == JoinInner && canMergeJoin(n) {
			return &sortMergeJoinExec{
				left:      left,
				right:     right,
				leftKey:   n.leftKey,
				rightKey:  n.rightKey,
				outSchema: n.outSchema,
			}, nil
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
		child, err := compileNode(n.input)
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

	case *explodeNode:
		// Row-cardinality change (one → N per parent), but each parent
		// row expands independently — no cross-batch dependency. Runs
		// per-batch through explodeExecOp; output batches may exceed
		// the batch-size soft cap when dense multi-part geometries or
		// long lists arrive.
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		return &explodeExecOp{
			input:     child,
			name:      n.name,
			outSchema: n.Schema(),
		}, nil

	case *renameNode:
		// Rename is schema-only — a per-batch relabel, no buffering
		// required. Streams like Filter / Project / Drop.
		child, err := compileNode(n.input)
		if err != nil {
			return nil, err
		}
		return &renameExecOp{
			input:     child,
			old:       n.old,
			new:       n.new,
			outSchema: n.Schema(),
		}, nil

	case *partitionAssertionNode:
		// Assertion is a metadata-only wrapper — compile the input
		// directly, no executor node needed. The metadata claim is
		// consumed at plan time (by alignment predicates), not at
		// runtime.
		return compileNode(n.input)
	}
	return nil, fmt.Errorf("gobi: Compile: unknown plan node %T", p)
}

// exprContainsOver reports whether the expression tree rooted at node
// includes any *overNode. Over evaluates per-partition and needs to
// see the whole input Frame at once — streaming per-batch execution
// would treat each batch as a disjoint partition and produce wrong
// results at batch boundaries. Callers force materialize when this
// returns true.
//
// Applies to every Over shape (scalar-aggregate and shape-preserving)
// because both have cross-batch partition semantics.
func exprContainsOver(node ExprNode) bool {
	if node == nil {
		return false
	}
	if _, ok := node.(*overNode); ok {
		return true
	}
	for _, c := range node.Children() {
		if exprContainsOver(c.node) {
			return true
		}
	}
	return false
}

// allBuiltInAggs reports whether every Aggregation is runnable through
// the streaming aggregate executor. That's true when either the
// aggregation uses a built-in Kind (no custom Fn) or the custom Fn
// implements IncrementalAggregator (per-batch Update + Finalize +
// Clone). Filtered aggregations still force the materializing path —
// per-agg filter masks need the whole row set to precompute against.
func allBuiltInAggs(aggs []Aggregation) bool {
	for _, a := range aggs {
		if a.Filter.node != nil {
			return false
		}
		if a.Fn == nil {
			continue
		}
		if _, ok := a.Fn.(IncrementalAggregator); !ok {
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

// fuseStreamChains walks the exec tree bottom-up and coalesces
// adjacent streaming batch-transform ops (filter / project /
// withColumn / drop / rename / explode) into a single
// fusedStreamExecOp. Cuts one batch↔Frame conversion cycle per fused
// op — meaningful on pipelines like
// `.WithColumn(...).WithColumn(...).Filter(...)` that would otherwise
// pay conversion overhead at each streaming boundary.
//
// Non-fusable ops (limit, materialize, scan, aggregate, join) are
// walked into but not folded — the fusion stops at their boundaries.
func fuseStreamChains(op ExecOperator) ExecOperator {
	if op == nil {
		return nil
	}

	// Recurse: fuse the subtree(s) first so we work bottom-up.
	switch e := op.(type) {
	case *filterExecOp:
		e.input = fuseStreamChains(e.input)
	case *projectExecOp:
		e.input = fuseStreamChains(e.input)
	case *withColumnExecOp:
		e.input = fuseStreamChains(e.input)
	case *dropExecOp:
		e.input = fuseStreamChains(e.input)
	case *renameExecOp:
		e.input = fuseStreamChains(e.input)
	case *explodeExecOp:
		e.input = fuseStreamChains(e.input)
	case *limitExecOp:
		e.input = fuseStreamChains(e.input)
	case *materializeExecOp:
		e.input = fuseStreamChains(e.input)
	case *fusedStreamExecOp:
		e.input = fuseStreamChains(e.input)
	case *streamingAggregateExec:
		e.input = fuseStreamChains(e.input)
	case *streamingJoinExec:
		e.left = fuseStreamChains(e.left)
		e.right = fuseStreamChains(e.right)
	case *sortMergeJoinExec:
		e.left = fuseStreamChains(e.left)
		e.right = fuseStreamChains(e.right)
	}

	// Try to fuse op with its input. Only frameApplier-implementing
	// ops can fuse — everything else is a boundary.
	applier, ok := op.(frameApplier)
	if !ok {
		return op
	}
	childInput := frameApplierChild(op)
	if childInput == nil {
		return op
	}

	// Case 1: child is already a fusedStreamExecOp — append this op
	// to its chain and rebind the output schema.
	if fused, ok := childInput.(*fusedStreamExecOp); ok {
		fused.ops = append(fused.ops, applier)
		fused.outSchema = op.Schema()
		return fused
	}

	// Case 2: child is another frameApplier — start a new chain of
	// two ops with the grandchild as the fused op's input.
	if childApplier, ok := childInput.(frameApplier); ok {
		grandchild := frameApplierChild(childInput)
		return &fusedStreamExecOp{
			input:     grandchild,
			ops:       []frameApplier{childApplier, applier},
			outSchema: op.Schema(),
		}
	}
	return op
}

// frameApplierChild returns the direct input of a frameApplier op, or
// nil if op isn't one of the recognized batch-transform types.
func frameApplierChild(op ExecOperator) ExecOperator {
	switch e := op.(type) {
	case *filterExecOp:
		return e.input
	case *projectExecOp:
		return e.input
	case *withColumnExecOp:
		return e.input
	case *dropExecOp:
		return e.input
	case *renameExecOp:
		return e.input
	case *explodeExecOp:
		return e.input
	}
	return nil
}

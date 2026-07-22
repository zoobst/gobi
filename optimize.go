package gobi

import "sort"

// Rule is a single plan-tree rewrite. A rule is a pure function from
// a plan to an equivalent plan, potentially cheaper to execute. The
// optimizer applies rules to a fixed point — no rule reports a change
// on the final pass.
//
// Rules should walk the whole tree themselves; the optimizer does not
// traverse for them. This lets a rule choose bottom-up or top-down
// order depending on what it does.
type Rule interface {
	Name() string
	Apply(plan LogicalPlan) (rewritten LogicalPlan, changed bool)
}

// DefaultRules returns the rule set applied by Optimize when no
// explicit list is passed. Order is a best-effort ordering that
// enables downstream folds — FoldConstants can turn a filter's
// condition into a literal, which RemoveTrivialTrueFilter then
// eliminates. The optimizer runs to a fixed point, so any correct
// interleaving eventually converges.
func DefaultRules() []Rule {
	return []Rule{
		&foldConstantsRule{},
		&removeTrivialFilterRule{},
		&combineFiltersRule{},
		&pushFilterBelowProjectRule{},
		&pushFilterBelowSortRule{},
		&projectionPushdownRule{},
		&pushPredicateToScanRule{},
		&cascadeEmptyRule{},
	}
}

// maxOptimizeIters is a safeguard: if a set of rules oscillates and
// never converges, the loop terminates with a partially-optimized
// plan rather than hanging. In practice the default rules stabilize
// in 2-3 iterations.
const maxOptimizeIters = 32

// Optimize applies rules to plan until either nothing changes or the
// iteration cap is hit. Returns the (possibly rewritten) root. If
// rules is empty, DefaultRules() is used.
func Optimize(plan LogicalPlan, rules ...Rule) LogicalPlan {
	if len(rules) == 0 {
		rules = DefaultRules()
	}
	for range maxOptimizeIters {
		anyChanged := false
		for _, r := range rules {
			next, changed := r.Apply(plan)
			if changed {
				plan = next
				anyChanged = true
			}
		}
		if !anyChanged {
			break
		}
	}
	return plan
}

// -----------------------------------------------------------------------------
// Plan traversal helpers
// -----------------------------------------------------------------------------

// mapExprs walks p and applies fn to every Expr field of every node,
// returning a plan structurally identical to p but with rewritten
// Exprs. Node types that carry no Exprs (Scan, Sort, Aggregate keys,
// Join keys, Drop, Limit, Tail) are traversed unchanged.
//
// The walk is bottom-up: children's Exprs get rewritten before the
// current node is rebuilt. Rebuilding is minimal — nodes whose
// Exprs didn't change are returned as-is (identity comparison on
// the Expr's underlying node pointer).
func mapExprs(p LogicalPlan, fn func(Expr) Expr) LogicalPlan {
	if p == nil {
		return nil
	}
	// Rewrite children first.
	var newInput, newRight LogicalPlan
	inputChanged := false
	switch n := p.(type) {
	case *filterNode:
		newInput = mapExprs(n.input, fn)
		inputChanged = newInput != n.input
		newCond := fn(n.cond)
		if !inputChanged && sameNode(newCond, n.cond) {
			return p
		}
		return &filterNode{input: newInput, cond: newCond}
	case *projectNode:
		newInput = mapExprs(n.input, fn)
		inputChanged = newInput != n.input
		newExprs := make([]Expr, len(n.exprs))
		exprsChanged := false
		for i, e := range n.exprs {
			ne := fn(e)
			newExprs[i] = ne
			if !sameNode(ne, e) {
				exprsChanged = true
			}
		}
		if !inputChanged && !exprsChanged {
			return p
		}
		return newProjectNode(newInput, newExprs)
	case *withColumnNode:
		newInput = mapExprs(n.input, fn)
		inputChanged = newInput != n.input
		newExpr := fn(n.expr)
		if !inputChanged && sameNode(newExpr, n.expr) {
			return p
		}
		return newWithColumnNode(newInput, n.name, newExpr)
	case *sortNode:
		newInput = mapExprs(n.input, fn)
		if newInput == n.input {
			return p
		}
		return &sortNode{input: newInput, keys: n.keys}
	case *aggregateNode:
		newInput = mapExprs(n.input, fn)
		if newInput == n.input {
			return p
		}
		return newAggregateNode(newInput, n.keys, n.aggs)
	case *joinNode:
		newInput = mapExprs(n.input, fn)
		newRight = mapExprs(n.right, fn)
		if newInput == n.input && newRight == n.right {
			return p
		}
		return newJoinNode(newInput, newRight, n.leftKey, n.rightKey, n.kind)
	case *limitNode:
		newInput = mapExprs(n.input, fn)
		if newInput == n.input {
			return p
		}
		return &limitNode{input: newInput, n: n.n}
	case *tailNode:
		newInput = mapExprs(n.input, fn)
		if newInput == n.input {
			return p
		}
		return &tailNode{input: newInput, n: n.n}
	case *dropNode:
		newInput = mapExprs(n.input, fn)
		if newInput == n.input {
			return p
		}
		return newDropNode(newInput, n.name)
	case *scanFrameNode, *scanFileNode:
		return p
	}
	return p
}

// sameNode reports whether a and b wrap the identical underlying
// ExprNode pointer — a cheap "did anything change" check that
// avoids deep equality.
func sameNode(a, b Expr) bool { return a.node == b.node }

// referencedColumns returns the set of column names an Expr reads.
// Used by rules that decide whether a predicate is safe to push
// through a schema-reshaping node.
func referencedColumns(e Expr) map[string]struct{} {
	out := make(map[string]struct{})
	collectRefs(e, out)
	return out
}

func collectRefs(e Expr, out map[string]struct{}) {
	if e.node == nil {
		return
	}
	if cr, ok := e.node.(*colRefNode); ok {
		out[cr.name] = struct{}{}
	}
	for _, c := range e.node.Children() {
		collectRefs(c, out)
	}
}

// -----------------------------------------------------------------------------
// Rule: FoldConstants
// -----------------------------------------------------------------------------

type foldConstantsRule struct{}

func (foldConstantsRule) Name() string { return "FoldConstants" }
func (r *foldConstantsRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	changed := false
	newP := mapExprs(p, func(e Expr) Expr {
		folded, did := foldExpr(e)
		if did {
			changed = true
			return folded
		}
		return e
	})
	return newP, changed
}

// foldExpr recursively simplifies constant subtrees. Bottom-up:
// children fold first, then the current node's local rules apply.
// Returns (rewritten, changed).
func foldExpr(e Expr) (Expr, bool) {
	if e.node == nil {
		return e, false
	}
	switch n := e.node.(type) {
	case *binOpNode:
		left, lc := foldExpr(Expr{node: n.left})
		right, rc := foldExpr(Expr{node: n.right})
		// Apply local folds against the (possibly-simplified) children.
		if folded, did := foldBinOp(n.op, left, right); did {
			return folded, true
		}
		if lc || rc {
			return binExpr(n.op, left, right), true
		}
		return e, false
	case *notNode:
		inner, ic := foldExpr(Expr{node: n.inner})
		// NOT NOT x → x
		if in, ok := inner.node.(*notNode); ok {
			return Expr{node: in.inner}, true
		}
		// NOT Lit(bool) → Lit(!bool)
		if lit, ok := inner.node.(*literalNode); ok && lit.err == nil {
			if b, ok := lit.value.(bool); ok {
				return Lit(!b), true
			}
		}
		if ic {
			return inner.Not(), true
		}
		return e, false
	case *aliasNode:
		inner, ic := foldExpr(Expr{node: n.inner})
		if ic {
			return inner.Alias(n.name), true
		}
		return e, false
	}
	return e, false
}

// foldBinOp handles algebraic simplifications on op(left, right) when
// one or both sides is a literal. Returns (result, true) on a
// successful fold, (_, false) otherwise.
func foldBinOp(op binOpKind, left, right Expr) (Expr, bool) {
	lLit, lIsLit := literalOf(left)
	rLit, rIsLit := literalOf(right)

	// Boolean identity / absorption laws.
	switch op {
	case bopAnd:
		if lIsLit {
			if b, ok := lLit.value.(bool); ok {
				if b {
					return right, true // true AND x → x
				}
				return Lit(false), true // false AND x → false
			}
		}
		if rIsLit {
			if b, ok := rLit.value.(bool); ok {
				if b {
					return left, true // x AND true → x
				}
				return Lit(false), true // x AND false → false
			}
		}
	case bopOr:
		if lIsLit {
			if b, ok := lLit.value.(bool); ok {
				if b {
					return Lit(true), true // true OR x → true
				}
				return right, true // false OR x → x
			}
		}
		if rIsLit {
			if b, ok := rLit.value.(bool); ok {
				if b {
					return Lit(true), true // x OR true → true
				}
				return left, true // x OR false → x
			}
		}
	}

	// Both literals: try to fold to a single literal.
	if lIsLit && rIsLit {
		return foldLiteralBinOp(op, lLit, rLit)
	}
	return Expr{}, false
}

// foldLiteralBinOp evaluates op(l, r) when both operands are literals
// with concrete Go values. Falls back to (_, false) for combinations
// we don't fold (e.g. mixed types, division by zero).
func foldLiteralBinOp(op binOpKind, l, r *literalNode) (Expr, bool) {
	if l.err != nil || r.err != nil {
		return Expr{}, false
	}
	// Numeric ops via float64 coercion.
	if lf, ok := l.asFloat64(); ok {
		if rf, ok := r.asFloat64(); ok {
			switch op {
			case bopAdd:
				return Lit(lf + rf), true
			case bopSub:
				return Lit(lf - rf), true
			case bopMul:
				return Lit(lf * rf), true
			case bopDiv:
				if rf == 0 {
					return Expr{}, false
				}
				return Lit(lf / rf), true
			case bopEq:
				return Lit(lf == rf), true
			case bopNe:
				return Lit(lf != rf), true
			case bopLt:
				return Lit(lf < rf), true
			case bopLe:
				return Lit(lf <= rf), true
			case bopGt:
				return Lit(lf > rf), true
			case bopGe:
				return Lit(lf >= rf), true
			}
		}
	}
	// String equality.
	if ls, ok := l.value.(string); ok {
		if rs, ok := r.value.(string); ok {
			switch op {
			case bopEq:
				return Lit(ls == rs), true
			case bopNe:
				return Lit(ls != rs), true
			}
		}
	}
	// Boolean ops on two literals.
	if lb, ok := l.value.(bool); ok {
		if rb, ok := r.value.(bool); ok {
			switch op {
			case bopAnd:
				return Lit(lb && rb), true
			case bopOr:
				return Lit(lb || rb), true
			case bopEq:
				return Lit(lb == rb), true
			case bopNe:
				return Lit(lb != rb), true
			}
		}
	}
	return Expr{}, false
}

// literalOf returns e.node cast to *literalNode, or (nil, false) if
// e isn't a literal.
func literalOf(e Expr) (*literalNode, bool) {
	if e.node == nil {
		return nil, false
	}
	lit, ok := e.node.(*literalNode)
	return lit, ok
}

// -----------------------------------------------------------------------------
// Rule: RemoveTrivialFilter
//
//   Filter(x, Lit(true))   →  x
//   Filter(x, Lit(false))  →  Empty(schema=x.Schema())
//
// The false case collapses to an emptyNode so downstream operators
// (Sort, Aggregate, etc.) skip work entirely. Schema is preserved so
// Collect returns a well-formed zero-row Frame.
// -----------------------------------------------------------------------------

type removeTrivialFilterRule struct{}

func (removeTrivialFilterRule) Name() string { return "RemoveTrivialFilter" }
func (r *removeTrivialFilterRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	return walkRewrite(p, func(node LogicalPlan) (LogicalPlan, bool) {
		f, ok := node.(*filterNode)
		if !ok {
			return node, false
		}
		lit, ok := f.cond.node.(*literalNode)
		if !ok || lit.err != nil {
			return node, false
		}
		b, ok := lit.value.(bool)
		if !ok {
			return node, false
		}
		if b {
			return f.input, true
		}
		return &emptyNode{schema: f.input.Schema()}, true
	})
}

// -----------------------------------------------------------------------------
// Rule: CombineFilters
//
// Filter(Filter(x, p1), p2)  →  Filter(x, p2 AND p1)
//
// Two linear scans collapse to one. Note the argument order: the
// outer filter's condition (p2) comes first in the And so it stays
// short-circuit-friendly for future evaluators. AND commutes for
// correctness.
// -----------------------------------------------------------------------------

type combineFiltersRule struct{}

func (combineFiltersRule) Name() string { return "CombineFilters" }
func (r *combineFiltersRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	return walkRewrite(p, func(node LogicalPlan) (LogicalPlan, bool) {
		outer, ok := node.(*filterNode)
		if !ok {
			return node, false
		}
		inner, ok := outer.input.(*filterNode)
		if !ok {
			return node, false
		}
		combined := outer.cond.And(inner.cond)
		return &filterNode{input: inner.input, cond: combined}, true
	})
}

// -----------------------------------------------------------------------------
// Rule: PushFilterBelowProject
//
// Filter(Project(x, exprs), pred)  →  Project(Filter(x, pred), exprs)
//
// SAFE when pred references only columns present in x's schema (i.e.
// columns the Project reads from — not columns it constructs via
// expressions). If pred references a Project-created column, we
// leave the filter above; a subtler rewrite could substitute the
// expression, but that's a later rule.
//
// The win: Filter runs on fewer rows before Project computes its
// (potentially expensive) output expressions.
// -----------------------------------------------------------------------------

type pushFilterBelowProjectRule struct{}

func (pushFilterBelowProjectRule) Name() string { return "PushFilterBelowProject" }
func (r *pushFilterBelowProjectRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	return walkRewrite(p, func(node LogicalPlan) (LogicalPlan, bool) {
		f, ok := node.(*filterNode)
		if !ok {
			return node, false
		}
		proj, ok := f.input.(*projectNode)
		if !ok {
			return node, false
		}
		// Which columns does the predicate reference?
		refs := referencedColumns(f.cond)
		// The Project's INPUT schema — that's where columns must live
		// for the predicate to be evaluable below the Project.
		inSchema := proj.input.Schema()
		inNames := make(map[string]struct{}, len(inSchema.Fields()))
		for _, fld := range inSchema.Fields() {
			inNames[fld.Name] = struct{}{}
		}
		for name := range refs {
			if _, ok := inNames[name]; !ok {
				// Predicate references something not in Project's input
				// (probably a Project-computed column). Leave as-is.
				return node, false
			}
		}
		// Safe to push. Rewrite: Filter(Project(x, exprs), pred)
		//                     → Project(Filter(x, pred), exprs)
		newFilter := &filterNode{input: proj.input, cond: f.cond}
		newProject := newProjectNode(newFilter, proj.exprs)
		return newProject, true
	})
}

// -----------------------------------------------------------------------------
// Rule: PushFilterBelowSort
//
//   Filter(Sort(x, keys), pred)  →  Sort(Filter(x, pred), keys)
//
// Always safe — filter row set doesn't depend on sort order, and
// sort of the filtered rows produces the same output either way.
// The win: Sort runs on a smaller set of rows. Bigger the filter's
// selectivity, bigger the payoff (sort is n log n; filter is n).
// -----------------------------------------------------------------------------

type pushFilterBelowSortRule struct{}

func (pushFilterBelowSortRule) Name() string { return "PushFilterBelowSort" }
func (r *pushFilterBelowSortRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	return walkRewrite(p, func(node LogicalPlan) (LogicalPlan, bool) {
		f, ok := node.(*filterNode)
		if !ok {
			return node, false
		}
		s, ok := f.input.(*sortNode)
		if !ok {
			return node, false
		}
		// Filter goes under; Sort goes over.
		newFilter := &filterNode{input: s.input, cond: f.cond}
		return &sortNode{input: newFilter, keys: s.keys}, true
	})
}

// -----------------------------------------------------------------------------
// Rule: ProjectionPushdown
//
// Top-down walk that computes "columns needed" at each level and
// pushes the tightest set down to any ProjectableScan leaf. Handles
// the schema-shaping operators (Filter, Project, WithColumn, Sort,
// Aggregate, Drop, Limit, Tail); joins are deliberately skipped for
// now — left/right column attribution requires a separate walker.
//
// Scans that don't implement ProjectableScan are left untouched.
// The projectFn provided by the source package decides whether to
// honor the projection (parquetio, for instance, intersects with
// any user-supplied Options.Columns).
// -----------------------------------------------------------------------------

type projectionPushdownRule struct{}

func (projectionPushdownRule) Name() string { return "ProjectionPushdown" }
func (r *projectionPushdownRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	// Compute the root's output columns; everything the plan produces
	// starts as "needed" — the caller wants the whole output schema.
	rootSchema := p.Schema()
	need := make(map[string]struct{}, len(rootSchema.Fields()))
	for _, f := range rootSchema.Fields() {
		need[f.Name] = struct{}{}
	}
	return pushProjection(p, need)
}

// pushProjection walks p top-down. neededOut is the set of columns
// this node's parent needs to see in the output. The function
// computes what THIS node needs from its child(ren) and recurses.
// Returns the (potentially rewritten) plan and whether anything
// changed.
func pushProjection(p LogicalPlan, neededOut map[string]struct{}) (LogicalPlan, bool) {
	switch n := p.(type) {
	case ProjectableScan:
		if len(neededOut) == 0 {
			return p, false
		}
		cols := setToSortedSlice(neededOut)
		// Intersect with the scan's declared schema so we don't ask
		// for columns the source doesn't have.
		if sch := n.Schema(); sch != nil && len(sch.Fields()) > 0 {
			have := make(map[string]struct{}, len(sch.Fields()))
			for _, f := range sch.Fields() {
				have[f.Name] = struct{}{}
			}
			filtered := cols[:0]
			for _, c := range cols {
				if _, ok := have[c]; ok {
					filtered = append(filtered, c)
				}
			}
			cols = filtered
		}
		if len(cols) == 0 {
			return p, false
		}
		newScan := n.ProjectColumns(cols)
		return newScan, !samePlanIdentity(newScan, p)

	case *filterNode:
		child := unionColSet(neededOut, referencedColumns(n.cond))
		newIn, changed := pushProjection(n.input, child)
		if !changed {
			return p, false
		}
		return &filterNode{input: newIn, cond: n.cond}, true

	case *projectNode:
		child := make(map[string]struct{})
		for _, e := range n.exprs {
			for c := range referencedColumns(e) {
				child[c] = struct{}{}
			}
		}
		newIn, changed := pushProjection(n.input, child)
		if !changed {
			return p, false
		}
		return newProjectNode(newIn, n.exprs), true

	case *withColumnNode:
		child := copyColSet(neededOut)
		delete(child, n.name) // WithColumn produces n.name from expr; child needn't supply it.
		for c := range referencedColumns(n.expr) {
			child[c] = struct{}{}
		}
		newIn, changed := pushProjection(n.input, child)
		if !changed {
			return p, false
		}
		return newWithColumnNode(newIn, n.name, n.expr), true

	case *sortNode:
		child := copyColSet(neededOut)
		for _, k := range n.keys {
			child[k.Column] = struct{}{}
		}
		newIn, changed := pushProjection(n.input, child)
		if !changed {
			return p, false
		}
		return &sortNode{input: newIn, keys: n.keys}, true

	case *aggregateNode:
		// Aggregate reshapes: input needs group keys + agg source cols.
		child := make(map[string]struct{})
		for _, k := range n.keys {
			child[k] = struct{}{}
		}
		for _, a := range n.aggs {
			if a.Column != "" {
				child[a.Column] = struct{}{}
			}
		}
		newIn, changed := pushProjection(n.input, child)
		if !changed {
			return p, false
		}
		return newAggregateNode(newIn, n.keys, n.aggs), true

	case *dropNode:
		// Drop needs its target column present in the input to remove it,
		// but downstream never sees it — so child needs neededOut plus n.name.
		child := copyColSet(neededOut)
		child[n.name] = struct{}{}
		newIn, changed := pushProjection(n.input, child)
		if !changed {
			return p, false
		}
		return newDropNode(newIn, n.name), true

	case *limitNode:
		newIn, changed := pushProjection(n.input, neededOut)
		if !changed {
			return p, false
		}
		return &limitNode{input: newIn, n: n.n}, true

	case *tailNode:
		newIn, changed := pushProjection(n.input, neededOut)
		if !changed {
			return p, false
		}
		return &tailNode{input: newIn, n: n.n}, true

	case *joinNode:
		// Deliberately don't push through joins — left/right column
		// attribution is more involved. See the Layer 4 followup.
		return p, false

	case *scanFrameNode, *emptyNode:
		// No projection support on these leaf types (scanFrameNode
		// holds an in-memory Frame; emptyNode is a constant).
		return p, false
	}
	return p, false
}

// samePlanIdentity reports whether a and b are the same LogicalPlan
// pointer. Used by rules that treat "returned self" as "no change."
func samePlanIdentity(a, b LogicalPlan) bool { return a == b }

func copyColSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func unionColSet(a, b map[string]struct{}) map[string]struct{} {
	out := copyColSet(a)
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

func setToSortedSlice(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// Rule: PushPredicateToScan
//
//   Filter(ScanFile(path), pred)  →  Filter(ScanFile(path, pred), pred)
//
// The scan gets the predicate as a hint (via PredicateSink) so it can
// skip whole row-groups whose statistics prove no row could match.
// The Filter above is intentionally kept: row-group skipping is
// coarse (min/max bounds only), so the row-level Filter continues to
// do exact filtering on surviving rows. Belt-and-suspenders.
//
// Fires only when the input to Filter directly implements
// PredicateSink — walks are shallow. If a Sort or Project sits
// between Filter and Scan, other rules (PushFilterBelowSort,
// PushFilterBelowProject) reorder first, then this rule fires on the
// next optimizer pass.
// -----------------------------------------------------------------------------

type pushPredicateToScanRule struct{}

func (pushPredicateToScanRule) Name() string { return "PushPredicateToScan" }
func (r *pushPredicateToScanRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	return walkRewrite(p, func(node LogicalPlan) (LogicalPlan, bool) {
		f, ok := node.(*filterNode)
		if !ok {
			return node, false
		}
		sink, ok := f.input.(PredicateSink)
		if !ok {
			return node, false
		}
		newScan := sink.ApplyPredicate(f.cond)
		if samePlanIdentity(newScan, sink) {
			return node, false // sink declined the hint
		}
		return &filterNode{input: newScan, cond: f.cond}, true
	})
}

// -----------------------------------------------------------------------------
// Rule: CascadeEmpty
//
// Propagate emptyNode upward through operators that preserve
// emptiness:
//
//   Filter(Empty, _)          → Empty  (same schema)
//   Sort(Empty, _)            → Empty  (same schema)
//   Limit(Empty, _)           → Empty  (same schema)
//   Tail(Empty, _)            → Empty  (same schema)
//   Drop(Empty, name)         → Empty  (schema minus name — via newDropNode)
//   Project(Empty, exprs)     → Empty  (project schema)
//   WithColumn(Empty, ...)    → Empty  (with-column schema)
//   Aggregate(Empty, ...)     → Empty  (aggregate schema; groupings on
//                                        no rows always produce zero output rows)
//   Join(Empty, _, Inner)     → Empty
//   Join(_, Empty, Inner)     → Empty
//   Join(Empty, _, Semi)      → Empty
//
// Left/Right/Full joins preserve the outer side even when the other
// is empty, so those are left alone.
//
// The unary cases reuse each node's constructor to get the same
// output schema they'd have with real data underneath — Schema()
// keeps propagating truthfully after the rewrite.
// -----------------------------------------------------------------------------

type cascadeEmptyRule struct{}

func (cascadeEmptyRule) Name() string { return "CascadeEmpty" }
func (r *cascadeEmptyRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	return walkRewrite(p, func(node LogicalPlan) (LogicalPlan, bool) {
		switch n := node.(type) {
		case *filterNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *sortNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *limitNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *tailNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *dropNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *projectNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *withColumnNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *aggregateNode:
			if _, ok := n.input.(*emptyNode); ok {
				return &emptyNode{schema: n.Schema()}, true
			}
		case *joinNode:
			_, leftEmpty := n.input.(*emptyNode)
			_, rightEmpty := n.right.(*emptyNode)
			if !leftEmpty && !rightEmpty {
				return node, false
			}
			switch n.kind {
			case JoinInner:
				if leftEmpty || rightEmpty {
					return &emptyNode{schema: n.Schema()}, true
				}
			case JoinSemi, JoinAnti:
				// Semi: no left rows → no output. Right-empty on
				// semi means "no matches possible" → empty.
				// Anti: left-empty → no rows to keep.
				if leftEmpty {
					return &emptyNode{schema: n.Schema()}, true
				}
				if rightEmpty && n.kind == JoinSemi {
					return &emptyNode{schema: n.Schema()}, true
				}
			}
		}
		return node, false
	})
}

// -----------------------------------------------------------------------------
// walkRewrite is a generic bottom-up walker that applies `visit` to
// every node in the tree and returns the (potentially rewritten)
// root. Handles branching (Join has two children) and preserves
// unchanged subtrees by identity.
// -----------------------------------------------------------------------------

func walkRewrite(p LogicalPlan, visit func(LogicalPlan) (LogicalPlan, bool)) (LogicalPlan, bool) {
	if p == nil {
		return nil, false
	}
	changed := false

	rewriteChild := func(c LogicalPlan) LogicalPlan {
		nc, did := walkRewrite(c, visit)
		if did {
			changed = true
		}
		return nc
	}

	var rebuilt LogicalPlan
	switch n := p.(type) {
	case *filterNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = &filterNode{input: newIn, cond: n.cond}
		}
	case *projectNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = newProjectNode(newIn, n.exprs)
		}
	case *withColumnNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = newWithColumnNode(newIn, n.name, n.expr)
		}
	case *sortNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = &sortNode{input: newIn, keys: n.keys}
		}
	case *aggregateNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = newAggregateNode(newIn, n.keys, n.aggs)
		}
	case *joinNode:
		newIn := rewriteChild(n.input)
		newRt := rewriteChild(n.right)
		if newIn != n.input || newRt != n.right {
			rebuilt = newJoinNode(newIn, newRt, n.leftKey, n.rightKey, n.kind)
		}
	case *limitNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = &limitNode{input: newIn, n: n.n}
		}
	case *tailNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = &tailNode{input: newIn, n: n.n}
		}
	case *dropNode:
		newIn := rewriteChild(n.input)
		if newIn != n.input {
			rebuilt = newDropNode(newIn, n.name)
		}
	}
	if rebuilt == nil {
		rebuilt = p
	}
	// Apply the visitor at this level.
	after, did := visit(rebuilt)
	if did {
		changed = true
	}
	return after, changed
}

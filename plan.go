package gobi

import (
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
)

// LogicalPlan represents a node in a query plan tree. Plans are
// immutable data structures — building a plan does not execute
// anything. LazyFrame consumes a LogicalPlan and executes it via
// Collect().
//
// A future optimizer will walk plan trees, rewrite them, and
// translate them to a physical plan for execution. Slice 1 (this
// file) provides the tree shape and node types; Collect() dispatches
// each node to the existing eager engine.
type LogicalPlan interface {
	// Schema returns the output schema of this node. Called during
	// LazyFrame.Schema() and by nodes that need to inspect their
	// input's shape (e.g. Project computing its output columns).
	Schema() *arrow.Schema

	// Children returns the immediate sub-plans, in evaluation order
	// (empty for leaves). Used by tree walkers, optimizers, and
	// Explain().
	Children() []LogicalPlan

	// String returns a single-line description of this node —
	// no children, no formatting. Used as the label in Explain
	// output; the tree walker handles indentation and recursion.
	String() string
}

// Namer is implemented by expression nodes that carry a meaningful
// output column name. Select and WithColumn use it to derive default
// column names (Col("id") → "id", Col(x).Alias("y") → "y").
//
// Nodes that don't implement Namer, or whose OutputName is empty,
// fall back to a positional default ("expr_0", "expr_1", ...).
type Namer interface {
	OutputName() string
}

// -----------------------------------------------------------------------------
// scanFrameNode: leaf that wraps an in-memory Frame
// -----------------------------------------------------------------------------

type scanFrameNode struct {
	frame *Frame
}

func (n *scanFrameNode) Schema() *arrow.Schema   { return n.frame.Schema() }
func (n *scanFrameNode) Children() []LogicalPlan { return nil }
func (n *scanFrameNode) String() string {
	rows, cols := n.frame.Shape()
	return fmt.Sprintf("Scan[frame](%d rows x %d cols)", rows, cols)
}

// -----------------------------------------------------------------------------
// filterNode: keep rows where cond evaluates true
// -----------------------------------------------------------------------------

type filterNode struct {
	input LogicalPlan
	cond  Expr
}

func (n *filterNode) Schema() *arrow.Schema   { return n.input.Schema() }
func (n *filterNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *filterNode) String() string          { return fmt.Sprintf("Filter(%s)", n.cond) }

// -----------------------------------------------------------------------------
// projectNode: reshape output schema to a set of expressions
// -----------------------------------------------------------------------------

type projectNode struct {
	input     LogicalPlan
	exprs     []Expr
	outSchema *arrow.Schema
}

// newProjectNode computes the output schema eagerly so LazyFrame.Schema()
// can report it without evaluation. Type-inference failures on an
// individual Expr produce a nil-typed field in the output schema —
// Collect() will surface the real error when it evaluates the Expr
// against a Frame.
func newProjectNode(input LogicalPlan, exprs []Expr) *projectNode {
	inSchema := input.Schema()
	fields := make([]arrow.Field, len(exprs))
	for i, e := range exprs {
		var dt arrow.DataType
		if e.node != nil {
			dt, _ = e.node.Type(inSchema)
		}
		fields[i] = arrow.Field{
			Name:     exprOutputName(e, i),
			Type:     dt,
			Nullable: true,
		}
	}
	return &projectNode{
		input:     input,
		exprs:     exprs,
		outSchema: arrow.NewSchema(fields, schemaMetadataPtr(inSchema)),
	}
}

func (n *projectNode) Schema() *arrow.Schema   { return n.outSchema }
func (n *projectNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *projectNode) String() string {
	parts := make([]string, len(n.exprs))
	for i, e := range n.exprs {
		parts[i] = e.String()
	}
	return fmt.Sprintf("Project(%s)", strings.Join(parts, ", "))
}

// -----------------------------------------------------------------------------
// withColumnNode: input schema + one appended or replaced column
// -----------------------------------------------------------------------------

type withColumnNode struct {
	input     LogicalPlan
	name      string
	expr      Expr
	outSchema *arrow.Schema
}

func newWithColumnNode(input LogicalPlan, name string, e Expr) *withColumnNode {
	inSchema := input.Schema()
	var dt arrow.DataType
	if e.node != nil {
		dt, _ = e.node.Type(inSchema)
	}
	newField := arrow.Field{Name: name, Type: dt, Nullable: true}

	replaceIdx := -1
	for i, f := range inSchema.Fields() {
		if f.Name == name {
			replaceIdx = i
			break
		}
	}
	var newFields []arrow.Field
	if replaceIdx >= 0 {
		newFields = append([]arrow.Field{}, inSchema.Fields()...)
		newFields[replaceIdx] = newField
	} else {
		newFields = append(append([]arrow.Field{}, inSchema.Fields()...), newField)
	}

	return &withColumnNode{
		input:     input,
		name:      name,
		expr:      e,
		outSchema: arrow.NewSchema(newFields, schemaMetadataPtr(inSchema)),
	}
}

func (n *withColumnNode) Schema() *arrow.Schema   { return n.outSchema }
func (n *withColumnNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *withColumnNode) String() string {
	return fmt.Sprintf("WithColumn(%q = %s)", n.name, n.expr)
}

// -----------------------------------------------------------------------------
// limitNode: keep first n rows
// -----------------------------------------------------------------------------

type limitNode struct {
	input LogicalPlan
	n     int
}

func (l *limitNode) Schema() *arrow.Schema   { return l.input.Schema() }
func (l *limitNode) Children() []LogicalPlan { return []LogicalPlan{l.input} }
func (l *limitNode) String() string          { return fmt.Sprintf("Limit(%d)", l.n) }

// -----------------------------------------------------------------------------
// sortNode: reorder rows by one or more SortKeys
// -----------------------------------------------------------------------------

type sortNode struct {
	input LogicalPlan
	keys  []SortKey
}

func (n *sortNode) Schema() *arrow.Schema   { return n.input.Schema() }
func (n *sortNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *sortNode) String() string {
	parts := make([]string, len(n.keys))
	for i, k := range n.keys {
		if k.Descending {
			parts[i] = fmt.Sprintf("%s DESC", k.Column)
		} else {
			parts[i] = k.Column
		}
	}
	return fmt.Sprintf("Sort(%s)", strings.Join(parts, ", "))
}

// -----------------------------------------------------------------------------
// aggregateNode: GroupBy + Agg baked into one plan node
// -----------------------------------------------------------------------------

type aggregateNode struct {
	input     LogicalPlan
	keys      []string
	aggs      []Aggregation
	outSchema *arrow.Schema
}

func newAggregateNode(input LogicalPlan, keys []string, aggs []Aggregation) *aggregateNode {
	inSchema := input.Schema()
	fields := make([]arrow.Field, 0, len(keys)+len(aggs))

	// Key columns come out non-null (GroupBy filters out null-keyed rows).
	// Use the input field's type; if the key is unknown, leave Type nil so
	// Collect() surfaces the real error.
	for _, k := range keys {
		if fieldsMatching, ok := inSchema.FieldsByName(k); ok && len(fieldsMatching) > 0 {
			f := fieldsMatching[0]
			f.Nullable = false
			fields = append(fields, f)
		} else {
			fields = append(fields, arrow.Field{Name: k})
		}
	}

	// Agg output columns. Count / NUnique → Int64 non-null;
	// First / Last preserve the source column's arrow type; custom
	// Aggregator uses its declared Type; everything else → Float64.
	for _, a := range aggs {
		t := aggOutputType(a)
		if a.Fn == nil && (a.Kind == AggFirst || a.Kind == AggLast) {
			if fm, ok := inSchema.FieldsByName(a.Column); ok && len(fm) > 0 {
				t = fm[0].Type
			}
		}
		fields = append(fields, arrow.Field{
			Name:     aggName(a),
			Type:     t,
			Nullable: aggOutputNullable(a),
		})
	}

	return &aggregateNode{
		input:     input,
		keys:      keys,
		aggs:      aggs,
		outSchema: arrow.NewSchema(fields, schemaMetadataPtr(inSchema)),
	}
}

func (n *aggregateNode) Schema() *arrow.Schema   { return n.outSchema }
func (n *aggregateNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *aggregateNode) String() string {
	aggParts := make([]string, len(n.aggs))
	for i, a := range n.aggs {
		aggParts[i] = aggName(a)
	}
	return fmt.Sprintf("Aggregate(keys=[%s], aggs=[%s])",
		strings.Join(n.keys, ", "), strings.Join(aggParts, ", "))
}

// aggOutputType picks the arrow type Aggregate will produce for one
// Aggregation. Kept aligned with the eager engine's builder-picking
// logic in groupby.go — if the two diverge, Schema() will lie.
//
// For First/Last the output type depends on the source column, so the
// LazyFrame builder (newAggregateNode) special-cases those and doesn't
// call this — it copies the source column's type directly. When this
// is called for First/Last (e.g. from ExplainPhysical without a
// resolved input schema) it falls back to Float64 as the safe scalar
// default.
func aggOutputType(a Aggregation) arrow.DataType {
	if a.Fn != nil {
		return a.Fn.Type()
	}
	switch a.Kind {
	case AggCount, AggNUnique:
		return arrow.PrimitiveTypes.Int64
	}
	return arrow.PrimitiveTypes.Float64
}

// aggOutputNullable reports whether the aggregation's output column
// may contain nulls. Count and NUnique are always non-null (empty
// group counts as 0); everything else may emit null on empty groups.
func aggOutputNullable(a Aggregation) bool {
	if a.Fn != nil {
		return true
	}
	switch a.Kind {
	case AggCount, AggNUnique:
		return false
	}
	return true
}

// -----------------------------------------------------------------------------
// joinNode: combine two plans on a key
// -----------------------------------------------------------------------------

type joinNode struct {
	input     LogicalPlan
	right     LogicalPlan
	leftKey   string
	rightKey  string
	kind      JoinType
	outSchema *arrow.Schema
}

func newJoinNode(left, right LogicalPlan, leftKey, rightKey string, kind JoinType) *joinNode {
	lSchema := left.Schema()
	rSchema := right.Schema()
	outSchema := buildJoinSchema(lSchema, rSchema, rightKey, kind)
	return &joinNode{
		input:     left,
		right:     right,
		leftKey:   leftKey,
		rightKey:  rightKey,
		kind:      kind,
		outSchema: outSchema,
	}
}

func (n *joinNode) Schema() *arrow.Schema   { return n.outSchema }
func (n *joinNode) Children() []LogicalPlan { return []LogicalPlan{n.input, n.right} }
func (n *joinNode) String() string {
	return fmt.Sprintf("Join(%s, left.%s = right.%s)", joinKindLabel(n.kind), n.leftKey, n.rightKey)
}

// buildJoinSchema mirrors Frame.Join's output construction: left
// fields first, then right fields except the right join key, with
// _right suffix on collisions. Semi/Anti drop the right side
// entirely.
//
// The left join key is always retained in the output — Frame.Join
// coalesces it against the right side's key values for Right/Full
// joins — so this function only needs rightKey to know what to
// exclude from the right side.
func buildJoinSchema(lSchema, rSchema *arrow.Schema, rightKey string, kind JoinType) *arrow.Schema {
	leftFields := lSchema.Fields()
	leftNames := make(map[string]struct{}, len(leftFields))
	for _, f := range leftFields {
		leftNames[f.Name] = struct{}{}
	}

	out := make([]arrow.Field, 0, len(leftFields)+len(rSchema.Fields()))
	out = append(out, leftFields...)

	if kind == JoinSemi || kind == JoinAnti {
		return arrow.NewSchema(out, schemaMetadataPtr(lSchema))
	}
	for _, f := range rSchema.Fields() {
		if f.Name == rightKey {
			continue
		}
		if _, clash := leftNames[f.Name]; clash {
			f.Name = f.Name + "_right"
		}
		out = append(out, f)
	}
	return arrow.NewSchema(out, schemaMetadataPtr(lSchema))
}

// joinKindLabel returns a short human-readable label for use in
// String() output. Not exposed via JoinType.String because JoinType is
// a raw uint8 with no existing String method.
func joinKindLabel(k JoinType) string {
	switch k {
	case JoinInner:
		return "inner"
	case JoinLeft:
		return "left"
	case JoinRight:
		return "right"
	case JoinFull:
		return "full"
	case JoinSemi:
		return "semi"
	case JoinAnti:
		return "anti"
	}
	return fmt.Sprintf("kind=%d", k)
}

// -----------------------------------------------------------------------------
// dropNode: input schema minus one column
// -----------------------------------------------------------------------------

type dropNode struct {
	input     LogicalPlan
	name      string
	outSchema *arrow.Schema
}

func newDropNode(input LogicalPlan, name string) *dropNode {
	inSchema := input.Schema()
	// If the column is missing at plan-build time we still construct
	// the node; the real error surfaces at Collect via Frame.DropColumn.
	fields := inSchema.Fields()
	keep := make([]arrow.Field, 0, len(fields))
	for _, f := range fields {
		if f.Name == name {
			continue
		}
		keep = append(keep, f)
	}
	return &dropNode{
		input:     input,
		name:      name,
		outSchema: arrow.NewSchema(keep, schemaMetadataPtr(inSchema)),
	}
}

func (n *dropNode) Schema() *arrow.Schema   { return n.outSchema }
func (n *dropNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *dropNode) String() string          { return fmt.Sprintf("Drop(%q)", n.name) }

// -----------------------------------------------------------------------------
// tailNode: keep last n rows. Schema unchanged; row count resolved at
// Collect() since intermediate plan stages don't know their row count.
// -----------------------------------------------------------------------------

type tailNode struct {
	input LogicalPlan
	n     int
}

func (n *tailNode) Schema() *arrow.Schema   { return n.input.Schema() }
func (n *tailNode) Children() []LogicalPlan { return []LogicalPlan{n.input} }
func (n *tailNode) String() string          { return fmt.Sprintf("Tail(%d)", n.n) }

// -----------------------------------------------------------------------------
// scanFileNode: deferred I/O source, format-agnostic via a closure.
//
// The scan node captures a read function that materializes a *Frame
// on demand (called by Collect). This shape lets parquetio, csvio,
// and future format packages contribute scan sources without adding
// a per-format node type here — they just call NewScanNode with the
// right label and closure.
//
// The schema is stored eagerly at construction (parquetio reads the
// parquet footer; csvio derives it from a Go struct type). If the
// scan source can't determine its schema up front, it passes nil and
// the read closure surfaces the real error at Collect.
// -----------------------------------------------------------------------------

type scanFileNode struct {
	label          string
	schema         *arrow.Schema
	read           func() (*Frame, error)
	streamRead     func(cb func(*Frame) error) error       // nil = falls back to `read`
	parallelStream func() []func(cb func(*Frame) error) error // nil = falls back to `streamRead`
	projectFn      func(cols []string) LogicalPlan         // nil = no projection support
	predicateFn    func(pred Expr) LogicalPlan             // nil = no predicate pushdown
}

// ScanOption configures optional capabilities on a scan node.
// Source packages (parquetio, csvio, ...) pass these to NewScanNode
// to declare features the optimizer can exploit — currently just
// column-projection pushdown.
type ScanOption func(*scanFileNode)

// WithColumnProjection registers a callback that returns a new scan
// node projected to the given column names. The optimizer's
// projection-pushdown rule calls this when it determines a scan
// source is being asked for a subset of its columns.
//
// The callback should:
//   - Return nil if projection doesn't apply (e.g. the caller
//     already restricted columns explicitly). Treated as "no
//     change" — the receiver stays in the plan.
//   - Otherwise return a fresh scan node with the projection
//     applied — the optimizer replaces the old node with what
//     this callback returns.
//
// cols is guaranteed non-empty and sorted; entries outside the scan's
// schema are the callback's responsibility to filter out.
func WithColumnProjection(fn func(cols []string) LogicalPlan) ScanOption {
	return func(n *scanFileNode) { n.projectFn = fn }
}

// WithPredicatePushdown registers a callback that returns a new scan
// node with a predicate baked in. The optimizer's PushPredicateToScan
// rule calls this when a Filter sits directly above the scan; the
// scan source is expected to use the predicate for row-group /
// bloom-filter skipping at read time.
//
// The Filter node above the scan is NOT removed after pushdown —
// row-group skipping is coarse (whole groups only), so the row-level
// Filter still runs for correctness. Callbacks should treat the
// predicate as a hint, not a guarantee.
//
// nil return = "no change" (same convention as WithColumnProjection).
func WithPredicatePushdown(fn func(pred Expr) LogicalPlan) ScanOption {
	return func(n *scanFileNode) { n.predicateFn = fn }
}

// WithStreamRead registers a streaming callback API for scan sources
// that support incremental read (parquetio.ReadFileChunksFunc,
// csvio.ReadFileChunksFunc). fn should call cb once per record batch
// and honor the batch-lifetime contract of the source package
// (typically: batches are Released after cb returns).
//
// When present, the Layer 6 executor uses this callback for true
// bounded-memory streaming through the plan. When absent, the
// executor falls back to the `read` closure (whole-file materialize
// then batch — correct but not memory-bounded).
func WithStreamRead(fn func(cb func(*Frame) error) error) ScanOption {
	return func(n *scanFileNode) { n.streamRead = fn }
}

// WithParallelStreamReads registers a callback that returns N
// stream-read closures, each processing a disjoint portion of the
// source. The Layer 6 executor spawns one producer goroutine per
// closure and fan-ins their batches into the downstream pipeline.
//
// The callback is responsible for deciding N (typically bounded by
// GOMAXPROCS and by the source's natural partition count — e.g.
// number of parquet row-groups). Return nil (or a slice of length
// 0-1) to signal "no parallel version available"; the executor
// falls back to WithStreamRead in that case.
//
// Semantics: batches from different workers arrive in unspecified
// order. Downstream operators that require order (Sort, Tail) buffer
// and reorder internally, so this is safe to enable for any plan
// shape.
func WithParallelStreamReads(fn func() []func(cb func(*Frame) error) error) ScanOption {
	return func(n *scanFileNode) { n.parallelStream = fn }
}

// NewScanNode constructs a leaf plan node representing a deferred I/O
// source. label is used in Explain output (e.g. "Scan[parquet](path)"),
// schema is the eagerly-inferred output schema (or nil if unknown at
// construction), and read is called by Collect() to materialize the
// scan's *Frame.
//
// Optional ScanOptions declare capabilities the optimizer can exploit
// — WithColumnProjection enables projection pushdown from any
// Project/Filter above the scan.
//
// Exposed for source packages (parquetio, csvio, ...) to build scan
// leaves without a per-format node type in the core package. Most
// callers should use the format-specific wrapper (parquetio.ScanFile
// and friends) instead of building nodes by hand.
func NewScanNode(label string, schema *arrow.Schema, read func() (*Frame, error), opts ...ScanOption) LogicalPlan {
	n := &scanFileNode{label: label, schema: schema, read: read}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// ProjectableScan is implemented by scan nodes that can accept a
// column-projection hint from the optimizer. Rules test for this
// interface via type assertion; scans that don't implement it are
// left untouched by projection pushdown.
type ProjectableScan interface {
	LogicalPlan
	// ProjectColumns returns a plan node identical to the receiver
	// but restricted to the given column names. If projection is
	// not beneficial (e.g. caller already set an explicit column
	// list), implementations should return the receiver unchanged.
	ProjectColumns(cols []string) LogicalPlan
}

// ProjectColumns satisfies ProjectableScan when a projection
// callback was registered via WithColumnProjection.
//
// A nil return from the callback is treated as "no projection
// applied" — implementations can use it to signal that the caller's
// existing column choice should win over the optimizer's proposal.
func (n *scanFileNode) ProjectColumns(cols []string) LogicalPlan {
	if n.projectFn == nil || len(cols) == 0 {
		return n
	}
	if newPlan := n.projectFn(cols); newPlan != nil {
		return newPlan
	}
	return n
}

// PredicateSink is implemented by scan nodes that can accept a
// filter-predicate hint from the optimizer. Rules test via type
// assertion; scans that don't implement it are left untouched by
// predicate pushdown.
type PredicateSink interface {
	LogicalPlan
	// ApplyPredicate returns a plan node identical to the receiver
	// but with pred available for row-group / bloom-filter skipping
	// at read time. The Filter node above the scan is not removed
	// (row-level filtering still runs after coarse skipping), so
	// callbacks may return the receiver unchanged when they decide
	// the hint isn't useful.
	ApplyPredicate(pred Expr) LogicalPlan
}

// ApplyPredicate satisfies PredicateSink when a predicate callback
// was registered via WithPredicatePushdown.
//
// nil callback return is treated as "no predicate applied" — the
// receiver stays in the plan.
func (n *scanFileNode) ApplyPredicate(pred Expr) LogicalPlan {
	if n.predicateFn == nil || pred.node == nil {
		return n
	}
	if newPlan := n.predicateFn(pred); newPlan != nil {
		return newPlan
	}
	return n
}

// -----------------------------------------------------------------------------
// emptyNode: zero-row constant leaf.
//
// Produced by the optimizer when it can prove a subtree returns no
// rows — currently just Filter(Lit(false)). Preserves the schema so
// downstream nodes' Schema() propagation stays honest.
// -----------------------------------------------------------------------------

type emptyNode struct {
	schema *arrow.Schema
}

func (n *emptyNode) Schema() *arrow.Schema {
	if n.schema == nil {
		return arrow.NewSchema(nil, nil)
	}
	return n.schema
}
func (n *emptyNode) Children() []LogicalPlan { return nil }
func (n *emptyNode) String() string          { return "Empty" }

func (n *scanFileNode) Schema() *arrow.Schema {
	if n.schema == nil {
		return arrow.NewSchema(nil, nil)
	}
	return n.schema
}
func (n *scanFileNode) Children() []LogicalPlan { return nil }
func (n *scanFileNode) String() string          { return n.label }

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// exprOutputName picks a column name for the i-th expression in a
// Select or as the target of a WithColumn call. Nodes implementing
// Namer with a non-empty OutputName win; anything else falls back
// to a positional default.
func exprOutputName(e Expr, i int) string {
	if e.node == nil {
		return fmt.Sprintf("expr_%d", i)
	}
	if n, ok := e.node.(Namer); ok {
		if name := n.OutputName(); name != "" {
			return name
		}
	}
	return fmt.Sprintf("expr_%d", i)
}

// schemaMetadataPtr returns a pointer to the schema's metadata, or
// nil if the schema has none. Needed by arrow.NewSchema's optional
// metadata argument.
func schemaMetadataPtr(s *arrow.Schema) *arrow.Metadata {
	if s == nil || !s.HasMetadata() {
		return nil
	}
	m := s.Metadata()
	return &m
}

// -----------------------------------------------------------------------------
// OutputName implementations on existing expression nodes.
//
// colRefNode.OutputName is the referenced column's name — the most
// common case (Col("id") → "id"). aliasNode.OutputName is the alias.
// All other built-in nodes are anonymous; users must Alias them if
// they care about the output column name.
// -----------------------------------------------------------------------------

func (n *colRefNode) OutputName() string { return n.name }
func (n *aliasNode) OutputName() string  { return n.name }

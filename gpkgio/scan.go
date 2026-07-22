package gpkgio

import (
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/zoobst/gobi"
)

// ScanFile returns a LazyFrame that reads path/opts when Collect is
// called. The scan participates in gobi's optimizer for all three
// pushdown categories:
//
//   - Projection pushdown: Frame.Select() above the scan restricts
//     the SQL SELECT column list at Collect time, reducing what
//     SQLite decodes.
//   - Predicate pushdown: Frame.FilterExpr() above the scan is
//     translated to a SQL WHERE fragment via gobi.ExprToSQL. Parts
//     that translate (col-vs-lit comparisons, AND, NOT) run in SQLite
//     and skip both decode + row materialization; parts that don't
//     (custom nodes, unsupported ops) stay in the executor. Splits
//     at top-level ANDs via gobi.SplitConjuncts so partial
//     translation still delivers a real speedup.
//   - Streaming: reads flow one batch at a time through
//     ReadFileChunksFunc, so peak memory stays bounded even on
//     large layers.
//
// The GeoPackage RTree (created by WriteFile) is not consulted at
// scan build time. It would kick in automatically if the SQL WHERE
// referenced the fid via an RTree join — a follow-up capability
// that requires spatial predicate awareness in ExprToSQL.
func ScanFile(path string, opts *ReadOptions) *gobi.LazyFrame {
	if opts == nil {
		opts = &ReadOptions{}
	}
	// Try to read the schema eagerly. If that fails, the read closure
	// below will surface the same error at Collect time — mirrors
	// parquetio's ScanFile behavior.
	sch, schemaErr := ReadSchema(path, opts)

	label := buildScanLabel(path, opts)

	node := gobi.NewScanNode(label, sch, func() (*gobi.Frame, error) {
		if schemaErr != nil {
			return nil, schemaErr
		}
		return ReadFile(path, opts)
	}, gobi.WithColumnProjection(func(cols []string) gobi.LogicalPlan {
		// User-supplied Columns wins over the optimizer's set —
		// same pattern parquetio uses. Rebuild the scan with the
		// optimizer's projection stamped into ReadOptions.Columns.
		if len(opts.Columns) > 0 {
			return nil // "no change"
		}
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		newOpts.Columns = cols
		return ScanFile(path, &newOpts).Plan()
	}), gobi.WithStreamRead(func(cb func(*gobi.Frame) error) error {
		if schemaErr != nil {
			return schemaErr
		}
		return ReadFileChunksFunc(path, opts, cb)
	}), gobi.WithPredicatePushdown(func(pred gobi.Expr) gobi.LogicalPlan {
		// The optimizer's PushPredicateToScan rule keeps the Filter
		// above the scan (belt-and-suspenders) and re-fires until the
		// sink signals "no change" by returning nil. We record the
		// push on ReadOptions.pushdownDone so the second visit is a no-op.
		if opts.pushdownDone {
			return nil
		}
		// Split the predicate at top-level ANDs — pieces that
		// translate go into the SQL WHERE; pieces that don't stay
		// in the executor's filter. AND is safe to split because
		// (A AND B) filters rows equivalently to (A applied first,
		// then B). OR at top level would need both sides to match
		// so we can't decompose it here; the whole predicate falls
		// back to executor-side filtering in that case.
		conjuncts := gobi.SplitConjuncts(pred)
		var pushed []string
		var pushedArgs []any
		for _, c := range conjuncts {
			sqlText, args, ok := gobi.ExprToSQL(c)
			if !ok {
				continue
			}
			pushed = append(pushed, sqlText)
			pushedArgs = append(pushedArgs, args...)
		}
		if len(pushed) == 0 {
			return nil // nothing translated; keep the existing plan
		}
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		// Merge with any pre-existing Where fragment. Parenthesize
		// each side so operator precedence stays predictable when
		// the caller's Where uses non-parenthesized ops.
		combined := joinWhereFragments(newOpts.Where, pushed)
		newOpts.Where = combined
		newOpts.WhereArgs = append(append([]any{}, newOpts.WhereArgs...), pushedArgs...)
		newOpts.pushdownDone = true
		return ScanFile(path, &newOpts).Plan()
	}))
	return gobi.NewLazyFrame(node)
}

// joinWhereFragments combines an existing Where fragment (from user
// ReadOptions) with the translator-emitted fragments via `AND`. Every
// piece is wrapped in parens defensively — user-supplied strings can
// contain OR chains at top level, in which case bare `AND`
// concatenation would silently change semantics.
func joinWhereFragments(existing string, added []string) string {
	if len(added) == 0 {
		return existing
	}
	parts := make([]string, 0, len(added)+1)
	if existing != "" {
		parts = append(parts, "("+existing+")")
	}
	parts = append(parts, added...)
	// Fragments emitted by ExprToSQL are already fully parenthesized.
	return strings.Join(parts, " AND ")
}

// ReadSchema opens path just far enough to build the arrow schema
// for the target layer. Cheap — it queries the SQLite catalog
// (pragma_table_info) without scanning any rows.
//
// Layer selection follows the same rules as ReadFile: opts.Layer if
// set, or the file's single feature table otherwise. Multiple
// feature tables with no opts.Layer is an error.
func ReadSchema(path string, opts *ReadOptions) (*arrow.Schema, error) {
	g, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer g.Close()

	if opts == nil {
		opts = &ReadOptions{}
	}
	target, err := resolveLayer(g, opts.Layer)
	if err != nil {
		return nil, err
	}
	cols, err := tableColumns(g.db, target.Name)
	if err != nil {
		return nil, err
	}
	// Apply the projection restriction so the schema matches what
	// ReadFile will actually return. Geometry always in.
	if len(opts.Columns) > 0 {
		want := make(map[string]struct{}, len(opts.Columns))
		for _, c := range opts.Columns {
			want[c] = struct{}{}
		}
		if target.GeomCol != "" {
			want[target.GeomCol] = struct{}{}
		}
		filtered := cols[:0]
		for _, c := range cols {
			if _, ok := want[c.name]; ok {
				filtered = append(filtered, c)
			}
		}
		cols = filtered
	}
	return schemaFromColumns(cols, target.GeomCol, target.SRID), nil
}

// buildScanLabel is the human-readable "Scan[gpkg](...)" label shown
// in gobi.LazyFrame.ExplainPhysical(). Includes projection and
// predicate state so the effect of pushdown is visible from Explain.
func buildScanLabel(path string, opts *ReadOptions) string {
	label := fmt.Sprintf("Scan[gpkg](%q", path)
	if opts != nil && opts.Layer != "" {
		label += fmt.Sprintf(", layer=%q", opts.Layer)
	}
	if opts != nil && len(opts.Columns) > 0 {
		label += fmt.Sprintf(", cols=[%s]", strings.Join(opts.Columns, " "))
	}
	if opts != nil && opts.Where != "" {
		label += fmt.Sprintf(", where=%q", opts.Where)
	}
	if opts != nil && opts.Limit > 0 {
		label += fmt.Sprintf(", limit=%d", opts.Limit)
	}
	return label + ")"
}

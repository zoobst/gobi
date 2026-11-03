package pgio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zoobst/gobi"
)

// ScanTable returns a LazyFrame that reads a whole table when
// Collect is called. Participates in gobi's optimizer:
//
//   - Projection pushdown: Frame.Select() above the scan narrows
//     the SELECT column list at Collect time.
//   - Predicate pushdown: Frame.FilterExpr() above the scan is
//     translated to a SQL WHERE clause via gobi.ExprToSQL. Parts
//     that translate run in PostgreSQL (skipping decode + row
//     transfer for filtered-out rows); parts that don't stay in
//     the executor. Splits at top-level ANDs so partial
//     translation still delivers a real win.
//   - Streaming: reads flow one batch at a time via
//     ReadTableChunksFunc.
//
// The scan doesn't automatically use PostGIS spatial indexes —
// that would require ExprToSQL knowing about `ST_Intersects` and
// friends, which it doesn't yet. Callers who need spatial index
// hits can construct the query themselves and use ScanQuery.
//
// Placeholder syntax: pgio uses `$1`, `$2`, ... rather than `?` —
// PostgreSQL doesn't accept `?` in native mode. ExprToSQL emits
// `?` markers, which pgio rewrites to `$N` at scan-build time.
func ScanTable(ctx context.Context, conn Conn, table string, opts *ReadOptions) *gobi.LazyFrame {
	if opts == nil {
		opts = &ReadOptions{}
	}
	// Schema resolution + geometry-column detection happens eagerly
	// so the LazyFrame's schema is available for optimizer
	// projection-pushdown. If the DB round-trip fails, the read
	// closure below surfaces the same error at Collect time.
	sch, dateCols, schemaErr := scanSchema(ctx, conn, table, opts)

	label := buildScanLabel(table, opts)

	node := gobi.NewScanNode(label, sch, func() (*gobi.Frame, error) {
		if schemaErr != nil {
			return nil, schemaErr
		}
		return ReadTable(ctx, conn, table, opts)
	}, gobi.WithColumnProjection(func(cols []string) gobi.LogicalPlan {
		if len(opts.Columns) > 0 {
			return nil
		}
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		newOpts.Columns = cols
		return ScanTable(ctx, conn, table, &newOpts).Plan()
	}), gobi.WithStreamRead(func(cb func(*gobi.Frame) error) error {
		if schemaErr != nil {
			return schemaErr
		}
		return ReadTableChunksFunc(ctx, conn, table, opts, cb)
	}), gobi.WithPredicatePushdown(func(pred gobi.Expr) gobi.LogicalPlan {
		if opts.pushdownDone {
			return nil
		}
		conjuncts := gobi.SplitConjuncts(pred)
		var pushed []string
		var pushedArgs []any
		for _, c := range conjuncts {
			sqlText, args, ok := gobi.ExprToSQL(c)
			if !ok || !timeArgsPushable(c, args, dateCols) {
				continue
			}
			// PostgreSQL doesn't accept `?` placeholders in the
			// native pgx interface — rewrite to `$N`. Offsets start
			// after any existing WhereArgs the caller supplied.
			offset := len(opts.WhereArgs) + len(pushedArgs)
			sqlText = renumberPlaceholders(sqlText, offset)
			pushed = append(pushed, sqlText)
			pushedArgs = append(pushedArgs, args...)
		}
		if len(pushed) == 0 {
			return nil
		}
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		newOpts.Where = joinWhereFragments(newOpts.Where, pushed)
		newOpts.WhereArgs = append(append([]any{}, newOpts.WhereArgs...), pushedArgs...)
		newOpts.pushdownDone = true
		return ScanTable(ctx, conn, table, &newOpts).Plan()
	}))
	return gobi.NewLazyFrame(node)
}

// ScanSchema returns the arrow.Schema ReadTable would produce for
// the given table/opts. Requires a live connection — schema
// inference is done by prepared-statement introspection against
// PostgreSQL. Cheap: one round-trip.
func ScanSchema(ctx context.Context, conn Conn, table string, opts *ReadOptions) (*arrow.Schema, error) {
	sch, _, err := scanSchema(ctx, conn, table, opts)
	return sch, err
}

// scanSchema is ScanSchema plus the set of date columns, which read as
// Timestamp too but need their own pushdown rule (timeArgsPushable).
func scanSchema(ctx context.Context, conn Conn, table string, opts *ReadOptions) (*arrow.Schema, map[string]bool, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	schemaName := defaultSchema(opts.Schema)
	geomSet, err := geometryColumnSet(ctx, conn, schemaName, table, opts.GeomColumns)
	if err != nil {
		return nil, nil, err
	}
	tableCols, err := listTableColumns(ctx, conn, schemaName, table)
	if err != nil {
		return nil, nil, err
	}
	pick := tableCols
	if len(opts.Columns) > 0 {
		want := make(map[string]struct{}, len(opts.Columns))
		for _, c := range opts.Columns {
			want[c] = struct{}{}
		}
		filtered := pick[:0]
		for _, c := range tableCols {
			if _, ok := want[c]; ok {
				filtered = append(filtered, c)
			}
		}
		pick = filtered
	}
	// Pull per-column arrow types from a LIMIT 0 SELECT — PostgreSQL
	// returns the field descriptions without transferring any row
	// data. Cheapest way to learn the OIDs.
	parts := make([]string, len(pick))
	for i, c := range pick {
		if geomSet[c] {
			parts[i] = fmt.Sprintf("ST_AsEWKB(%s) AS %s", quoteIdent(c), quoteIdent(c))
			continue
		}
		parts[i] = quoteIdent(c)
	}
	probeSQL := fmt.Sprintf(`SELECT %s FROM %s.%s LIMIT 0`,
		strings.Join(parts, ", "),
		quoteIdent(schemaName), quoteIdent(table))
	rows, err := conn.Query(ctx, probeSQL)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	geomOIDs, err := geometryOIDs(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	fields := make([]arrow.Field, len(fds))
	dateCols := make(map[string]bool)
	for i, fd := range fds {
		if fd.DataTypeOID == pgtype.DateOID {
			dateCols[fd.Name] = true
		}
		if geomOIDs[fd.DataTypeOID] || geomSet[fd.Name] {
			fields[i] = gobi.GeometryField(fd.Name, 0)
			continue
		}
		dt, err := oidToArrowType(fd.DataTypeOID)
		if err != nil {
			return nil, nil, fmt.Errorf("pgio: schema col %q: %w", fd.Name, err)
		}
		fields[i] = arrow.Field{Name: fd.Name, Type: dt, Nullable: true}
	}
	return arrow.NewSchema(fields, nil), dateCols, nil
}

// timeArgsPushable reports whether a translated conjunct with time
// arguments gives PostgreSQL's answer or a superset of it (the
// executor filters again above the scan). PostgreSQL types a
// parameter from the column it's compared with, so two cases would
// round the literal and lose rows:
//   - a date column truncates it to the day: d < 12:00 on the 2nd
//     becomes d < the 2nd, dropping the 2nd itself;
//   - timestamp / timestamptz keep microseconds, and pgx truncates
//     the rest: ts < 1.0000005 s becomes ts < 1 s.
//
// Those stay in the executor.
func timeArgsPushable(c gobi.Expr, args []any, dateCols map[string]bool) bool {
	hasTime := false
	for _, a := range args {
		if t, ok := a.(time.Time); ok {
			hasTime = true
			if t.Nanosecond()%1000 != 0 {
				return false
			}
		}
	}
	if !hasTime {
		return true
	}
	for _, col := range gobi.ExprColumns(c) {
		if dateCols[col] {
			return false
		}
	}
	return true
}

// buildScanLabel is the human-readable "Scan[postgres](...)" label
// shown in gobi.LazyFrame.ExplainPhysical().
func buildScanLabel(table string, opts *ReadOptions) string {
	label := fmt.Sprintf("Scan[postgres](%q", table)
	if opts != nil {
		if opts.Schema != "" {
			label += fmt.Sprintf(", schema=%q", opts.Schema)
		}
		if len(opts.Columns) > 0 {
			label += fmt.Sprintf(", cols=[%s]", strings.Join(opts.Columns, " "))
		}
		if opts.Where != "" {
			label += fmt.Sprintf(", where=%q", opts.Where)
		}
		if opts.Limit > 0 {
			label += fmt.Sprintf(", limit=%d", opts.Limit)
		}
	}
	return label + ")"
}

// joinWhereFragments combines an existing Where fragment with the
// translator-emitted fragments via AND. Same shape as gpkgio's
// helper — user-supplied strings can contain non-parenthesized ops,
// so wrap defensively.
func joinWhereFragments(existing string, added []string) string {
	if len(added) == 0 {
		return existing
	}
	parts := make([]string, 0, len(added)+1)
	if existing != "" {
		parts = append(parts, "("+existing+")")
	}
	parts = append(parts, added...)
	return strings.Join(parts, " AND ")
}

// renumberPlaceholders rewrites `?` markers in sql to `$N` where N
// starts at offset+1. ExprToSQL emits `?` (SQLite-style), but pgx
// requires `$N` positional placeholders. Simple linear pass — no
// need for a full SQL tokenizer since ExprToSQL only produces `?`
// inside comparison RHSs where they never appear inside string
// literals or identifiers.
func renumberPlaceholders(sql string, offset int) string {
	var b strings.Builder
	n := offset
	for _, c := range sql {
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

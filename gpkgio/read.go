package gpkgio

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
)

// ReadOptions controls ReadFile / ScanFile behavior.
//
// Empty / zero fields fall back to sensible defaults: Layer picks
// the only feature table when the file has one, or errors otherwise.
type ReadOptions struct {
	// Layer names the feature table to read. Required when the file
	// has more than one feature table; optional otherwise.
	Layer string

	// Columns optionally restricts the read to these column names.
	// nil / empty means "read every column". The geometry column
	// is always included when it's present in the source layer,
	// even if it isn't listed here — GeoPackage's whole point is
	// geometry, and dropping it silently would surprise users. If
	// you truly want an attribute-only projection, use
	// `SELECT *` directly against the underlying sql.DB.
	Columns []string

	// Where is an optional SQL WHERE fragment appended verbatim
	// after WHERE in the SELECT. Callers are responsible for their
	// own quoting — this is a lower-level knob than a full
	// gobi.Expr predicate. ScanFile lifts predicate pushdown into
	// this automatically for expressions the SQL translator can
	// handle; ReadFile leaves it to callers.
	//
	// Use `?` placeholders in the fragment and supply matching
	// values via WhereArgs — driver-safe parameterization. Callers
	// concatenating literal values into the string are responsible
	// for their own escaping.
	Where string

	// WhereArgs holds the positional bind arguments for `?` markers
	// in Where. Filled automatically when ScanFile pushes a
	// translated predicate down; callers who set Where by hand can
	// supply their own args here.
	WhereArgs []any

	// Limit caps the number of rows returned. 0 = unlimited.
	Limit int64

	// Allocator overrides the arrow allocator used for the produced
	// Frame's columns. nil = memory.DefaultAllocator. Provided for
	// callers who pool arrow buffers across pipelines.
	Allocator memory.Allocator

	// pushdownDone is an internal flag ScanFile's predicate-pushdown
	// callback sets after the first successful push, so the
	// optimizer's fixed-point loop doesn't re-push the same
	// predicate on every iteration. The gobi optimizer's PushPredicateToScan
	// rule keeps the Filter above the scan for belt-and-suspenders
	// safety, which means the rule re-fires until the sink signals
	// "no change" by returning nil.
	pushdownDone bool
}

// ReadFile opens the GeoPackage at path and reads one layer's rows
// into a Frame. The geometry column (if present) is returned as a
// Binary column tagged with gobi.GeometryField metadata carrying
// the layer's SRS.
//
// The layer's arrow schema is inferred from the SQLite column
// affinities: INTEGER → Int64, REAL → Float64, TEXT → String,
// BLOB → Binary. The geometry column always comes out as Binary
// regardless of affinity so the WKB payload round-trips through
// WriteFile without re-encoding.
//
// For streaming reads, ReadFileChunksFunc is the callback-based
// counterpart — bounded memory regardless of layer size.
func ReadFile(path string, opts *ReadOptions) (*gobi.Frame, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	g, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer g.Close()

	target, err := resolveLayer(g, opts.Layer)
	if err != nil {
		return nil, err
	}
	sqlText, err := buildSelectSQL(g.db, target, opts)
	if err != nil {
		return nil, err
	}
	return readFrom(g.db, target, sqlText, opts.WhereArgs, resolveAllocator(opts))
}

// ReadFileChunksFunc streams the rows of one layer through fn one
// batch at a time. Peak memory is bounded to one batch (default
// 64k rows). Returning a non-nil error from fn aborts the read.
//
// Batch boundaries are just row-count-based; there's no attempt to
// align with SQLite's B-tree pages or the RTree. If you need
// predicate-driven skipping, use ScanFile instead — it drives the
// LazyFrame optimizer which pushes WHERE clauses into the SQL.
func ReadFileChunksFunc(path string, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	if opts == nil {
		opts = &ReadOptions{}
	}
	g, err := Open(path)
	if err != nil {
		return err
	}
	defer g.Close()

	target, err := resolveLayer(g, opts.Layer)
	if err != nil {
		return err
	}
	sqlText, err := buildSelectSQL(g.db, target, opts)
	if err != nil {
		return err
	}
	return streamFrom(context.Background(), g.db, target, sqlText, opts.WhereArgs, resolveAllocator(opts), defaultChunkRows, fn)
}

// defaultChunkRows is the row-count target per batch emitted by
// ReadFileChunksFunc and the ScanFile stream reader. Matches
// parquetio's default so users mixing formats see similar shapes.
const defaultChunkRows = 65_536

// resolveAllocator returns opts.Allocator or memory.DefaultAllocator
// when unset. Kept small so every read path uses the same resolution.
func resolveAllocator(opts *ReadOptions) memory.Allocator {
	if opts != nil && opts.Allocator != nil {
		return opts.Allocator
	}
	return memory.DefaultAllocator
}

// resolveLayer picks a FeatureTable to read based on opts.Layer.
// When opts.Layer is empty and the file has exactly one feature
// table, that one is used; when there are multiple, we return an
// error listing them.
func resolveLayer(g *GeoPackage, want string) (*FeatureTable, error) {
	tables, err := g.FeatureTables()
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("gpkg: no feature tables in file")
	}
	if want == "" {
		if len(tables) == 1 {
			return &tables[0], nil
		}
		names := make([]string, len(tables))
		for i, t := range tables {
			names[i] = t.Name
		}
		return nil, fmt.Errorf("gpkg: multiple feature tables (%v); set ReadOptions.Layer to pick one",
			names)
	}
	for i := range tables {
		if tables[i].Name == want {
			return &tables[i], nil
		}
	}
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	return nil, fmt.Errorf("gpkg: layer %q not found; available: %v", want, names)
}

// buildSelectSQL constructs the SELECT for a Read/Scan. Column
// projection is applied by naming individual columns explicitly.
// The geometry column is always included when the layer has one.
// Predicate + limit come from opts.
func buildSelectSQL(db *sql.DB, target *FeatureTable, opts *ReadOptions) (string, error) {
	cols, err := tableColumns(db, target.Name)
	if err != nil {
		return "", err
	}
	pick := cols
	if len(opts.Columns) > 0 {
		want := make(map[string]struct{}, len(opts.Columns))
		for _, c := range opts.Columns {
			want[c] = struct{}{}
		}
		// Always include the geometry column even if the caller
		// didn't list it — dropping it silently would surprise the
		// GeoPackage use case (Frames without geometry are handled
		// by attribute-only tables that don't route through here).
		if target.GeomCol != "" {
			want[target.GeomCol] = struct{}{}
		}
		filtered := pick[:0]
		for _, c := range cols {
			if _, ok := want[c.name]; ok {
				filtered = append(filtered, c)
			}
		}
		pick = filtered
	}
	parts := make([]string, len(pick))
	for i, c := range pick {
		parts[i] = quoteIdent(c.name)
	}
	sqlText := fmt.Sprintf("SELECT %s FROM %s", strings.Join(parts, ", "), quoteIdent(target.Name))
	if opts.Where != "" {
		sqlText += " WHERE " + opts.Where
	}
	if opts.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	return sqlText, nil
}

// tableColumns queries SQLite's pragma_table_info to enumerate the
// columns of a table + their declared affinities. Preserves column
// order (which SQLite preserves per CREATE TABLE ordering).
func tableColumns(db *sql.DB, table string) ([]columnInfo, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT name, type FROM pragma_table_info(%s)`,
		"'"+strings.ReplaceAll(table, "'", "''")+"'"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []columnInfo
	for rows.Next() {
		var c columnInfo
		if err := rows.Scan(&c.name, &c.declType); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// columnInfo carries the SQLite name + declared type for one column.
// Declared type is the raw string from the CREATE TABLE, not the
// affinity — but we resolve to affinity in schemaFromColumns via
// the SQLite rule set (contains "INT" → INTEGER, etc.).
type columnInfo struct {
	name     string
	declType string
}

// schemaFromColumns picks an arrow.Field for each SQLite column
// based on its declared type. Follows SQLite's 5-rule affinity
// resolution (§3 of CREATE TABLE docs) except that the geometry
// column always comes out as a Binary field carrying the geometry
// metadata gobi.GeometryField sets.
func schemaFromColumns(cols []columnInfo, geomCol string, srsID int32) *arrow.Schema {
	fields := make([]arrow.Field, len(cols))
	for i, c := range cols {
		if c.name == geomCol {
			fields[i] = gobi.GeometryField(c.name, srsID)
			continue
		}
		fields[i] = arrow.Field{
			Name:     c.name,
			Type:     arrowTypeForAffinity(c.declType),
			Nullable: true,
		}
	}
	return arrow.NewSchema(fields, nil)
}

// arrowTypeForAffinity maps a SQLite declared type to an arrow type.
// SQLite's affinity rules are lenient; this covers the common cases
// gobi.WriteFile emits + the handful of aliases GDAL / ogr2ogr use.
func arrowTypeForAffinity(decl string) arrow.DataType {
	u := strings.ToUpper(decl)
	switch {
	case u == "" || u == "BLOB":
		return arrow.BinaryTypes.Binary
	case strings.Contains(u, "INT"):
		return arrow.PrimitiveTypes.Int64
	case strings.Contains(u, "CHAR"), strings.Contains(u, "TEXT"), strings.Contains(u, "CLOB"):
		return arrow.BinaryTypes.String
	case strings.Contains(u, "REAL"), strings.Contains(u, "FLOA"), strings.Contains(u, "DOUB"):
		return arrow.PrimitiveTypes.Float64
	case strings.Contains(u, "BOOL"):
		// SQLite stores bool as INTEGER; arrow Bool is 1-bit which
		// loses on round-trip through a fresh WriteFile→ReadFile.
		// We follow SQLite's storage class and use Int64 so the
		// data doesn't get truncated on rewrite.
		return arrow.PrimitiveTypes.Int64
	}
	// SQLite's NUMERIC fallback — we can't tell int vs float from
	// the declared type, so pick Float64 as the safe superset.
	return arrow.PrimitiveTypes.Float64
}

// readFrom pulls every row of sqlText into a Frame. Streams under
// the hood so peak memory stays bounded even for large layers.
// whereArgs are the `?` bind values for sqlText's WHERE clause; nil
// / empty when the SQL has no placeholders. pool is the arrow
// allocator used for the produced Frame's columns.
func readFrom(db *sql.DB, target *FeatureTable, sqlText string, whereArgs []any, pool memory.Allocator) (*gobi.Frame, error) {
	var out *gobi.Frame
	// Reuse the streaming path with a batch size big enough to
	// swallow most tables in one chunk. The streamFrom callback
	// receives one Frame per batch; ReadFile concatenates them.
	err := streamFrom(context.Background(), db, target, sqlText, whereArgs, pool, defaultChunkRows, func(f *gobi.Frame) error {
		if out == nil {
			out = f
			return nil
		}
		combined, err := out.Concat(f)
		if err != nil {
			return err
		}
		out = combined
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		// Empty result set — return a zero-row Frame with the layer's
		// schema so callers can still inspect column names/types.
		cols, err := tableColumns(db, target.Name)
		if err != nil {
			return nil, err
		}
		schema := schemaFromColumns(cols, target.GeomCol, target.SRID)
		return emptyFrame(schema)
	}
	return out, nil
}

// streamFrom pulls rows in batches of up to batchSize and hands each
// batch to fn as a Frame. Returns fn's first non-nil error verbatim,
// or the underlying SQL error if any.
//
// whereArgs are the positional `?` bind values for sqlText's WHERE
// fragment. Passed to db.QueryContext so predicate-pushdown from
// ScanFile uses parameterized SQL rather than string concatenation.
// pool is the arrow allocator used for column builders in every
// emitted batch — reuse the same allocator across batches to
// benefit from arrow-go's pool amortization.
func streamFrom(ctx context.Context, db *sql.DB, target *FeatureTable, sqlText string, whereArgs []any, pool memory.Allocator, batchSize int, fn func(*gobi.Frame) error) error {
	cols, err := tableColumns(db, target.Name)
	if err != nil {
		return err
	}
	// Filter cols to the projection the SELECT actually chose. The
	// SELECT was built column-by-column from a subset of cols so we
	// need to align — re-derive from sqlText's column list would be
	// error-prone; instead, extract the projected names from the
	// column-list within the SQL string.
	projected := projectedColumns(sqlText, cols)
	schema := schemaFromColumns(projected, target.GeomCol, target.SRID)

	rows, err := db.QueryContext(ctx, sqlText, whereArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make([]array.Builder, len(projected))
	for i, c := range projected {
		field := schema.Field(i)
		b, err := builderFor(pool, field.Type)
		if err != nil {
			return fmt.Errorf("gpkg: builder for %s: %w", c.name, err)
		}
		builders[i] = b
	}
	defer func() {
		for _, b := range builders {
			b.Release()
		}
	}()

	// Scan destinations — one *any per column, reused per row.
	dests := make([]any, len(projected))
	holders := make([]any, len(projected))
	for i := range dests {
		holders[i] = &dests[i]
	}

	rowCount := 0
	for rows.Next() {
		if err := rows.Scan(holders...); err != nil {
			return err
		}
		for i, c := range projected {
			v := dests[i]
			if v == nil {
				builders[i].AppendNull()
				continue
			}
			if err := appendSQLValue(builders[i], v, c.name == target.GeomCol); err != nil {
				return fmt.Errorf("gpkg: append col %s: %w", c.name, err)
			}
		}
		rowCount++
		if rowCount >= batchSize {
			f, err := finalizeBatch(schema, builders)
			if err != nil {
				return err
			}
			if err := fn(f); err != nil {
				return err
			}
			// Reset builders for the next batch.
			for i, c := range projected {
				builders[i].Release()
				field := schema.Field(i)
				b, err := builderFor(pool, field.Type)
				if err != nil {
					return fmt.Errorf("gpkg: rebuild builder for %s: %w", c.name, err)
				}
				builders[i] = b
			}
			rowCount = 0
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Emit the trailing partial batch (or an empty one if the whole
	// result set was empty and the caller wants the schema anyway).
	if rowCount > 0 {
		f, err := finalizeBatch(schema, builders)
		if err != nil {
			return err
		}
		return fn(f)
	}
	return nil
}

// projectedColumns extracts the columns actually appearing in the
// SELECT's column-list from sqlText. It's a tiny scanner rather
// than a real SQL parser — sufficient because buildSelectSQL emits
// a canonical `SELECT "name1", "name2", ... FROM ...` shape and no
// other producer feeds sqlText.
func projectedColumns(sqlText string, all []columnInfo) []columnInfo {
	start := strings.Index(sqlText, "SELECT ")
	end := strings.Index(sqlText, " FROM ")
	if start < 0 || end < 0 || end <= start {
		return all
	}
	list := sqlText[start+len("SELECT ") : end]
	names := make(map[string]struct{})
	for _, part := range strings.Split(list, ",") {
		p := strings.TrimSpace(part)
		p = strings.Trim(p, `"`)
		names[p] = struct{}{}
	}
	out := make([]columnInfo, 0, len(all))
	for _, c := range all {
		if _, ok := names[c.name]; ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// appendSQLValue coerces one scanned SQL value into a typed builder.
// Follows database/sql's own type promotion: SQLite INTEGER comes
// back as int64, REAL as float64, TEXT as string, BLOB as []byte.
// Geometry columns skip the coercion — the raw GPB blob is stored
// as-is under a Binary builder, ready for round-trip via WriteFile.
func appendSQLValue(b array.Builder, v any, isGeom bool) error {
	if isGeom {
		// Strip the GPB header (magic + version + flags + srsID +
		// envelope) so the value in the Frame is plain WKB, matching
		// what gobi's geometry codec expects.
		blob, ok := v.([]byte)
		if !ok {
			return fmt.Errorf("geometry not []byte: %T", v)
		}
		wkb, err := stripGPBHeader(blob)
		if err != nil {
			return err
		}
		b.(*array.BinaryBuilder).Append(wkb)
		return nil
	}
	switch tb := b.(type) {
	case *array.Int64Builder:
		switch x := v.(type) {
		case int64:
			tb.Append(x)
		case bool:
			if x {
				tb.Append(1)
			} else {
				tb.Append(0)
			}
		case float64:
			tb.Append(int64(x))
		default:
			return fmt.Errorf("int column got %T", v)
		}
	case *array.Float64Builder:
		switch x := v.(type) {
		case float64:
			tb.Append(x)
		case int64:
			tb.Append(float64(x))
		default:
			return fmt.Errorf("float column got %T", v)
		}
	case *array.StringBuilder:
		switch x := v.(type) {
		case string:
			tb.Append(x)
		case []byte:
			tb.Append(string(x))
		default:
			return fmt.Errorf("string column got %T", v)
		}
	case *array.BinaryBuilder:
		switch x := v.(type) {
		case []byte:
			tb.Append(x)
		case string:
			tb.Append([]byte(x))
		default:
			return fmt.Errorf("binary column got %T", v)
		}
	default:
		return fmt.Errorf("unsupported builder %T", b)
	}
	return nil
}

// stripGPBHeader parses the GPB header on a geometry blob and
// returns the trailing WKB. Handles all envelope sizes per spec
// §2.1.3.1.1 (0/32/48/64 bytes).
func stripGPBHeader(blob []byte) ([]byte, error) {
	if len(blob) < 8 || blob[0] != 'G' || blob[1] != 'P' {
		return nil, ErrInvalidHeader
	}
	env, err := envelopeBytes(blob[3])
	if err != nil {
		return nil, err
	}
	off := 8 + env
	if len(blob) < off {
		return nil, fmt.Errorf("gpkg: truncated GPB header")
	}
	return blob[off:], nil
}

// finalizeBatch materializes every builder into an arrow.Array,
// assembles the Frame, and releases the arrays back to the caller
// via the Frame's Arrow buffer refcounts. Builders remain valid
// (still hold their own ref) so the caller can Release them.
func finalizeBatch(schema *arrow.Schema, builders []array.Builder) (*gobi.Frame, error) {
	arrs := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrs[i] = b.NewArray()
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(schema.Field(i), chunked)
		chunked.Release()
	}
	return gobi.NewFrame(schema, cols)
}

// emptyFrame builds a zero-row Frame matching schema. Used when a
// SELECT returns no rows — we still want to return a Frame the
// caller can inspect for column names + types.
func emptyFrame(schema *arrow.Schema) (*gobi.Frame, error) {
	pool := memory.DefaultAllocator
	cols := make([]arrow.Column, schema.NumFields())
	for i, f := range schema.Fields() {
		b, err := builderFor(pool, f.Type)
		if err != nil {
			return nil, err
		}
		arr := b.NewArray()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		cols[i] = *arrow.NewColumn(f, chunked)
		arr.Release()
		chunked.Release()
		b.Release()
	}
	return gobi.NewFrame(schema, cols)
}

// builderFor is a local counterpart to gobi.builderForType. Kept
// package-local since gobi doesn't export it and the set of arrow
// types the gpkg read path produces is small.
func builderFor(pool memory.Allocator, t arrow.DataType) (array.Builder, error) {
	switch t.ID() {
	case arrow.INT64:
		return array.NewInt64Builder(pool), nil
	case arrow.FLOAT64:
		return array.NewFloat64Builder(pool), nil
	case arrow.STRING:
		return array.NewStringBuilder(pool), nil
	case arrow.BINARY:
		return array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary), nil
	}
	return nil, fmt.Errorf("gpkg: no builder for arrow type %s", t)
}

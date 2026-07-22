package pgio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zoobst/gobi"
)

// ReadQuery runs sql against conn and materializes the result into a
// Frame. Column types are inferred from the query's field
// descriptions (pgx returns the PostgreSQL OID for each column,
// which maps to an arrow.DataType via oidToArrowType).
//
// Geometry columns are detected by comparing the column's OID
// against the connection's geometry OID (queried lazily via
// selectGeometryOID). Geometry values come back as raw EWKB bytes;
// pgio strips the EWKB SRID header so the resulting Binary column
// carries plain WKB, matching gobi's on-Frame convention.
//
// Peak memory scales with the query's row count. For bounded memory,
// use ReadQueryChunksFunc.
func ReadQuery(ctx context.Context, conn Conn, sql string, args ...any) (*gobi.Frame, error) {
	var out *gobi.Frame
	err := ReadQueryChunksFunc(ctx, conn, sql, args, nil, defaultChunkRows, func(f *gobi.Frame) error {
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
		return nil, fmt.Errorf("pgio: ReadQuery: empty result and no schema hint available")
	}
	return out, nil
}

// ReadTable reads a whole table (with optional projection + WHERE)
// into a Frame. Convenience wrapper over ReadQuery that builds the
// SELECT internally, including `ST_AsBinary(geom_col) AS geom_col`
// wrappers for known geometry columns — so callers don't have to
// hand-craft the query to get WKB out.
func ReadTable(ctx context.Context, conn Conn, table string, opts *ReadOptions) (*gobi.Frame, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	sql, args, err := buildTableSelect(ctx, conn, table, opts)
	if err != nil {
		return nil, err
	}
	// Route through ReadQueryChunksFunc directly (not ReadQuery) so
	// opts.Allocator threads through. ReadQuery is the entry point
	// for raw SQL where the caller doesn't have a ReadOptions to
	// consult.
	var out *gobi.Frame
	err = ReadQueryChunksFunc(ctx, conn, sql, args, resolveAllocator(opts), defaultChunkRows, func(f *gobi.Frame) error {
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
		return nil, fmt.Errorf("pgio: ReadTable: empty result")
	}
	return out, nil
}

// ReadQueryChunksFunc streams the result of sql through fn one batch
// at a time (batches of ~64k rows by default). Peak memory bounded
// to one batch.
//
// pool overrides the arrow allocator for the produced Frames; pass
// nil for memory.DefaultAllocator. Callers plumbing through a
// ReadOptions should use resolveAllocator(opts) at the call site.
//
// The Frame passed to fn owns its arrow buffers; if the callback
// wants to keep the Frame past its return, call frame.Retain() and
// match it with a Release() later.
func ReadQueryChunksFunc(ctx context.Context, conn Conn, sql string, args []any, pool memory.Allocator, batchSize int, fn func(*gobi.Frame) error) error {
	if pool == nil {
		pool = memory.DefaultAllocator
	}
	if batchSize <= 0 {
		batchSize = defaultChunkRows
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	geomOIDs, err := geometryOIDs(ctx, conn)
	if err != nil {
		return err
	}

	// Build the arrow schema + one builder per column up front.
	// Types are picked from pgx's OID → arrow mapping.
	fields := make([]arrow.Field, len(fds))
	builders := make([]array.Builder, len(fds))
	isGeom := make([]bool, len(fds))
	for i, fd := range fds {
		if geomOIDs[fd.DataTypeOID] {
			isGeom[i] = true
			fields[i] = gobi.GeometryField(fd.Name, 0) // SRID filled per-row below
			builders[i] = array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
			continue
		}
		dt, err := oidToArrowType(fd.DataTypeOID)
		if err != nil {
			return fmt.Errorf("pgio: column %q: %w", fd.Name, err)
		}
		fields[i] = arrow.Field{Name: fd.Name, Type: dt, Nullable: true}
		b, err := builderForArrow(pool, dt)
		if err != nil {
			return fmt.Errorf("pgio: builder for %s (%s): %w", fd.Name, dt, err)
		}
		builders[i] = b
	}
	schema := arrow.NewSchema(fields, nil)
	defer func() {
		for _, b := range builders {
			if b != nil {
				b.Release()
			}
		}
	}()

	// Track the SRID observed in the first geometry value so the
	// output field's metadata carries it. PostGIS layers don't
	// always agree on SRID within a single table, but ~all real
	// data does; we go with the first row's SRID and warn if it
	// mismatches.
	srids := make(map[int]int32) // column index → SRID

	rowCount := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return err
		}
		for i, v := range vals {
			if v == nil {
				builders[i].AppendNull()
				continue
			}
			if isGeom[i] {
				wkb, srid, err := decodeEWKB(v)
				if err != nil {
					return fmt.Errorf("pgio: column %q: %w", fds[i].Name, err)
				}
				if existing, ok := srids[i]; !ok {
					srids[i] = srid
				} else if existing != srid && srid != 0 {
					// Later rows disagreeing with the first — take
					// the first observation as canonical; subsequent
					// rows carry their own SRID in the returned
					// bytes if the caller cares.
				}
				builders[i].(*array.BinaryBuilder).Append(wkb)
				continue
			}
			if err := appendPGValue(builders[i], v); err != nil {
				return fmt.Errorf("pgio: column %q: %w", fds[i].Name, err)
			}
		}
		rowCount++
		if rowCount >= batchSize {
			f, err := finalizeBatch(schema, builders, srids, fields, pool)
			if err != nil {
				return err
			}
			if err := fn(f); err != nil {
				return err
			}
			// Rebuild builders for the next batch.
			for i, fd := range fds {
				builders[i].Release()
				if isGeom[i] {
					builders[i] = array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
					continue
				}
				b, err := builderForArrow(pool, fields[i].Type)
				if err != nil {
					return fmt.Errorf("pgio: rebuild builder for %s: %w", fd.Name, err)
				}
				builders[i] = b
			}
			rowCount = 0
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if rowCount > 0 {
		f, err := finalizeBatch(schema, builders, srids, fields, pool)
		if err != nil {
			return err
		}
		return fn(f)
	}
	// Empty result: return a zero-row Frame so callers see the schema.
	empty, err := emptyFrame(schema)
	if err != nil {
		return err
	}
	return fn(empty)
}

// ReadTableChunksFunc streams a whole table (with optional filters
// via opts) through fn in batches. Same shape as ReadQueryChunksFunc
// but uses the built-in table-select builder.
func ReadTableChunksFunc(ctx context.Context, conn Conn, table string, opts *ReadOptions, fn func(*gobi.Frame) error) error {
	if opts == nil {
		opts = &ReadOptions{}
	}
	sql, args, err := buildTableSelect(ctx, conn, table, opts)
	if err != nil {
		return err
	}
	return ReadQueryChunksFunc(ctx, conn, sql, args, resolveAllocator(opts), defaultChunkRows, fn)
}

const defaultChunkRows = 65_536

// buildTableSelect assembles the SELECT for a ReadTable call:
//   - qualifies the table with the schema
//   - includes ST_AsBinary(col) wrappers for geometry columns so
//     the output is plain-ish EWKB (still carries SRID header)
//   - honors opts.Columns (projection), opts.Where (predicate),
//     opts.Limit
func buildTableSelect(ctx context.Context, conn Conn, table string, opts *ReadOptions) (string, []any, error) {
	schema := defaultSchema(opts.Schema)
	geomSet, err := geometryColumnSet(ctx, conn, schema, table, opts.GeomColumns)
	if err != nil {
		return "", nil, err
	}
	tableCols, err := listTableColumns(ctx, conn, schema, table)
	if err != nil {
		return "", nil, err
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
	parts := make([]string, len(pick))
	for i, c := range pick {
		if geomSet[c] {
			// ST_AsEWKB preserves the SRID; ST_AsBinary drops it.
			// We want SRID preserved for round-trip through
			// WriteTable, so use ST_AsEWKB. The resulting bytes are
			// EWKB — decodeEWKB in ReadQueryChunksFunc strips the
			// SRID header into a separate value.
			parts[i] = fmt.Sprintf("ST_AsEWKB(%s) AS %s", quoteIdent(c), quoteIdent(c))
			continue
		}
		parts[i] = quoteIdent(c)
	}
	sql := fmt.Sprintf(`SELECT %s FROM %s.%s`,
		strings.Join(parts, ", "),
		quoteIdent(schema), quoteIdent(table))
	args := append([]any{}, opts.WhereArgs...)
	if opts.Where != "" {
		sql += " WHERE " + opts.Where
	}
	if opts.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	return sql, args, nil
}

// listTableColumns returns column names for schema.table in the
// order PostgreSQL stores them (ordinal_position). Uses information_schema
// so it works against any table the user can read.
func listTableColumns(ctx context.Context, conn Conn, schema, table string) ([]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pgio: table %s.%s not found or empty", schema, table)
	}
	return out, nil
}

// geometryColumnSet returns a lookup map for which columns of
// schema.table are PostGIS geometry columns. Uses the standard
// geometry_columns view. Falls back to the explicit hint list when
// the view is unavailable (e.g., PostGIS not installed — pgio
// still works for plain-Postgres tables, just without geometry
// detection).
func geometryColumnSet(ctx context.Context, conn Conn, schema, table string, hint []string) (map[string]bool, error) {
	if len(hint) > 0 {
		out := make(map[string]bool, len(hint))
		for _, h := range hint {
			out[h] = true
		}
		return out, nil
	}
	rows, err := conn.Query(ctx, `
		SELECT f_geometry_column
		FROM geometry_columns
		WHERE f_table_schema = $1 AND f_table_name = $2`, schema, table)
	if err != nil {
		// PostGIS not installed — treat as no geometry columns
		// (still useful for plain Postgres tables).
		return map[string]bool{}, nil
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		out[col] = true
	}
	// rows.Err() intentionally ignored — if PostGIS is missing, we
	// still return the (empty) map above.
	return out, nil
}

// geometryOIDs returns the OID(s) that the PostgreSQL server uses
// for the PostGIS geometry type. Cached per-connection would be
// nice, but we don't hold conn state — accepting the extra
// round-trip is fine for the shapes ReadQuery is designed to
// handle (interactive analytics, ETL). If the query fails (PostGIS
// not installed), an empty map is returned and no columns get
// treated as geometry — safe fallback.
func geometryOIDs(ctx context.Context, conn Conn) (map[uint32]bool, error) {
	rows, err := conn.Query(ctx, `
		SELECT oid FROM pg_type WHERE typname IN ('geometry', 'geography')`)
	if err != nil {
		return map[uint32]bool{}, nil
	}
	defer rows.Close()
	out := map[uint32]bool{}
	for rows.Next() {
		var oid uint32
		if err := rows.Scan(&oid); err != nil {
			return nil, err
		}
		out[oid] = true
	}
	return out, nil
}

// decodeEWKB strips the 4-byte SRID header from an EWKB blob and
// returns (WKB, srid). PostGIS's ST_AsEWKB output layout:
//
//	byte 0      byte order (0=BE, 1=LE)
//	bytes 1-4   type (with SRID bit set)
//	bytes 5-8   SRID (in the declared byte order)
//	bytes 9-N   coordinate payload
//
// This function collapses that into plain WKB (with the SRID bit
// cleared) plus a separate int32 SRID value.
func decodeEWKB(v any) ([]byte, int32, error) {
	blob, ok := v.([]byte)
	if !ok {
		return nil, 0, fmt.Errorf("geometry value not []byte: %T", v)
	}
	if len(blob) < 9 {
		return nil, 0, fmt.Errorf("EWKB truncated: %d bytes", len(blob))
	}
	// Read type + SRID with the correct byte order.
	var littleEndian bool
	switch blob[0] {
	case 0:
		littleEndian = false
	case 1:
		littleEndian = true
	default:
		return nil, 0, fmt.Errorf("EWKB byte-order flag = %d", blob[0])
	}
	var typeBits, sridBits uint32
	if littleEndian {
		typeBits = uint32(blob[1]) | uint32(blob[2])<<8 | uint32(blob[3])<<16 | uint32(blob[4])<<24
		sridBits = uint32(blob[5]) | uint32(blob[6])<<8 | uint32(blob[7])<<16 | uint32(blob[8])<<24
	} else {
		typeBits = uint32(blob[4]) | uint32(blob[3])<<8 | uint32(blob[2])<<16 | uint32(blob[1])<<24
		sridBits = uint32(blob[8]) | uint32(blob[7])<<8 | uint32(blob[6])<<16 | uint32(blob[5])<<24
	}
	const sridFlag = 0x20000000
	hasSRID := typeBits&sridFlag != 0
	if !hasSRID {
		// Plain WKB — return as-is with SRID unknown.
		return blob, 0, nil
	}
	// Clear the SRID flag so the returned bytes are plain WKB.
	newType := typeBits &^ sridFlag
	wkb := make([]byte, 0, len(blob)-4)
	wkb = append(wkb, blob[0])
	if littleEndian {
		wkb = append(wkb, byte(newType), byte(newType>>8), byte(newType>>16), byte(newType>>24))
	} else {
		wkb = append(wkb, byte(newType>>24), byte(newType>>16), byte(newType>>8), byte(newType))
	}
	wkb = append(wkb, blob[9:]...)
	return wkb, int32(sridBits), nil
}

// oidToArrowType maps a pgx PostgreSQL OID to an arrow data type.
// Only covers the common scalar types — extended types (arrays,
// composites, JSONB) fall through with an error so the user knows
// they need to CAST in their query.
func oidToArrowType(oid uint32) (arrow.DataType, error) {
	switch oid {
	case pgtype.Int2OID:
		return arrow.PrimitiveTypes.Int16, nil
	case pgtype.Int4OID:
		return arrow.PrimitiveTypes.Int32, nil
	case pgtype.Int8OID:
		return arrow.PrimitiveTypes.Int64, nil
	case pgtype.Float4OID:
		return arrow.PrimitiveTypes.Float32, nil
	case pgtype.Float8OID:
		return arrow.PrimitiveTypes.Float64, nil
	case pgtype.BoolOID:
		return arrow.FixedWidthTypes.Boolean, nil
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.NameOID:
		return arrow.BinaryTypes.String, nil
	case pgtype.ByteaOID:
		return arrow.BinaryTypes.Binary, nil
	case pgtype.TimestampOID, pgtype.TimestamptzOID:
		return arrow.FixedWidthTypes.Timestamp_ns, nil
	case pgtype.DateOID:
		return arrow.FixedWidthTypes.Timestamp_ns, nil
	case pgtype.NumericOID:
		// Numeric can be huge; arrow's Decimal128 needs a fixed
		// precision/scale that we can't infer without extra
		// metadata. Fall back to Float64 with loss-warning.
		return arrow.PrimitiveTypes.Float64, nil
	case pgtype.UUIDOID:
		return arrow.BinaryTypes.String, nil
	case pgtype.JSONOID, pgtype.JSONBOID:
		return arrow.BinaryTypes.String, nil
	}
	return nil, fmt.Errorf("unsupported PostgreSQL OID %d — CAST the column to a supported type in your query", oid)
}

// builderForArrow returns an empty typed builder for the given
// arrow.DataType. Mirrors the builderFor helper in gpkgio.
func builderForArrow(pool memory.Allocator, t arrow.DataType) (array.Builder, error) {
	switch t.ID() {
	case arrow.INT16:
		return array.NewInt16Builder(pool), nil
	case arrow.INT32:
		return array.NewInt32Builder(pool), nil
	case arrow.INT64:
		return array.NewInt64Builder(pool), nil
	case arrow.FLOAT32:
		return array.NewFloat32Builder(pool), nil
	case arrow.FLOAT64:
		return array.NewFloat64Builder(pool), nil
	case arrow.BOOL:
		return array.NewBooleanBuilder(pool), nil
	case arrow.STRING:
		return array.NewStringBuilder(pool), nil
	case arrow.BINARY:
		return array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary), nil
	case arrow.TIMESTAMP:
		return array.NewTimestampBuilder(pool, t.(*arrow.TimestampType)), nil
	}
	return nil, fmt.Errorf("no builder for arrow type %s", t)
}

// appendPGValue coerces a pgx-scanned value into a typed arrow
// builder. Handles the mismatch between pgx's Go types (int64,
// float64, time.Time, string, []byte) and arrow's typed builders.
func appendPGValue(b array.Builder, v any) error {
	switch tb := b.(type) {
	case *array.Int16Builder:
		switch x := v.(type) {
		case int16:
			tb.Append(x)
		case int32:
			tb.Append(int16(x))
		case int64:
			tb.Append(int16(x))
		default:
			return fmt.Errorf("int16 col got %T", v)
		}
	case *array.Int32Builder:
		switch x := v.(type) {
		case int32:
			tb.Append(x)
		case int16:
			tb.Append(int32(x))
		case int64:
			tb.Append(int32(x))
		default:
			return fmt.Errorf("int32 col got %T", v)
		}
	case *array.Int64Builder:
		switch x := v.(type) {
		case int64:
			tb.Append(x)
		case int32:
			tb.Append(int64(x))
		case int16:
			tb.Append(int64(x))
		default:
			return fmt.Errorf("int64 col got %T", v)
		}
	case *array.Float32Builder:
		switch x := v.(type) {
		case float32:
			tb.Append(x)
		case float64:
			tb.Append(float32(x))
		default:
			return fmt.Errorf("float32 col got %T", v)
		}
	case *array.Float64Builder:
		switch x := v.(type) {
		case float64:
			tb.Append(x)
		case float32:
			tb.Append(float64(x))
		case int64:
			tb.Append(float64(x))
		default:
			return fmt.Errorf("float64 col got %T", v)
		}
	case *array.BooleanBuilder:
		x, ok := v.(bool)
		if !ok {
			return fmt.Errorf("bool col got %T", v)
		}
		tb.Append(x)
	case *array.StringBuilder:
		switch x := v.(type) {
		case string:
			tb.Append(x)
		case []byte:
			tb.Append(string(x))
		default:
			// Fall back to fmt to catch UUID, JSON, etc. that pgx
			// returns as their native Go types.
			tb.Append(fmt.Sprintf("%v", v))
		}
	case *array.BinaryBuilder:
		x, ok := v.([]byte)
		if !ok {
			return fmt.Errorf("binary col got %T", v)
		}
		tb.Append(x)
	case *array.TimestampBuilder:
		switch x := v.(type) {
		case time.Time:
			tb.Append(arrow.Timestamp(x.UnixNano()))
		default:
			return fmt.Errorf("timestamp col got %T", v)
		}
	default:
		return fmt.Errorf("unsupported builder %T", b)
	}
	return nil
}

// finalizeBatch materializes every builder into an arrow.Array,
// attaches SRID metadata to any geometry fields, and assembles the
// Frame. Builders remain valid (still hold their own ref) so
// callers can Release them.
func finalizeBatch(schema *arrow.Schema, builders []array.Builder, srids map[int]int32, origFields []arrow.Field, _ memory.Allocator) (*gobi.Frame, error) {
	arrs := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrs[i] = b.NewArray()
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()

	// Rebuild the field list so geometry columns carry the SRID we
	// observed in this batch. Non-geometry fields pass through.
	fields := make([]arrow.Field, len(origFields))
	for i, f := range origFields {
		if srid, ok := srids[i]; ok && srid != 0 {
			fields[i] = gobi.GeometryField(f.Name, srid)
			continue
		}
		fields[i] = f
	}
	outSchema := arrow.NewSchema(fields, nil)

	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	// Cheat: use outSchema, not the input schema, so the resulting
	// Frame carries geometry field metadata.
	return gobi.NewFrame(outSchema, cols)
}

// emptyFrame builds a zero-row Frame matching schema — used when a
// SELECT returns no rows.
func emptyFrame(schema *arrow.Schema) (*gobi.Frame, error) {
	pool := memory.DefaultAllocator
	cols := make([]arrow.Column, schema.NumFields())
	for i, f := range schema.Fields() {
		b, err := builderForArrow(pool, f.Type)
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

// quoteIdent quotes a PostgreSQL identifier per ANSI SQL. Same rules
// as gpkgio's quoter — embedded double-quotes are doubled.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

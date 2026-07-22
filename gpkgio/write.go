package gpkgio

import (
	"database/sql"
	_ "embed"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// initMetadataSQL is the DDL for the three GeoPackage 1.3 mandatory
// metadata tables. Kept as a sibling .sql file (rather than a Go
// string literal) so the SQL stays readable in editors and diff
// tools that syntax-highlight it, and so future extension DDL can
// land alongside without cluttering write.go.
//
//go:embed schema/init_metadata.sql
var initMetadataSQL string

// splitSQLStatements chops a multi-statement SQL blob into
// individual statements ready for db.Exec. Handles:
//
//   - `;`-terminated statements (each becomes one Exec call)
//   - `--` line comments (stripped before the split so a semicolon
//     inside a comment doesn't accidentally end a statement)
//   - blank lines / whitespace-only fragments (dropped)
//
// Not a full SQL tokenizer — doesn't understand string literals or
// nested block comments. Fine for our DDL, where semicolons only
// appear as statement terminators. If we ever need to embed DDL
// with semicolons inside string literals, switch to a real parser.
func splitSQLStatements(blob string) []string {
	// Strip line comments first so `-- foo ; bar` doesn't split.
	// SplitSeq (Go 1.24+) iterates without allocating a slice.
	var b strings.Builder
	for line := range strings.SplitSeq(blob, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var out []string
	for part := range strings.SplitSeq(b.String(), ";") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// WriteOptions controls WriteFile / Writer behavior.
type WriteOptions struct {
	// Layer names the feature table to create/replace. Required.
	// Must be a valid SQLite identifier (letters, digits, underscore,
	// not starting with a digit) — WriteFile will reject anything
	// else so we can safely interpolate it into DDL without a full
	// quoting round-trip.
	Layer string

	// GeomCol names the geometry column in the output layer.
	// Defaults to the first Frame column tagged as WKB geometry
	// (via gobi.GeometryField). If the Frame has no geometry
	// column at all, the file is written as an attribute-only
	// table (still valid GeoPackage, just no gpkg_geometry_columns
	// entry for this layer).
	GeomCol string

	// SRID is the spatial reference identifier stamped into every
	// row's GPB header + registered in gpkg_contents. Defaults to
	// the value carried by the geometry column's schema metadata
	// (gobi.MetaGeometryCRS); if that's unset, defaults to 4326.
	//
	// The corresponding row in gpkg_spatial_ref_sys is inserted
	// on-demand — the default 4326 (WGS84) is always registered
	// even for attribute-only layers so the file passes strict
	// validators.
	SRID int32

	// BatchSize is the number of rows per transaction commit. Higher
	// values reduce commit overhead but grow the WAL / journal on
	// crash-safety trade-off. 1000 is a reasonable default matching
	// GDAL's ogr2ogr for GPKG output.
	BatchSize int

	// SkipRTree, when true, disables creation of the standard
	// GeoPackage rtree_<layer>_<geomcol> shadow table. Skipped
	// automatically when the Frame has no geometry column.
	//
	// The RTree is created by default because the per-insert
	// overhead is small and it unlocks fast bbox filtering on
	// downstream reads (both from gobi and from external tools
	// like QGIS / GDAL). Use this opt-out only when you're
	// writing intermediate scratch files where read-side
	// filtering doesn't matter.
	SkipRTree bool

	// Replace, when true, drops any pre-existing layer with the
	// same name (feature table + gpkg_contents row + gpkg_geometry_columns
	// row + RTree shadow tables + triggers) before creating the new
	// one. When false, WriteFile errors if the layer already exists —
	// the safer default that keeps us from silently clobbering data
	// in an existing multi-layer GeoPackage.
	Replace bool
}

// colWriter reads one row's value from a Series and returns it in a
// form the SQLite driver understands (Go primitives, []byte for
// binary, string for text, nil for null). Constructed once per
// column at DDL build time and reused across every row.
type colWriter struct {
	name string
	read func(row int) (any, error)
}

// defaultWriteOptions returns a copy of opts with zero-value fields
// filled in. Never mutates the caller's struct.
func defaultWriteOptions(opts *WriteOptions) WriteOptions {
	out := WriteOptions{
		SRID:     4326,
		BatchSize: 1000,
	}
	if opts != nil {
		if opts.Layer != "" {
			out.Layer = opts.Layer
		}
		if opts.GeomCol != "" {
			out.GeomCol = opts.GeomCol
		}
		if opts.SRID != 0 {
			out.SRID = opts.SRID
		}
		if opts.BatchSize > 0 {
			out.BatchSize = opts.BatchSize
		}
		out.SkipRTree = opts.SkipRTree
		out.Replace = opts.Replace
	}
	return out
}

// WriteFile writes df to a GeoPackage file at path, creating the file
// if it doesn't exist. When the file already exists, the layer is
// appended (or replaced when opts.Replace is true) — other layers
// stay untouched.
//
// The Frame's geometry column (if any) is detected via the arrow
// schema metadata that gobi.GeometryField() sets; alternatively
// opts.GeomCol can name it explicitly. Non-geometry columns are
// written as native SQLite affinity types based on the Frame's
// arrow schema:
//
//	Int8/16/32/64, Uint8/16/32/64 → INTEGER
//	Float32/64                    → REAL
//	Bool                          → INTEGER (0 or 1)
//	String, LargeString           → TEXT
//	Binary, LargeBinary           → BLOB
//	Timestamp                     → TEXT (ISO 8601)
//	other                         → TEXT via string(v) fallback
//
// Batch inserts run in a single transaction of opts.BatchSize rows
// each. Prepared statements are reused across the write.
func WriteFile(df *gobi.Frame, path string, opts *WriteOptions) error {
	o := defaultWriteOptions(opts)
	if o.Layer == "" {
		return fmt.Errorf("gpkg: WriteFile: Layer is required")
	}
	if !validSQLIdent(o.Layer) {
		return fmt.Errorf("gpkg: WriteFile: Layer %q is not a valid SQLite identifier", o.Layer)
	}

	// Detect the geometry column if the caller didn't name one.
	geomIdx := -1
	if o.GeomCol != "" {
		i, err := columnIndex(df, o.GeomCol)
		if err != nil {
			return err
		}
		geomIdx = i
	} else {
		for i := 0; i < df.NumCols(); i++ {
			s, err := df.ColumnAt(i)
			if err != nil {
				return err
			}
			if s.IsGeometry() {
				geomIdx = i
				o.GeomCol = s.Name()
				break
			}
		}
	}
	// If a geometry column was requested but the Frame's field lacks
	// the geometry marker, still write it as BLOB — but skip the
	// gpkg_geometry_columns registration so the resulting file isn't
	// ambiguously advertised as a feature table.
	geomOK := geomIdx >= 0
	if geomOK {
		s, _ := df.ColumnAt(geomIdx)
		if !s.IsGeometry() {
			geomOK = false
		}
		if o.SRID == 4326 {
			// Pick up the geometry field's declared CRS if the caller
			// left SRID at the default — matches user expectation
			// that "the geometry says it's in EPSG:X" flows through.
			if epsg := geometryEPSGFromField(s.Column().Field()); epsg > 0 {
				o.SRID = epsg
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()

	// Serialize writes on a single connection. modernc.org/sqlite
	// is fine with concurrent readers but writers on the same file
	// must run one at a time; keeping the pool at 1 conn removes
	// any ambiguity about which conn holds the transaction.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("gpkg: enable WAL: %w", err)
	}
	// application_id + user_version are what makes the file
	// recognizable as GeoPackage 1.3 by QGIS / GDAL / ogr2ogr.
	// application_id = ASCII "GPKG" (0x47504B47) per spec §1.1.1.1;
	// user_version = 10300 → GeoPackage 1.3.
	if _, err := db.Exec(`PRAGMA application_id = 1196444487`); err != nil {
		return fmt.Errorf("gpkg: set application_id: %w", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 10300`); err != nil {
		return fmt.Errorf("gpkg: set user_version: %w", err)
	}

	if err := initMetadataTables(db); err != nil {
		return fmt.Errorf("gpkg: init metadata: %w", err)
	}
	if err := registerSRS(db, o.SRID); err != nil {
		return fmt.Errorf("gpkg: register srs: %w", err)
	}
	if o.Replace {
		if err := dropLayer(db, o.Layer); err != nil {
			return fmt.Errorf("gpkg: drop existing layer: %w", err)
		}
	}
	if err := layerExists(db, o.Layer); err != nil {
		return err
	}

	// Build feature table DDL from the frame schema. The geometry
	// column, if any, gets a BLOB affinity so the raw GPB blob fits.
	tableDDL, colDDL, err := buildFeatureTableDDL(df, o.Layer, geomIdx)
	if err != nil {
		return err
	}
	if _, err := db.Exec(tableDDL); err != nil {
		return fmt.Errorf("gpkg: create layer table: %w\nSQL: %s", err, tableDDL)
	}

	// Register in gpkg_contents + gpkg_geometry_columns (feature
	// tables only). gpkg_contents.last_change is set to
	// datetime('now') so QGIS shows a fresh timestamp on the layer.
	if err := registerLayerContents(db, o.Layer, o.SRID); err != nil {
		return fmt.Errorf("gpkg: register layer: %w", err)
	}
	if geomOK {
		geomTypeName, hasZ, hasM := geomTypeForColumn(df, geomIdx)
		if _, err := db.Exec(`
			INSERT OR REPLACE INTO gpkg_geometry_columns
			  (table_name, column_name, geometry_type_name, srs_id, z, m)
			VALUES (?, ?, ?, ?, ?, ?)`,
			o.Layer, o.GeomCol, geomTypeName, o.SRID, boolToInt(hasZ), boolToInt(hasM)); err != nil {
			return fmt.Errorf("gpkg: register geometry column: %w", err)
		}
	}

	// Optional RTree — see rtree.go. Skipped when there's no geometry.
	if !o.SkipRTree && geomOK {
		if err := createRTree(db, o.Layer, o.GeomCol); err != nil {
			return fmt.Errorf("gpkg: create rtree: %w", err)
		}
	}

	// Insert rows in batches.
	return insertRows(db, df, o, geomIdx, colDDL)
}

// insertRows runs the actual batched INSERT loop. Wraps every
// opts.BatchSize rows in a single transaction for throughput; uses
// one prepared statement across the whole write. Also accumulates
// the layer's extent (min/max x/y) and, at the end, writes it into
// gpkg_contents.
func insertRows(db *sql.DB, df *gobi.Frame, opts WriteOptions, geomIdx int, cols []colWriter) error {
	if len(cols) == 0 || df.NumRows() == 0 {
		return nil
	}
	// Build the INSERT statement text once. Column names + `?`
	// placeholders in the same order as `cols`.
	names := make([]string, len(cols))
	qs := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.name)
		qs[i] = "?"
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		quoteIdent(opts.Layer),
		strings.Join(names, ", "),
		strings.Join(qs, ", "))

	// If RTree is enabled + there's a geometry column, the insert
	// loop also writes into the shadow table. Direct maintenance
	// because pure-Go SQLite lacks the SpatiaLite ST_* functions
	// the spec's example triggers rely on. See rtree.go for the
	// full rationale.
	rtreeSQL := ""
	if !opts.SkipRTree && geomIdx >= 0 {
		rtreeSQL = fmt.Sprintf("INSERT INTO %s (id, minx, maxx, miny, maxy) VALUES (?, ?, ?, ?, ?)",
			quoteIdent(rtreeTableName(opts.Layer, opts.GeomCol)))
	}

	extent := geometry.EmptyBounds()

	nRows := df.NumRows()
	batchSize := opts.BatchSize
	scratch := make([]byte, 0, 64) // reused for GPB encoding

	for start := 0; start < nRows; start += batchSize {
		end := min(start+batchSize, nRows)
		if err := insertBatch(db, insertSQL, rtreeSQL, cols, geomIdx, opts.SRID, start, end, &extent, &scratch); err != nil {
			return err
		}
	}

	// Final bounds → gpkg_contents.
	if !extent.Empty() {
		if _, err := db.Exec(`
			UPDATE gpkg_contents
			SET min_x = ?, min_y = ?, max_x = ?, max_y = ?, last_change = datetime('now')
			WHERE table_name = ?`,
			extent.MinX, extent.MinY, extent.MaxX, extent.MaxY, opts.Layer); err != nil {
			return fmt.Errorf("gpkg: update contents bounds: %w", err)
		}
	}
	return nil
}

// insertBatch runs one transaction with one prepared statement for
// rows [start, end). When rtreeSQL is non-empty, a second prepared
// statement writes each geometry-carrying row's envelope into the
// RTree shadow table before the row's fid is even known — we use
// tx.LastInsertId() after each feature INSERT to get the fid the
// AUTOINCREMENT column just assigned.
//
// Accumulates the layer's extent into *extent as a side effect.
// scratch is a reusable []byte for GPB blob building.
func insertBatch(db *sql.DB, insertSQL, rtreeSQL string, cols []colWriter, geomIdx int, srsID int32, start, end int, extent *geometry.Bounds, scratch *[]byte) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("gpkg: prepare insert: %w\nSQL: %s", err, insertSQL)
	}
	defer stmt.Close()

	var rtreeStmt *sql.Stmt
	if rtreeSQL != "" {
		rtreeStmt, err = tx.Prepare(rtreeSQL)
		if err != nil {
			return fmt.Errorf("gpkg: prepare rtree insert: %w\nSQL: %s", err, rtreeSQL)
		}
		defer rtreeStmt.Close()
	}

	vals := make([]any, len(cols))
	// Track the current row's bounds so we can push them into the
	// RTree after INSERTing the feature row. reset to Empty per row.
	rowBounds := geometry.EmptyBounds()
	rowHasGeom := false

	for row := start; row < end; row++ {
		rowBounds = geometry.EmptyBounds()
		rowHasGeom = false
		for i, c := range cols {
			v, err := c.read(row)
			if err != nil {
				return fmt.Errorf("gpkg: read %s row %d: %w", c.name, row, err)
			}
			if i == geomIdxLocal(cols, geomIdx) && v != nil {
				// Geometry: wrap the raw WKB in a GPB header + envelope.
				wkb, ok := v.([]byte)
				if !ok {
					return fmt.Errorf("gpkg: geom column %q row %d not []byte: %T",
						c.name, row, v)
				}
				blob, b, err := encodeGPKGGeometry(*scratch, wkb, srsID)
				if err != nil {
					return fmt.Errorf("gpkg: encode row %d: %w", row, err)
				}
				*scratch = blob
				// Extend the layer extent for gpkg_contents at commit.
				*extent = extent.Union(b)
				rowBounds = b
				rowHasGeom = true
				// Copy blob into a fresh slice so scratch reuse on
				// the next row doesn't overwrite the value the
				// driver is about to marshal.
				payload := make([]byte, len(blob))
				copy(payload, blob)
				vals[i] = payload
				continue
			}
			vals[i] = v
		}
		res, err := stmt.Exec(vals...)
		if err != nil {
			return fmt.Errorf("gpkg: insert row %d: %w", row, err)
		}
		if rtreeStmt != nil && rowHasGeom && !rowBounds.Empty() {
			fid, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("gpkg: last insert id row %d: %w", row, err)
			}
			if _, err := rtreeStmt.Exec(fid, rowBounds.MinX, rowBounds.MaxX, rowBounds.MinY, rowBounds.MaxY); err != nil {
				return fmt.Errorf("gpkg: rtree insert row %d: %w", row, err)
			}
		}
	}
	return tx.Commit()
}

// geomIdxLocal returns the position of the geometry column within the
// cols slice, or -1 if there's no geometry. Since cols is built in
// the same order as the Frame's columns, the frame's geomIdx aligns
// with the same index in cols.
func geomIdxLocal(cols []colWriter, geomIdx int) int {
	if geomIdx < 0 || geomIdx >= len(cols) {
		return -1
	}
	return geomIdx
}

// buildFeatureTableDDL emits the CREATE TABLE statement for the
// feature layer + returns a list of colWriter closures that will
// pull typed values out of the frame at row R. The output columns
// order matches the frame's schema so index alignment is preserved.
//
// The primary key column is named "fid" per GeoPackage convention;
// callers can rely on it being present + auto-incrementing. If the
// frame already has a column named "fid" we use it verbatim
// (assumed integer); otherwise we insert an implicit fid.
func buildFeatureTableDDL(df *gobi.Frame, table string, geomIdx int) (string, []colWriter, error) {
	var (
		parts   []string
		writers []colWriter
		hasFID  bool
	)
	names := df.ColumnNames()
	for i, name := range names {
		s, err := df.ColumnAt(i)
		if err != nil {
			return "", nil, err
		}
		if name == "fid" {
			hasFID = true
		}
		affinity := sqliteAffinity(s.DataType(), i == geomIdx)
		parts = append(parts, fmt.Sprintf("%s %s", quoteIdent(name), affinity))
		w, err := columnWriter(s, name)
		if err != nil {
			return "", nil, err
		}
		writers = append(writers, w)
	}
	// Add fid column at the front if the frame doesn't already have one.
	// GeoPackage feature tables MUST have an INTEGER PRIMARY KEY column.
	if !hasFID {
		parts = append([]string{`"fid" INTEGER PRIMARY KEY AUTOINCREMENT`}, parts...)
	} else {
		// Rewrite the existing fid column to be a PK. This is
		// best-effort — if the caller's fid type isn't Int, the DDL
		// will fail at CREATE and they'll see a clear error.
		for i, p := range parts {
			if strings.HasPrefix(p, `"fid" `) {
				parts[i] = `"fid" INTEGER PRIMARY KEY AUTOINCREMENT`
				break
			}
		}
	}

	ddl := fmt.Sprintf("CREATE TABLE %s (%s)",
		quoteIdent(table),
		strings.Join(parts, ", "))
	return ddl, writers, nil
}

// columnWriter picks the read closure for one Series based on its
// arrow type. Falls back to a generic readScalarAt-shaped path for
// anything not in the fast-type list.
func columnWriter(s gobi.Series, name string) (colWriter, error) {
	dt := s.DataType()
	// One-chunk fast path: pull the concrete arrow.Array once and
	// index into it directly. Multi-chunk fallback goes through
	// gobi's row-walker.
	chunks := s.Column().Data().Chunks()
	if len(chunks) == 1 {
		if w, ok := singleChunkColumnWriter(name, chunks[0]); ok {
			return w, nil
		}
	}
	// Fall through: no fast path for this arrow type. gpkg's write
	// path can't marshal arbitrary types into SQLite without picking
	// an affinity, so this is an outright error rather than a slow
	// fallback. Add a case to singleChunkColumnWriter if you need
	// support for a new type.
	return colWriter{}, fmt.Errorf("gpkg: unsupported column type %s for column %q", dt, name)
}

// singleChunkColumnWriter returns a fast read closure keyed on the
// concrete arrow.Array type of the single-chunk column. ok=false
// when the type isn't in the supported list — caller falls back to
// the generic path.
func singleChunkColumnWriter(name string, chunk arrow.Array) (colWriter, bool) {
	switch a := chunk.(type) {
	case *array.Int64:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}}, true
	case *array.Int32:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return int64(a.Value(row)), nil
		}}, true
	case *array.Uint64:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return int64(a.Value(row)), nil
		}}, true
	case *array.Uint32:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return int64(a.Value(row)), nil
		}}, true
	case *array.Float64:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}}, true
	case *array.Float32:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return float64(a.Value(row)), nil
		}}, true
	case *array.Boolean:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			if a.Value(row) {
				return int64(1), nil
			}
			return int64(0), nil
		}}, true
	case *array.String:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}}, true
	case *array.LargeString:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}}, true
	case *array.Binary:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}}, true
	case *array.Timestamp:
		return colWriter{name: name, read: func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			// Store as raw int64 for simplicity — full ISO 8601 encoding
			// requires the timestamp's declared unit which we don't
			// track here. Users who need TEXT ISO strings can cast
			// before writing.
			return int64(a.Value(row)), nil
		}}, true
	}
	return colWriter{}, false
}

// sqliteAffinity picks a SQLite affinity for the arrow type. Follows
// SQLite's 5 rules (§3 of the CREATE TABLE docs): INTEGER, TEXT,
// BLOB, REAL, NUMERIC. Geometry columns are always BLOB.
func sqliteAffinity(dt arrow.DataType, isGeom bool) string {
	if isGeom {
		return "BLOB"
	}
	switch dt.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64,
		arrow.BOOL, arrow.TIMESTAMP:
		return "INTEGER"
	case arrow.FLOAT16, arrow.FLOAT32, arrow.FLOAT64:
		return "REAL"
	case arrow.STRING, arrow.LARGE_STRING:
		return "TEXT"
	case arrow.BINARY, arrow.LARGE_BINARY, arrow.FIXED_SIZE_BINARY:
		return "BLOB"
	}
	return "TEXT"
}

// -----------------------------------------------------------------------------
// GPB blob encoding
// -----------------------------------------------------------------------------

// encodeGPKGGeometry wraps wkb in an OGC GeoPackage Binary header
// with the standard XY envelope (flags = 0x01 << 1 = 0x02, plus
// bit 0 = 1 for little-endian). Appends into dst so callers can
// reuse a scratch buffer across rows. Returns the appended slice
// plus the parsed bounds so the layer's extent can accumulate.
//
// Layout (spec §2.1.3.1):
//
//	byte  0..1  magic "GP"
//	byte  2     version (0)
//	byte  3     flags (bits: 0=byte-order, 3..1=envelope kind, 4=empty, 5=binary type, 6..7=reserved)
//	byte  4..7  SRS ID (little-endian int32)
//	byte  8..39 envelope minX, maxX, minY, maxY (float64 LE) — 32 bytes total for XY
//	byte 40..   WKB payload
func encodeGPKGGeometry(dst, wkb []byte, srsID int32) ([]byte, geometry.Bounds, error) {
	g, err := geometry.ParseWKB(wkb)
	if err != nil {
		return nil, geometry.EmptyBounds(), fmt.Errorf("parse wkb: %w", err)
	}
	b := g.Bounds()

	dst = dst[:0]
	dst = append(dst, 'G', 'P')
	dst = append(dst, 0) // version
	// Flags: LE = 1, envelope = 1 (XY = 32 bytes).
	// Bits: 7..6 = 00 reserved, 5 = 0 standard, 4 = 0 non-empty, 3..1 = 001 XY, 0 = 1 LE.
	dst = append(dst, 0b0000_0011)
	var srs [4]byte
	binary.LittleEndian.PutUint32(srs[:], uint32(srsID))
	dst = append(dst, srs[:]...)

	var env [32]byte
	binary.LittleEndian.PutUint64(env[0:8], f64bits(b.MinX))
	binary.LittleEndian.PutUint64(env[8:16], f64bits(b.MaxX))
	binary.LittleEndian.PutUint64(env[16:24], f64bits(b.MinY))
	binary.LittleEndian.PutUint64(env[24:32], f64bits(b.MaxY))
	dst = append(dst, env[:]...)

	dst = append(dst, wkb...)
	return dst, b, nil
}

// f64bits reinterprets a float64 as its IEEE-754 bit pattern. Uses
// math.Float64bits — the standard library's canonical implementation
// handles NaN payloads correctly and inlines cleanly.
func f64bits(v float64) uint64 { return math.Float64bits(v) }

// -----------------------------------------------------------------------------
// Metadata + helpers
// -----------------------------------------------------------------------------

// initMetadataTables ensures every mandatory GeoPackage metadata
// table exists. Idempotent — safe to call on an existing file. The
// three tables required by spec §1.1.3 are:
//
//	gpkg_spatial_ref_sys      — coordinate reference system registry
//	gpkg_contents             — table of contents (one row per layer)
//	gpkg_geometry_columns     — geometry-column descriptor per feature layer
//
// The DDL lives in schema/init_metadata.sql (embedded above via
// //go:embed) so the SQL stays syntax-highlightable and any future
// change to spec-compliance shows up as a clean diff.
//
// Two additional tables (gpkg_extensions, gpkg_ogr_contents) are
// created elsewhere: gpkg_extensions comes from createRTree when a
// spatial index is emitted; gpkg_ogr_contents isn't required and
// isn't written.
func initMetadataTables(db *sql.DB) error {
	for _, s := range splitSQLStatements(initMetadataSQL) {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec: %w\nSQL: %s", err, s)
		}
	}
	// Insert the three mandatory SRS rows the spec requires:
	// -1 (undefined cartesian), 0 (undefined geographic), 4326 (WGS84).
	// INSERT OR IGNORE keeps us idempotent when opening an existing file.
	seeds := []struct {
		name string
		id   int32
		org  string
		code int32
		def  string
		desc string
	}{
		{"Undefined cartesian SRS", -1, "NONE", -1, "undefined", "undefined cartesian coordinate reference system"},
		{"Undefined geographic SRS", 0, "NONE", 0, "undefined", "undefined geographic coordinate reference system"},
		{"WGS 84 geodetic", 4326, "EPSG", 4326, wkt4326, "WGS 84 (long, lat) — EPSG:4326"},
	}
	for _, s := range seeds {
		if _, err := db.Exec(`
			INSERT OR IGNORE INTO gpkg_spatial_ref_sys
			  (srs_name, srs_id, organization, organization_coordsys_id, definition, description)
			VALUES (?, ?, ?, ?, ?, ?)`,
			s.name, s.id, s.org, s.code, s.def, s.desc); err != nil {
			return fmt.Errorf("seed srs %d: %w", s.id, err)
		}
	}
	return nil
}

// registerSRS inserts a placeholder row for a custom SRS ID that
// isn't one of the pre-seeded defaults. Uses INSERT OR IGNORE so
// this is a no-op when the SRS row already exists.
func registerSRS(db *sql.DB, srsID int32) error {
	// Already seeded by initMetadataTables — skip the extra insert.
	if srsID == -1 || srsID == 0 || srsID == 4326 {
		return nil
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO gpkg_spatial_ref_sys
		  (srs_name, srs_id, organization, organization_coordsys_id, definition, description)
		VALUES (?, ?, ?, ?, ?, ?)`,
		fmt.Sprintf("EPSG:%d", srsID), srsID, "EPSG", srsID, "unknown", "registered by gobi")
	return err
}

// registerLayerContents inserts / replaces the layer's row in
// gpkg_contents. Bounds are set to NULL initially; updated at end
// of write with the accumulated extent.
func registerLayerContents(db *sql.DB, layer string, srsID int32) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO gpkg_contents
		  (table_name, data_type, identifier, description, last_change, srs_id)
		VALUES (?, 'features', ?, '', datetime('now'), ?)`,
		layer, layer, srsID)
	return err
}

// dropLayer removes every artifact of a previous layer with the
// same name: the feature table, its RTree shadow table, and the
// gpkg_contents / gpkg_geometry_columns rows. Gobi maintains the
// RTree directly rather than via SpatiaLite triggers (see rtree.go),
// so there are no triggers to drop.
//
// If external tooling (GDAL / QGIS) opened the file and installed
// its own trigger cluster, those triggers reference the feature
// table by name and will be dropped automatically by SQLite when
// the feature table is dropped. So this function stays trigger-
// agnostic without needing to enumerate every possible trigger
// naming scheme.
func dropLayer(db *sql.DB, layer string) error {
	var geomCol string
	if err := db.QueryRow(`SELECT column_name FROM gpkg_geometry_columns WHERE table_name = ?`, layer).Scan(&geomCol); err != nil && err != sql.ErrNoRows {
		return err
	}
	// Drop the RTree shadow table first; when it exists, it has an
	// FK-like relationship to the feature table via fid values, and
	// dropping the feature table doesn't cascade.
	if geomCol != "" {
		rtreeName := rtreeTableName(layer, geomCol)
		if _, err := db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, quoteIdent(rtreeName))); err != nil {
			return fmt.Errorf("drop rtree: %w", err)
		}
		// Any triggers the external tools may have installed
		// against the feature table will disappear when the table
		// itself is dropped — SQLite drops table-bound triggers
		// automatically.
	}
	if _, err := db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, quoteIdent(layer))); err != nil {
		return fmt.Errorf("drop feature table: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM gpkg_contents WHERE table_name = ?`, layer); err != nil {
		return fmt.Errorf("delete gpkg_contents row: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM gpkg_geometry_columns WHERE table_name = ?`, layer); err != nil {
		return fmt.Errorf("delete gpkg_geometry_columns row: %w", err)
	}
	return nil
}

// layerExists returns an error when the named layer already exists
// in gpkg_contents. Used to fail-loud when opts.Replace is false.
func layerExists(db *sql.DB, layer string) error {
	var name string
	err := db.QueryRow(`SELECT table_name FROM gpkg_contents WHERE table_name = ?`, layer).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("gpkg: layer %q already exists (set WriteOptions.Replace=true to overwrite)", layer)
}

// geomTypeForColumn peeks at the first non-null WKB blob to infer
// the geometry type name for gpkg_geometry_columns. Returns the
// generic "GEOMETRY" (a valid GeoPackage type) if the column is
// entirely null or contains an unrecognized geometry.
//
// A stricter implementation would peek at multiple rows and pick
// MULTIPOINT / MULTILINESTRING / MULTIPOLYGON when the column is
// mixed — but GeoPackage's "GEOMETRY" is a valid fallback that
// keeps QGIS and GDAL happy, so the extra logic doesn't pay off
// until users complain.
func geomTypeForColumn(df *gobi.Frame, geomIdx int) (typeName string, hasZ bool, hasM bool) {
	if geomIdx < 0 {
		return "GEOMETRY", false, false
	}
	s, err := df.ColumnAt(geomIdx)
	if err != nil {
		return "GEOMETRY", false, false
	}
	wkb, ok := firstNonNullBinary(s)
	if !ok {
		return "GEOMETRY", false, false
	}
	g, err := geometry.ParseWKB(wkb)
	if err != nil {
		return "GEOMETRY", false, false
	}
	return geomTypeName(g), false, false
}

// firstNonNullBinary returns the first non-null value from a
// Binary-typed Series as raw bytes. Walks arrow chunks by hand
// because gobi doesn't export a Series-value accessor.
func firstNonNullBinary(s gobi.Series) ([]byte, bool) {
	for _, chunk := range s.Column().Data().Chunks() {
		ba, ok := chunk.(*array.Binary)
		if !ok {
			return nil, false
		}
		for i := 0; i < ba.Len(); i++ {
			if ba.IsNull(i) {
				continue
			}
			return ba.Value(i), true
		}
	}
	return nil, false
}

// geometryEPSGFromField reads the "gobi:crs_epsg" metadata entry a
// gobi.GeometryField() writes and parses it back to an int32.
// Returns 0 when the field has no such metadata — same convention
// gobi's internal geometryCRSFromField uses.
func geometryEPSGFromField(f arrow.Field) int32 {
	v, ok := f.Metadata.GetValue(gobi.MetaGeometryCRS)
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

// geomTypeName maps a gobi geometry type to the GeoPackage
// geometry_type_name enumeration (spec §2.1.4). All values are
// uppercase per spec table 3.
func geomTypeName(g geometry.Geometry) string {
	switch g.(type) {
	case geometry.Point:
		return "POINT"
	case geometry.LineString:
		return "LINESTRING"
	case geometry.Polygon:
		return "POLYGON"
	case geometry.MultiPoint:
		return "MULTIPOINT"
	case geometry.MultiLineString:
		return "MULTILINESTRING"
	case geometry.MultiPolygon:
		return "MULTIPOLYGON"
	case geometry.GeometryCollection:
		return "GEOMETRYCOLLECTION"
	}
	return "GEOMETRY"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// validSQLIdent restricts identifiers to a conservative subset so
// downstream string interpolation into DDL can't inject SQL. Users
// who want unusual names (spaces, dashes, unicode) can pre-quote
// them themselves before calling WriteFile — the identifier still
// has to survive the metadata table row.
func validSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' {
			continue
		}
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' && i > 0 {
			continue
		}
		return false
	}
	return true
}

// columnIndex finds a column by name; errors with a helpful message
// listing the available names when not found.
func columnIndex(df *gobi.Frame, name string) (int, error) {
	names := df.ColumnNames()
	for i, n := range names {
		if n == name {
			return i, nil
		}
	}
	return -1, fmt.Errorf("gpkg: column %q not found; available: %v", name, names)
}

// wkt4326 is the OGC-standard WKT string for EPSG:4326 (WGS 84).
// Written into gpkg_spatial_ref_sys.definition — GDAL/QGIS parse
// this on layer open, so having a proper definition (not just
// "undefined") avoids the "SRS not recognized" warning on read.
const wkt4326 = `GEOGCS["WGS 84",DATUM["WGS_1984",SPHEROID["WGS 84",6378137,298.257223563,AUTHORITY["EPSG","7030"]],AUTHORITY["EPSG","6326"]],PRIMEM["Greenwich",0,AUTHORITY["EPSG","8901"]],UNIT["degree",0.0174532925199433,AUTHORITY["EPSG","9122"]],AUTHORITY["EPSG","4326"]]`

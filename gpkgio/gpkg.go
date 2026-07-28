// Package gpkgio reads and writes gobi Frames as OGC GeoPackage
// (SQLite) files.
//
// GeoPackage 1.3 (application_id 0x47504B47, user_version 10300) is
// the target — every WriteFile emits a file that QGIS, GDAL /
// ogr2ogr, and other GeoPackage-aware tools recognize as a
// compliant feature GeoPackage. Each geometry is stored as the
// standard GPB blob (magic + version + flags + SRS ID + optional
// envelope + WKB payload) per spec §2.1.3.1.
//
// The package offers three entry points:
//
//   - ReadFile materializes a single layer as a Frame. Peak memory
//     scales with the layer size; good for small/medium layers.
//
//   - ReadFileChunksFunc streams a layer as record-batch-sized
//     Frames, releasing arrow buffers after each callback. Peak
//     memory is bounded regardless of layer size.
//
//   - ScanFile returns a gobi.LazyFrame — participates in the
//     optimizer's projection pushdown (SELECT column list is
//     narrowed to what the plan actually uses) and streams under
//     the hood. Predicate-pushdown-to-SQL is not implemented yet;
//     callers who need SQL-side filtering can use ReadOptions.Where
//     directly.
//
// Write is transactional and prepared-statement-based: rows batch
// into transactions of WriteOptions.BatchSize (default 1000). The
// standard GeoPackage RTree spatial index (rtree_<layer>_<geomcol>)
// is created and populated inline during the insert loop; gobi
// maintains it from Go rather than via SpatiaLite triggers, since
// pure-Go modernc.org/sqlite doesn't ship ST_MinX/ST_MaxX/ST_IsEmpty.
//
// Multi-layer GeoPackages are supported: each WriteFile appends its
// layer to the target file, leaving other layers untouched. Use
// WriteOptions.Replace to overwrite an existing layer of the same
// name.
package gpkgio

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	// modernc.org/sqlite is a pure-Go SQLite driver; requires no cgo.
	_ "modernc.org/sqlite"

	"github.com/zoobst/gobi/geometry"
)

// ErrInvalidHeader is returned when a geometry blob lacks the "GP" magic.
var ErrInvalidHeader = errors.New("gpkg: invalid geometry header")

// FeatureTable describes one feature table registered in gpkg_geometry_columns.
type FeatureTable struct {
	Name     string
	GeomCol  string
	SRID    int32
	GeomType string
}

// GeoPackage is an open GeoPackage database.
type GeoPackage struct {
	db *sql.DB
}

// Open opens the GeoPackage file at path.
func Open(path string) (*GeoPackage, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &GeoPackage{db: db}, nil
}

// Close releases the database handle.
func (g *GeoPackage) Close() error { return g.db.Close() }

// LayerNames returns the names of every registered feature table in
// the file, in gpkg_geometry_columns order. Cheap — one metadata
// query. Useful when the caller only needs a list to iterate over
// (see also FeatureTables for the richer struct-per-layer shape).
func (g *GeoPackage) LayerNames() ([]string, error) {
	tables, err := g.FeatureTables()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(tables))
	for i, t := range tables {
		out[i] = t.Name
	}
	return out, nil
}

// SumColumn returns SUM(col) over every row of layer as a Float64.
// Integer columns are promoted; TEXT and BLOB columns error at
// SQLite (SUM on non-numeric values yields 0 or NULL depending on
// content). Returns 0 when the layer has no rows or every value is
// null.
//
// The whole computation runs inside SQLite — no WKB decode, no
// Go-side row iteration, no builder allocations. Constant memory
// regardless of layer size. Intended for the "rank layers by a
// summary metric before deciding which to keep" pattern where the
// geometry column is dead weight for the ranking step.
//
// Errors on unknown layer or column, or if the caller-supplied
// name isn't a safe SQL identifier.
func (g *GeoPackage) SumColumn(layer, col string) (float64, error) {
	if !validSQLIdent(layer) {
		return 0, fmt.Errorf("gpkg: SumColumn: layer %q is not a valid SQLite identifier", layer)
	}
	if !validSQLIdent(col) {
		return 0, fmt.Errorf("gpkg: SumColumn: column %q is not a valid SQLite identifier", col)
	}
	// SQLite's legacy "double-quoted identifier falls back to string
	// literal" behavior turns SUM("bogus_col") into SUM('bogus_col')
	// silently, which coerces to 0 instead of raising. Verify the
	// column exists first via PRAGMA table_info so the caller gets a
	// clear error on typos rather than a bogus 0.
	if err := requireColumn(g.db, layer, col); err != nil {
		return 0, err
	}
	query := fmt.Sprintf("SELECT COALESCE(SUM(%s), 0) FROM %s",
		quoteIdent(col), quoteIdent(layer))
	var sum float64
	if err := g.db.QueryRow(query).Scan(&sum); err != nil {
		return 0, fmt.Errorf("gpkg: SumColumn(%s.%s): %w", layer, col, err)
	}
	return sum, nil
}

// MeanColumn returns AVG(col) over every non-null row of layer.
// Empty or all-null layers return NaN (matching Series.Mean's
// convention) rather than an error — cheap to check via
// math.IsNaN. Integer columns promote to Float64.
//
// Same PRAGMA table_info gate as SumColumn: typos surface as
// "column not found" instead of SQLite's silent 0.
func (g *GeoPackage) MeanColumn(layer, col string) (float64, error) {
	return g.scalarAgg("MeanColumn", "AVG", layer, col)
}

// MinColumn returns MIN(col) as Float64. Empty or all-null layers
// return NaN (matching Series.Min). Integer columns promote.
func (g *GeoPackage) MinColumn(layer, col string) (float64, error) {
	return g.scalarAgg("MinColumn", "MIN", layer, col)
}

// MaxColumn returns MAX(col) as Float64. Empty or all-null layers
// return NaN (matching Series.Max). Integer columns promote.
func (g *GeoPackage) MaxColumn(layer, col string) (float64, error) {
	return g.scalarAgg("MaxColumn", "MAX", layer, col)
}

// scalarAgg is the shared implementation for MeanColumn / MinColumn
// / MaxColumn. Runs SELECT <aggFn>(col) FROM layer, treating NULL
// (empty layer / all-null column) as NaN. aggFn is a fixed SQLite
// aggregate keyword ("AVG", "MIN", "MAX") — never caller-supplied,
// so no SQL-injection surface beyond what layer/col already have.
func (g *GeoPackage) scalarAgg(caller, aggFn, layer, col string) (float64, error) {
	if !validSQLIdent(layer) {
		return 0, fmt.Errorf("gpkg: %s: layer %q is not a valid SQLite identifier", caller, layer)
	}
	if !validSQLIdent(col) {
		return 0, fmt.Errorf("gpkg: %s: column %q is not a valid SQLite identifier", caller, col)
	}
	if err := requireColumn(g.db, layer, col); err != nil {
		return 0, err
	}
	query := fmt.Sprintf("SELECT %s(%s) FROM %s",
		aggFn, quoteIdent(col), quoteIdent(layer))
	var v sql.NullFloat64
	if err := g.db.QueryRow(query).Scan(&v); err != nil {
		return 0, fmt.Errorf("gpkg: %s(%s.%s): %w", caller, layer, col, err)
	}
	if !v.Valid {
		return math.NaN(), nil
	}
	return v.Float64, nil
}

// requireColumn returns an error unless col is one of the columns
// declared on layer. Guards against SQLite's double-quoted-identifier
// fallback (unknown quoted identifiers get reinterpreted as string
// literals; a bare SUM(unknown) then returns 0 with no error).
func requireColumn(db *sql.DB, layer, col string) error {
	// PRAGMA table_info doesn't accept bound parameters, so the
	// layer name has to be interpolated. Caller has already run it
	// through validSQLIdent.
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdent(layer)))
	if err != nil {
		return fmt.Errorf("gpkg: table_info(%s): %w", layer, err)
	}
	defer rows.Close()
	found := false
	tableHasAnyRows := false
	for rows.Next() {
		tableHasAnyRows = true
		var cid int
		var name, dtype string
		var notnull, dflt, pk any
		if err := rows.Scan(&cid, &name, &dtype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !tableHasAnyRows {
		return fmt.Errorf("gpkg: layer %q not found", layer)
	}
	if !found {
		return fmt.Errorf("gpkg: column %q not found in layer %q", col, layer)
	}
	return nil
}

// CountRows returns the number of rows in layer via SELECT COUNT(*).
// Constant memory; no per-row work on the Go side. Errors on unknown
// layer or an unsafe identifier.
func (g *GeoPackage) CountRows(layer string) (int64, error) {
	if !validSQLIdent(layer) {
		return 0, fmt.Errorf("gpkg: CountRows: layer %q is not a valid SQLite identifier", layer)
	}
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", quoteIdent(layer))
	var n int64
	if err := g.db.QueryRow(query).Scan(&n); err != nil {
		return 0, fmt.Errorf("gpkg: CountRows(%s): %w", layer, err)
	}
	return n, nil
}

// FeatureTables returns every registered feature table.
func (g *GeoPackage) FeatureTables() ([]FeatureTable, error) {
	rows, err := g.db.Query(`
		SELECT table_name, column_name, srs_id, geometry_type_name
		FROM gpkg_geometry_columns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FeatureTable
	for rows.Next() {
		var ft FeatureTable
		if err := rows.Scan(&ft.Name, &ft.GeomCol, &ft.SRID, &ft.GeomType); err != nil {
			return nil, err
		}
		out = append(out, ft)
	}
	return out, rows.Err()
}

// Feature is one row from a feature table.
type Feature struct {
	Attributes map[string]any
	Geometry   geometry.Geometry
}

// ReadFeatures returns every row of the named feature table with its
// geometry decoded into a geometry.Geometry.
func (g *GeoPackage) ReadFeatures(table string) ([]Feature, error) {
	tables, err := g.FeatureTables()
	if err != nil {
		return nil, err
	}
	var target *FeatureTable
	for i := range tables {
		if tables[i].Name == table {
			target = &tables[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("gpkg: feature table %q not registered", table)
	}
	rows, err := g.db.Query(fmt.Sprintf(`SELECT * FROM %q`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	geomIdx := -1
	for i, c := range cols {
		if c == target.GeomCol {
			geomIdx = i
			break
		}
	}
	if geomIdx == -1 {
		return nil, fmt.Errorf("gpkg: geometry column %q not present in %q", target.GeomCol, table)
	}

	var out []Feature
	for rows.Next() {
		holders := make([]any, len(cols))
		for i := range holders {
			var v any
			holders[i] = &v
		}
		if err := rows.Scan(holders...); err != nil {
			return nil, err
		}
		attrs := make(map[string]any, len(cols)-1)
		var geom geometry.Geometry
		for i, name := range cols {
			val := *(holders[i].(*any))
			if i == geomIdx {
				if val == nil {
					continue
				}
				b, ok := val.([]byte)
				if !ok {
					return nil, fmt.Errorf("gpkg: geometry column not []byte, got %T", val)
				}
				g, err := DecodeGeometry(b)
				if err != nil {
					return nil, err
				}
				geom = g
				continue
			}
			attrs[name] = val
		}
		out = append(out, Feature{Attributes: attrs, Geometry: geom})
	}
	return out, rows.Err()
}

// DecodeGeometry decodes a GeoPackage geometry blob (header + WKB) into a
// geometry.Geometry, attaching the header's SRS as a CRS if it maps to a
// known one.
func DecodeGeometry(b []byte) (geometry.Geometry, error) {
	if len(b) < 8 || b[0] != 'G' || b[1] != 'P' {
		return nil, ErrInvalidHeader
	}
	// b[2] version, b[3] flags, b[4:8] SRS ID.
	flags := b[3]
	srsID := int32(binary.LittleEndian.Uint32(b[4:8]))

	envelopeSize, err := envelopeBytes(flags)
	if err != nil {
		return nil, err
	}
	off := 8 + envelopeSize
	if len(b) < off {
		return nil, io.ErrUnexpectedEOF
	}
	g, err := geometry.ParseWKB(b[off:])
	if err != nil {
		return nil, err
	}
	// Attach CRS if we know it.
	if crs, err := geometry.LookupCRS(srsID); err == nil {
		g = withCRS(g, crs)
	}
	return g, nil
}

// envelopeBytes returns the number of envelope bytes indicated by the flag
// byte. See OGC GeoPackage §2.1.3.1.1 — envelope contents indicator is bits
// 3..1 of flag byte.
func envelopeBytes(flags byte) (int, error) {
	switch (flags >> 1) & 0x07 {
	case 0:
		return 0, nil
	case 1:
		return 32, nil // XY
	case 2, 3:
		return 48, nil // XYZ or XYM
	case 4:
		return 64, nil // XYZM
	default:
		return 0, fmt.Errorf("%w: reserved envelope code", ErrInvalidHeader)
	}
}

func withCRS(g geometry.Geometry, c geometry.CRS) geometry.Geometry {
	switch t := g.(type) {
	case geometry.Point:
		t.CRSValue = c
		return t
	case geometry.LineString:
		t.CRSValue = c
		return t
	case geometry.Polygon:
		t.CRSValue = c
		return t
	case geometry.MultiPoint:
		t.CRSValue = c
		return t
	default:
		return g
	}
}

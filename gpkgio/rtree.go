package gpkgio

import (
	"database/sql"
	"fmt"
)

// rtreeTableName returns the standard GeoPackage RTree shadow-table
// name for a (layer, geomCol) pair.
func rtreeTableName(table, geomCol string) string {
	return fmt.Sprintf("rtree_%s_%s", table, geomCol)
}

// createRTree creates the standard GeoPackage RTree virtual table
// for the (layer, geomCol) pair and registers it in gpkg_extensions.
//
// Note: gobi populates the RTree directly from Go during the INSERT
// loop rather than via SQLite triggers. The GeoPackage spec's example
// triggers rely on SpatiaLite geometry functions (ST_MinX, ST_MaxX,
// ST_IsEmpty) which modernc.org/sqlite doesn't ship — we're stock
// SQLite with no cgo. Direct maintenance is spec-conformant (§2.1.6
// only requires the RTree contents match the feature table's
// geometry envelopes; how you maintain it is unspecified).
//
// Consequence: rows added to the feature table by OTHER tools that
// don't understand this convention (e.g. hand-crafted INSERTs)
// won't appear in the RTree. GDAL / QGIS create their own triggers
// on the file when they open it in write mode and update the RTree
// themselves; that's fine — our RTree stays consistent for the rows
// we wrote and their maintenance handles any rows they add.
func createRTree(db *sql.DB, table, geomCol string) error {
	rtreeName := rtreeTableName(table, geomCol)
	if _, err := db.Exec(fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING rtree (id, minx, maxx, miny, maxy)`,
		quoteIdent(rtreeName))); err != nil {
		return fmt.Errorf("create rtree table: %w", err)
	}
	// Register the RTree extension in gpkg_extensions so strict
	// validators find it. GDAL and QGIS accept the file with or
	// without this row, but ogr_verify wants it.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS gpkg_extensions (
			table_name TEXT,
			column_name TEXT,
			extension_name TEXT NOT NULL,
			definition TEXT NOT NULL,
			scope TEXT NOT NULL,
			CONSTRAINT ge_tce UNIQUE (table_name, column_name, extension_name)
		)`); err != nil {
		return err
	}
	if _, err := db.Exec(`
		INSERT OR REPLACE INTO gpkg_extensions
		  (table_name, column_name, extension_name, definition, scope)
		VALUES (?, ?, 'gpkg_rtree_index', 'http://www.geopackage.org/spec/#extension_rtree', 'write-only')`,
		table, geomCol); err != nil {
		return err
	}
	return nil
}

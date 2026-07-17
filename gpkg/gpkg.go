// Package gpkg reads geometry features from an OGC GeoPackage (SQLite).
//
// GeoPackage stores each geometry as a small header followed by a standard
// WKB payload. This package focuses on that geometry decoding plus a light
// discovery API — feature tables are exposed as an iterator that yields
// (attributes, geometry) tuples.
package gpkg

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

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
	SRSID    int32
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
		if err := rows.Scan(&ft.Name, &ft.GeomCol, &ft.SRSID, &ft.GeomType); err != nil {
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

package readers

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/zoobst/gobi/geometry"

	_ "modernc.org/sqlite"
)

type FeatureTable struct {
	Name    string
	GeomCol string
	SRSID   int
}

type GeoPackage struct {
	db *sql.DB
}

func OpenGeoPackage(path string) (gpkg GeoPackage, err error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return gpkg, fmt.Errorf("failed to open GeoPackage: %w", err)
	}
	return GeoPackage{db: db}, nil
}

func (g *GeoPackage) FeatureTables() ([]FeatureTable, error) {
	rows, err := g.db.Query(`
		SELECT table_name, column_name, srs_id
		FROM gpkg_geometry_columns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []FeatureTable
	for rows.Next() {
		var ft FeatureTable
		if err := rows.Scan(&ft.Name, &ft.GeomCol, &ft.SRSID); err != nil {
			return nil, err
		}
		tables = append(tables, ft)
	}
	return tables, nil
}

func parseGeoPackageWKB(b []byte) (outGeo geometry.Geometry, err error) {
	if len(b) < 8 || b[0] != 'G' || b[1] != 'P' {
		return nil, errors.New("invalid GeoPackage geometry header")
	}

	srsID := int32(binary.LittleEndian.Uint32(b[4:8]))
	wkb := b[8:]

	geom, err := geometry.ParseWKB(wkb)
	if err != nil {
		return nil, err
	}

	err = geometry.ToCRS(&geom, srsID)
	if err != nil {
		return nil, err
	}

	return geom, nil
}

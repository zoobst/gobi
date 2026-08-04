package geojsonio

import (
	"github.com/zoobst/gobi"
)

// ReadStructs reads a GeoJSON file and decodes each feature into a T.
// Property names are matched against T's fields via gobi's tag
// resolution under the "geojson" namespace: `geojson:"prop"` tags
// take priority, with fallback to `gobi:"col"` → field name.
// `geojson:"-"` skips a field. Fields tagged `geom:"true"` map to
// the feature's geometry (WKB / WKT).
//
//	type Row struct {
//	    ID       int64  `geojson:"id"`
//	    Name     string `geojson:"name" gobi:"name"`
//	    Geometry []byte `geom:"true"`
//	}
//	rows, err := geojsonio.ReadStructs[Row]("data.geojson", nil)
//
// Wraps ReadFile + gobi.ToStructs.
func ReadStructs[T any](path string, opts *ReadOptions) ([]T, error) {
	f, err := ReadFile(path, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("geojson"))
}

// WriteStructs encodes rows as GeoJSON. Property names come from the
// "geojson" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, path string, opts *WriteOptions) error {
	f, err := gobi.FromStructs(rows, gobi.StructTagFormat("geojson"))
	if err != nil {
		return err
	}
	return WriteFile(f, path, opts)
}

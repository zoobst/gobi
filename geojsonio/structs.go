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
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. Pass gobi.StructInterner in structOpts
// to share one copy of each value of `intern`-tagged fields across
// calls.
func ReadStructs[T any](path string, opts *ReadOptions, structOpts ...gobi.StructOption) ([]T, error) {
	f, err := ReadFile(path, opts)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("geojson"), gobi.StructCopyValues()}, structOpts...)...)
}

// WriteStructs encodes rows as GeoJSON. Property names come from the
// "geojson" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, path string, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("geojson")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteFile(f, path, opts)
}

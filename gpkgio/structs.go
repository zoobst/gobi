package gpkgio

import (
	"github.com/zoobst/gobi"
)

// ReadStructs reads a GeoPackage file (first feature table, or the
// one selected via opts.Table) and decodes each row into a T. Column
// names are matched against T's fields via gobi's tag resolution
// under the "gpkg" namespace: `gpkg:"col"` tags take priority, with
// fallback to `gobi:"col"` → field name. `gpkg:"-"` skips a field.
// Fields tagged `geom:"true"` map to the geometry column.
//
//	type Row struct {
//	    ID       int64  `gpkg:"fid"`
//	    Name     string `gpkg:"name"`
//	    Geometry []byte `geom:"true"`
//	}
//	rows, err := gpkgio.ReadStructs[Row]("data.gpkg", nil)
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
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("gpkg"), gobi.StructCopyValues()}, structOpts...)...)
}

// WriteStructs encodes rows into a GeoPackage file. Column names come
// from the "gpkg" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, path string, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("gpkg")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteFile(f, path, opts)
}

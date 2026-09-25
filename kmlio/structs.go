package kmlio

import (
	"github.com/zoobst/gobi"
)

// ReadStructs reads a KML or KMZ file and decodes each Placemark
// into a T. Property names are matched against T's fields via
// gobi's tag resolution under the "kml" namespace: `kml:"prop"`
// tags take priority, with fallback to `gobi:"col"` → field name.
// `kml:"-"` skips a field. Fields tagged `geom:"true"` map to the
// placemark's geometry.
//
//	type Row struct {
//	    Name        string `kml:"name"`
//	    Description string `kml:"description"`
//	    Geometry    []byte `geom:"true"`
//	}
//	rows, err := kmlio.ReadStructs[Row]("data.kml", nil)
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
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("kml"), gobi.StructCopyValues()}, structOpts...)...)
}

// WriteStructs encodes rows into a KML/KMZ file. Property names come
// from the "kml" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, path string, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("kml")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteFile(f, path, opts)
}

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
func ReadStructs[T any](path string, opts *ReadOptions) ([]T, error) {
	f, err := ReadFile(path, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("kml"))
}

// WriteStructs encodes rows into a KML/KMZ file. Property names come
// from the "kml" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, path string, opts *WriteOptions) error {
	f, err := gobi.FromStructs(rows, gobi.StructTagFormat("kml"))
	if err != nil {
		return err
	}
	return WriteFile(f, path, opts)
}

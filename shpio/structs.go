package shpio

import (
	"github.com/zoobst/gobi"
)

// ReadStructs reads a Shapefile (`base.shp` + sibling `.dbf` etc.)
// and decodes each row into a T. Column names are matched against
// T's fields via gobi's tag resolution under the "shp" namespace:
// `shp:"NAME10"` tags take priority. Because Shapefile / DBF field
// names are limited to 10 ASCII characters, the "shp" tag is
// particularly useful for aliasing Go's longer field names down to
// their storage names. Resolution fallback: `shp:` → `gobi:` → csv →
// field name. `shp:"-"` skips a field. Fields tagged `geom:"true"`
// map to the shapefile's geometry column.
//
//	type Row struct {
//	    ID           int64  `shp:"OBJECTID"`
//	    Name         string `shp:"NAME"       gobi:"name"`
//	    Population   int64  `shp:"POP10"`             // 10-char DBF-friendly alias
//	    Geometry     []byte `geom:"true"`
//	    Notes        string `shp:"-"`                 // omit from output
//	}
//	rows, err := shpio.ReadStructs[Row]("counties", nil)
//
// Wraps ReadFile + gobi.ToStructs.
func ReadStructs[T any](base string, opts *ReadOptions) ([]T, error) {
	f, err := ReadFile(base, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("shp"))
}

// WriteStructs encodes rows into a Shapefile. Column names come from
// the "shp" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, base string, opts *WriteOptions) error {
	f, err := gobi.FromStructs(rows, gobi.StructTagFormat("shp"))
	if err != nil {
		return err
	}
	return WriteFile(f, base, opts)
}

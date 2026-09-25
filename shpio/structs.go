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
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. Pass gobi.StructInterner in structOpts
// to share one copy of each value of `intern`-tagged fields across
// calls.
func ReadStructs[T any](base string, opts *ReadOptions, structOpts ...gobi.StructOption) ([]T, error) {
	f, err := ReadFile(base, opts)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("shp"), gobi.StructCopyValues()}, structOpts...)...)
}

// WriteStructs encodes rows into a Shapefile. Column names come from
// the "shp" tag namespace (see ReadStructs).
func WriteStructs[T any](rows []T, base string, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("shp")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteFile(f, base, opts)
}

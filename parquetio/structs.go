package parquetio

import (
	"github.com/zoobst/gobi"
)

// ReadStructs reads a parquet file and decodes each row into a T.
// Column names are matched against T's fields using gobi's tag
// resolution with the "parquet" format namespace, e.g.:
//
//	type Row struct {
//	    ID   int64  `parquet:"id"`
//	    Name string `parquet:"name" gobi:"name"`  // gobi: as universal fallback
//	    Skip string `parquet:"-"`                  // omitted entirely
//	}
//	rows, err := parquetio.ReadStructs[Row]("data.parquet", nil)
//
// Resolution fallback: parquet tag → gobi tag → csv tag (legacy) →
// field name. See gobi.ResolveFieldName for details.
//
// Wraps ReadFile + gobi.ToStructs; there's no performance penalty
// compared to doing those two calls yourself.
func ReadStructs[T any](path string, opts *ReadOptions) ([]T, error) {
	f, err := ReadFile(path, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("parquet"))
}

// WriteStructs encodes rows into a parquet file. Column names come
// from struct-tag resolution under the "parquet" format namespace
// (see ReadStructs for the tag conventions).
//
// Wraps gobi.FromStructs + WriteFile.
func WriteStructs[T any](rows []T, path string, opts *WriteOptions) error {
	f, err := gobi.FromStructs(rows, gobi.StructTagFormat("parquet"))
	if err != nil {
		return err
	}
	return WriteFile(f, path, opts)
}

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
// Wraps ReadFile + gobi.ToStructs.
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. Pass gobi.StructInterner in structOpts
// to share one copy of each value of `intern`-tagged fields across
// calls. The copy costs one allocation per string / []byte cell; for
// zero-copy decoding of short-lived rows, call ReadFile +
// gobi.ToStructs yourself and keep the Frame alive while the rows are
// in use.
func ReadStructs[T any](path string, opts *ReadOptions, structOpts ...gobi.StructOption) ([]T, error) {
	f, err := ReadFile(path, opts)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("parquet"), gobi.StructCopyValues()}, structOpts...)...)
}

// WriteStructs encodes rows into a parquet file. Column names come
// from struct-tag resolution under the "parquet" format namespace
// (see ReadStructs for the tag conventions). structOpts pass through
// to gobi.FromStructs — e.g. gobi.StructRequiredFields() and
// gobi.StructZeroTimeAsValue() for parquet-go's REQUIRED columns and
// zero-time handling.
//
// Wraps gobi.FromStructs + WriteFile.
func WriteStructs[T any](rows []T, path string, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("parquet")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteFile(f, path, opts)
}

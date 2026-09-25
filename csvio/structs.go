package csvio

import (
	"io"

	"github.com/zoobst/gobi"
)

// ReadStructs reads a CSV file and decodes each row into a T. Column
// names are matched against T's fields via gobi's tag resolution
// under the "csv" namespace: `csv:"col"` tags take priority, with
// fallback to `gobi:"col"` → field name. `csv:"-"` skips a field.
//
//	type Row struct {
//	    ID   int64  `csv:"id"`
//	    Name string `csv:"name" gobi:"name"`
//	}
//	rows, err := csvio.ReadStructs[Row]("data.csv", nil)
//
// Wraps ReadFile[T] + gobi.ToStructs. No writing counterpart because
// csvio itself is read-only; write via parquetio or another sink.
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. Pass gobi.StructInterner in structOpts
// to share one copy of each value of `intern`-tagged fields across
// calls.
func ReadStructs[T any](path string, opts *ReadOptions, structOpts ...gobi.StructOption) ([]T, error) {
	f, err := ReadFile[T](path, opts)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("csv"), gobi.StructCopyValues()}, structOpts...)...)
}

// ReadStructsReader is the io.Reader-backed variant of ReadStructs.
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. Pass gobi.StructInterner in structOpts
// to share one copy of each value of `intern`-tagged fields across
// calls.
func ReadStructsReader[T any](r io.Reader, opts *ReadOptions, structOpts ...gobi.StructOption) ([]T, error) {
	f, err := Read[T](r, opts)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("csv"), gobi.StructCopyValues()}, structOpts...)...)
}

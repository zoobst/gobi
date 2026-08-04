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
func ReadStructs[T any](path string, opts *ReadOptions) ([]T, error) {
	f, err := ReadFile[T](path, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("csv"))
}

// ReadStructsReader is the io.Reader-backed variant of ReadStructs.
func ReadStructsReader[T any](r io.Reader, opts *ReadOptions) ([]T, error) {
	f, err := Read[T](r, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("csv"))
}

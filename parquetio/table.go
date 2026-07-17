package parquetio

import (
	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
)

// newTableFromColumns is a small shim around array.NewTable that takes the
// signature the parquetio package needs (columns by value, explicit row count).
func newTableFromColumns(schema *arrow.Schema, cols []arrow.Column, rows int64) arrow.Table {
	return array.NewTable(schema, cols, rows)
}

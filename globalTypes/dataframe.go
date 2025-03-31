package globalTypes

import (
	"github.com/apache/arrow/go/arrow"
	"github.com/apache/arrow/go/arrow/array"
)

// NewDataFrame creates a new DataFrame from Arrow Table
func NewDataFrame() *DataFrame {
	return &DataFrame{
		Table:   []array.Column{},
		Schema:  arrow.NewSchema([]arrow.Field{}, &arrow.Metadata{}),
		Columns: make(map[string]Series), // TODO: Fix this
	}
}

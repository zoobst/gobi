package globalTypes

import "github.com/apache/arrow/go/arrow/array"

func NewSeries() *Series {
	return &Series{
		Col: array.Column{},
	}
}

package globalTypes

import (
	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// NewSeries creates a new Series with a memory allocator
func NewSeries(name string, values *arrow.Column) Series {
	allocator := memory.NewGoAllocator()
	return Series{
		Name:      name,
		Values:    values,
		Allocator: allocator,
	}
}

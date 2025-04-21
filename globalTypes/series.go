package globalTypes

import (
	"fmt"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
	berrors "github.com/zoobst/gobi/bErrors"
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

func NewSeriesFromColumns(cols []arrow.Column, schema *arrow.Schema) []Series {
	sers := []Series{}
	for _, col := range cols {
		ser := Series{
			Name:      col.Name(),
			Values:    &col,
			Allocator: memory.DefaultAllocator,
		}
		sers = append(sers, ser)
	}
	return sers
}

func (s Series) String() string {
	o := new(strings.Builder)
	o.WriteString("\n")
	o.WriteString(s.Name + ": [")
	for j, chunk := range s.Values.Data().Chunks() {
		if j != 0 {
			o.WriteString(", ")
		}
		o.WriteString(chunk.String())
	}
	return o.String()
}

func (s *Series) Iloc(i int) (Series, error) {
	if s.Values.Len() < i {
		return Series{}, fmt.Errorf(berrors.ErrIndexOutOfRange.Error(), i, s.Values.Len())
	}

	ser := Series{
		Allocator: memory.DefaultAllocator,
	}

	t := array.NewColumnSlice(s.Values, int64(i), int64(i+1))

	ser.Values = t
	ser.Name = s.Name

	return ser, nil
}

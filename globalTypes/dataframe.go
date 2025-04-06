package globalTypes

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
	berrors "github.com/zoobst/gobi/bErrors"
)

// NewDataFrame creates a new DataFrame from Arrow Table
func NewDataFrame(s *arrow.Schema) arrow.Table {
	return DataFrame{
		schema: s,
	}
}

func NewDataFrameFromTable(t arrow.Table) *DataFrame {
	df := DataFrame{
		schema: t.Schema(),
	}
	for i := range t.NumCols() {
		df.Series = append(df.Series, NewSeries(t.Column(int(i)).Name(), t.Column(int(i))))
	}
	return &df
}

func (df DataFrame) Shape() (int, int) {
	return int(df.NumCols()), int(df.NumRows())
}

func (df DataFrame) Schema() *arrow.Schema { return df.schema }

func (df DataFrame) NumRows() int64 { return int64(df.Series[0].Values.Len()) }

func (df DataFrame) NumCols() int64 { return int64(len(df.Series)) }

func (df DataFrame) Column(i int) *arrow.Column { return df.Series[i].Values }

func (df DataFrame) AddColumn(pos int, f arrow.Field, c arrow.Column) (arrow.Table, error) {
	if int64(c.Len()) != df.NumRows() {
		return nil, fmt.Errorf(berrors.ErrColLengthMismatch.Error(), c.Len(), df.NumRows())
	}
	if f.Type != c.DataType() {
		return nil, fmt.Errorf(berrors.ErrColTypeMismatch.Error(), f.Type, c.DataType())
	}
	newSchema, err := df.schema.AddField(pos, f)
	if err != nil {
		return nil, err
	}
	cols := make([]Series, df.NumCols()+1)
	copy(cols[:pos], df.Series[:pos])
	cols[pos] = NewSeries(c.Name(), &c)
	copy(cols[pos+1:], df.Series[pos:])
	newTable := DataFrame{
		schema: newSchema,
		Series: cols,
	}
	return newTable, nil
}

func (df DataFrame) String() string {
	o := new(strings.Builder)
	o.WriteString("\n")

	for i := range int(df.NumCols()) {
		col := df.Column(i)
		o.WriteString(col.Field().Name + ": [")
		for j, chunk := range col.Data().Chunks() {
			if j != 0 {
				o.WriteString(", ")
			}
			o.WriteString(chunk.String())
		}
		o.WriteString("]\n")
	}
	return o.String()
}

func (df DataFrame) Retain() {
	atomic.AddInt64(&df.refCount, 1)
}

func (df DataFrame) Release() {
	if atomic.LoadInt64(&df.refCount) > 0 {
		log.Fatal("too many releases")
	}

	if atomic.AddInt64(&df.refCount, -1) == 0 {
		for i := range df.Series {
			df.Series[i].Values.Release()
		}
		df.Series = nil
	}
}

func (df *DataFrame) Head(nRows int) (DataFrame, error) {
	var n int64
	if nRows == 0 {
		n = 5
	} else {
		n = int64(nRows)
	}
	if n < 0 {
		return DataFrame{}, fmt.Errorf(berrors.ErrInvalidNumRows.Error(), nRows)
	}
	var serList []Series
	for _, ser := range df.Series {
		if ser.Values.Len() < int(n) {
			n = int64(ser.Values.Len())
		}
		nSer := Series{
			Name:      ser.Name,
			Values:    array.NewColumnSlice(ser.Values, 0, n),
			Allocator: memory.DefaultAllocator,
		}
		serList = append(serList, nSer)

	}
	newDf := DataFrame{
		schema: df.Schema(),
		Series: serList,
	}
	return newDf, nil
}

func (df *DataFrame) Tail(nRows int) (DataFrame, error) {
	var n int64
	if nRows == 0 {
		n = 6
	} else {
		n = int64(nRows)
	}
	if n <= 0 {
		return DataFrame{}, fmt.Errorf(berrors.ErrInvalidNumRows.Error(), nRows)
	}
	var serList []Series
	for _, ser := range df.Series {
		if ser.Values.Len() < int(n) {
			n = int64(ser.Values.Len())
		}
		nSer := Series{
			Name:      ser.Name,
			Values:    array.NewColumnSlice(ser.Values, int64(ser.Values.Len())-n, int64(ser.Values.Len()-1)),
			Allocator: memory.DefaultAllocator,
		}
		serList = append(serList, nSer)

	}
	newDf := DataFrame{
		schema: df.Schema(),
		Series: serList,
	}
	return newDf, nil
}

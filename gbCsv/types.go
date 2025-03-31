package gbcsv

import (
	gTypes "github.com/zoobst/gobi/globalTypes"

	"github.com/zoobst/gobi/cmprssn"

	"github.com/apache/arrow/go/arrow"
)

type CsvReadOptions struct {
	HasHeader       bool
	Columns         map[string]gTypes.GBType
	Separator       rune
	CommentPrefix   rune
	QuoteChar       rune
	SkipRows        int
	SkipSlice       [2]int
	InferSchema     bool
	Schema          *arrow.Schema
	SchemaOverrides map[string]any
	NullValues      []any
	IgnoreErrors    bool
	TryToParseDates bool
	MaxWorkers      int
	BatchSize       int
	NumRows         int
	Encoding        *arrow.DataType
	SampleSize      int
	Compression     cmprssn.CompressionType
}

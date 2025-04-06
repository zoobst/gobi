package gbcsv

import (
	"github.com/zoobst/gobi/cmprssn"

	"github.com/apache/arrow/go/arrow"
)

const (
	defaultHasHeader       bool = true
	defaultSeparator       rune = ','
	defaultCommentPrefix   rune = '#'
	defaultQuoteChar       rune = '"'
	defaultInferSchema     bool = true
	defaultIgnoreErrors    bool = false
	defaultTryToParseDates bool = false
	defaultBatchSize       int  = 8192
	defaultNumRows         int  = -1
	defaultSampleSize      int  = 1024
)

type CsvReadOptions struct {
	HasHeader       *bool
	Columns         *[]string
	Separator       *rune
	CommentPrefix   *rune
	QuoteChar       *rune
	SkipRows        *int
	SkipSlice       *[2]int
	InferSchema     *bool
	Schema          *arrow.Schema
	SchemaOverrides *map[string]any
	NullValues      *[]any
	IgnoreErrors    *bool
	TryToParseDates *bool
	MaxWorkers      *int
	BatchSize       *int
	NumRows         *int
	Encoding        *arrow.DataType
	SampleSize      *int
	Compression     *cmprssn.CompressionType
}

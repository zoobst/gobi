package gbcsv

import (
	"github.com/zoobst/gobi/cmprssn"

	"github.com/apache/arrow/go/v18/arrow"
)

var (
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

func (cro *CsvReadOptions) setDefaults() {
	if cro.HasHeader == nil {
		cro.HasHeader = &defaultHasHeader
	}
	if cro.Separator == nil {
		cro.Separator = &defaultSeparator
	}
	if cro.CommentPrefix == nil {
		cro.CommentPrefix = &defaultCommentPrefix
	}
	if cro.QuoteChar == nil {
		cro.QuoteChar = &defaultQuoteChar
	}
	if cro.InferSchema == nil {
		cro.InferSchema = &defaultInferSchema
	}
	if cro.IgnoreErrors == nil {
		cro.IgnoreErrors = &defaultIgnoreErrors
	}
	if cro.TryToParseDates == nil {
		cro.TryToParseDates = &defaultTryToParseDates
	}
	if cro.BatchSize == nil {
		cro.BatchSize = &defaultBatchSize
	}
	if cro.NumRows == nil {
		cro.NumRows = &defaultNumRows
	}
	if cro.SampleSize == nil {
		cro.SampleSize = &defaultSampleSize
	}
}

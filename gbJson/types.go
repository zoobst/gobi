package gbJson

import (
	"github.com/zoobst/gobi/cmprssn"
)

type GeoJSONReadOptions struct {
	SkipRows        *int
	SkipSlice       [2]int
	IgnoreErrors    *bool
	TryToParseDates *bool
	MaxWorkers      *int
	BatchSize       *int
	NumRows         *int
	SampleSize      *int
	Compression     *cmprssn.CompressionType
}

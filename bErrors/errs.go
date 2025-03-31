package berrors

import (
	"errors"
)

var ErrEmptyArray error = errors.New("array is empty")

var ErrEmptyDataFrame error = errors.New("dataframe is empty")

var ErrSchemaMismatch error = errors.New("schema provided does not match data")

var ErrUnsupportedCompressionType error = errors.New("unsupported compression type")

var WarningUnknownPropertyType string = "Warning: Unknown type for property '%s', defaulting to string.\n"

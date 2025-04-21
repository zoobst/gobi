package berrors

import (
	"errors"
)

var ErrEmptyArray error = errors.New("array is empty")

var ErrEmptyDataFrame error = errors.New("dataframe is empty")

var ErrSchemaMismatch error = errors.New("schema provided does not match data")

var ErrUnsupportedCompressionType error = errors.New("unsupported compression type")

var WarningUnknownPropertyType string = "Warning: Unknown type for property '%s', defaulting to string.\n"

var ErrNumCoords error = errors.New("serialized coords are incorrect length")

var ErrColLengthMismatch error = errors.New("arrow/array: column length mismatch: %d != %d")

var ErrColTypeMismatch error = errors.New("arrow/array: column type mismatch: %v != %v")

var ErrInvalidGeometryType error = errors.New("invalid/unknown geometry type")

var ErrInvalidNumRows error = errors.New("invalid number of rows: %d")

var ErrUnsupportedType error = errors.New("unsupported type: %s")

var ErrInvalidType error = errors.New("Invalid Type for type: %v")

var ErrIndexOutOfRange error = errors.New("index: %d is out of range of dataframe with len %d")

var ErrUnknownColumn error = errors.New("unrecognized column name: %s")

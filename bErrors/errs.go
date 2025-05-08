package berrors

import (
	"errors"
)

var (
	ErrEmptyArray error = errors.New("array is empty")

	ErrEmptyDataFrame error = errors.New("dataframe is empty")

	ErrSchemaMismatch error = errors.New("schema provided does not match data")

	ErrUnsupportedCompressionType error = errors.New("unsupported compression type")

	ErrNumCoords error = errors.New("serialized coords are incorrect length")

	ErrColLengthMismatch error = errors.New("arrow/array: column length mismatch: %d != %d")

	ErrColTypeMismatch error = errors.New("arrow/array: column type mismatch: %v != %v")

	ErrInvalidGeometryType error = errors.New("invalid/unknown geometry type")

	ErrInvalidNumRows error = errors.New("invalid number of rows: %d")

	ErrUnsupportedType error = errors.New("unsupported type: %s")

	ErrInvalidType error = errors.New("invalid Type for type: %v")

	ErrIndexOutOfRange error = errors.New("index: %d is out of range of dataframe with len %d")

	ErrUnknownColumn error = errors.New("unrecognized column name: %s")

	ErrIncompatibleCoordinates error = errors.New("coordinate type %f, %f is incompatible with WGS84 degree coordinate system. Please specify a projected coordinate system to continue")

	ErrInvalidUnit error = errors.New("invalid unit: %s provided")
)

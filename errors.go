package gobi

import "errors"

var (
	ErrColumnNotFound     = errors.New("gobi: column not found")
	ErrColumnLenMismatch  = errors.New("gobi: column length mismatch")
	ErrColumnTypeMismatch = errors.New("gobi: column type mismatch")
	ErrDuplicateColumn    = errors.New("gobi: duplicate column name")
	ErrRowOutOfRange      = errors.New("gobi: row index out of range")
	ErrEmptyFrame         = errors.New("gobi: empty dataframe")
	ErrNotGeometry        = errors.New("gobi: column is not a geometry column")
	// ErrInvalidGeometryCRS: a geometry field's gobi:crs_epsg tag is
	// present but isn't a decimal EPSG code. Refused rather than read as
	// "no CRS", which would mislabel or misread the coordinates.
	ErrInvalidGeometryCRS = errors.New("gobi: unreadable geometry CRS tag")
	ErrExprTypeMismatch   = errors.New("gobi: expression type mismatch")
	ErrUnsupportedLiteral = errors.New("gobi: unsupported literal type")

	// ErrTimeParse is returned by StrToTime / StrToDate when a non-null
	// value doesn't match the layout. The error names the row and wraps
	// the *time.ParseError.
	ErrTimeParse = errors.New("gobi: value does not match time layout")

	// ErrTimeOutOfRange is returned when a time doesn't fit the column
	// it would go into, such as UnixNano on a year past 2262, or
	// AddDuration taking a value past the end of its unit's range.
	ErrTimeOutOfRange = errors.New("gobi: time out of range")
)

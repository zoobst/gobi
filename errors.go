package gobi

import "errors"

var (
	ErrColumnNotFound  = errors.New("gobi: column not found")
	ErrColumnLenMismatch = errors.New("gobi: column length mismatch")
	ErrColumnTypeMismatch = errors.New("gobi: column type mismatch")
	ErrRowOutOfRange   = errors.New("gobi: row index out of range")
	ErrEmptyFrame      = errors.New("gobi: empty dataframe")
	ErrNotGeometry     = errors.New("gobi: column is not a geometry column")
)

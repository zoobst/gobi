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
	ErrExprTypeMismatch   = errors.New("gobi: expression type mismatch")
	ErrUnsupportedLiteral = errors.New("gobi: unsupported literal type")
)

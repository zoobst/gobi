package geometry

import "errors"

var (
	ErrShortWKB          = errors.New("geometry: WKB too short")
	ErrInvalidByteOrder  = errors.New("geometry: invalid WKB byte order")
	ErrUnsupportedWKB    = errors.New("geometry: unsupported WKB geometry type")
	ErrTypeMismatch      = errors.New("geometry: WKB type does not match target")
	ErrEmptyGeometry     = errors.New("geometry: geometry has no points")
	ErrInvalidWKT        = errors.New("geometry: invalid WKT")
	ErrUnknownCRS        = errors.New("geometry: unknown CRS")
	ErrInvalidUnit       = errors.New("geometry: invalid distance unit")
	ErrCRSMismatch       = errors.New("geometry: operation requires geometries in the same CRS")
	ErrProjectionMissing = errors.New("geometry: reprojection between these CRSes is not implemented")
)

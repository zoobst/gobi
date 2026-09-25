package gobi

import (
	"errors"
	"fmt"
	"math"
	"reflect"
)

// ErrStructFieldInexact is returned by ToStructs under
// StructCoerceNumbers when a value can't be represented exactly in the
// field's type: a non-integral float into an integer field, or an
// integer too large for a float field's mantissa.
var ErrStructFieldInexact = errors.New("gobi: value not exactly representable in struct field")

// StructCoerceNumbers lets ToStructs convert between numeric kinds —
// signed, unsigned and floating point — as long as each value
// survives exactly. It's for sources whose column types drift between
// files (the same logical field written as INT32 by one producer and
// DOUBLE by another).
//
//   - float → int / uint: only integral values in the field's range
//     (3.0 → 3; 3.5 is ErrStructFieldInexact).
//   - int ↔ uint: only values in the target's range (-1 → uint is
//     ErrStructFieldOverflow).
//   - int / uint → float: only values the float represents exactly
//     (up to 2^53 for float64, 2^24 for float32).
//
// Same-kind conversions (int64 column → int32 field) work without this
// option and are range-checked either way. Bools and strings are
// never coerced.
func StructCoerceNumbers() StructOption {
	return func(o *structOpts) { o.coerceNumbers = true }
}

// StructRequireColumns makes ToStructs fail with ErrColumnNotFound when
// a struct field has no matching column, instead of leaving the field
// at its zero value. Use it when a missing column means a schema
// mismatch rather than an optional field.
func StructRequireColumns() StructOption {
	return func(o *structOpts) { o.requireColumns = true }
}

// coerceNumber converts a numeric v across kinds into fv. handled is
// false when v or fv isn't numeric, or when the kinds already match
// (assignScalar covers those, with its own range check).
func coerceNumber(fv reflect.Value, v any) (handled bool, err error) {
	src, ok := numericKindOf(v)
	if !ok {
		return false, nil
	}
	dst := fv.Kind()
	switch {
	case isIntKind(dst):
		if src == reflect.Int64 {
			return false, nil
		}
		x, err := toInt64Exact(v)
		if err != nil {
			return true, fmt.Errorf("%w: %v into %s field", err, v, fv.Type())
		}
		if fv.OverflowInt(x) {
			return true, fmt.Errorf("%w: value %d overflows %s field", ErrStructFieldOverflow, x, fv.Type())
		}
		fv.SetInt(x)
		return true, nil
	case isUintKind(dst):
		if src == reflect.Uint64 {
			return false, nil
		}
		x, err := toUint64Exact(v)
		if err != nil {
			return true, fmt.Errorf("%w: %v into %s field", err, v, fv.Type())
		}
		if fv.OverflowUint(x) {
			return true, fmt.Errorf("%w: value %d overflows %s field", ErrStructFieldOverflow, x, fv.Type())
		}
		fv.SetUint(x)
		return true, nil
	case dst == reflect.Float32 || dst == reflect.Float64:
		if src == reflect.Float64 {
			return false, nil
		}
		f, err := toFloatExact(v, dst == reflect.Float32)
		if err != nil {
			return true, fmt.Errorf("%w: %v into %s field", err, v, fv.Type())
		}
		fv.SetFloat(f)
		return true, nil
	}
	return false, nil
}

// numericKindOf groups v's Go type: Int64 for signed ints, Uint64 for
// unsigned, Float64 for floats.
func numericKindOf(v any) (reflect.Kind, bool) {
	switch v.(type) {
	case int64, int32, int16, int8:
		return reflect.Int64, true
	case uint64, uint32, uint16, uint8:
		return reflect.Uint64, true
	case float64, float32:
		return reflect.Float64, true
	}
	return reflect.Invalid, false
}

func isIntKind(k reflect.Kind) bool {
	return k == reflect.Int || k == reflect.Int8 || k == reflect.Int16 || k == reflect.Int32 || k == reflect.Int64
}

func isUintKind(k reflect.Kind) bool {
	return k == reflect.Uint || k == reflect.Uint8 || k == reflect.Uint16 || k == reflect.Uint32 || k == reflect.Uint64
}

// two63 is 2^63 as a float64 (exactly representable).
const two63 = float64(1 << 63)

func toInt64Exact(v any) (int64, error) {
	switch x := v.(type) {
	case uint64, uint32, uint16, uint8:
		u := toU64(x)
		if u > math.MaxInt64 {
			return 0, ErrStructFieldOverflow
		}
		return int64(u), nil
	case float64, float32:
		f := toF64(x)
		if f != math.Trunc(f) || math.IsInf(f, 0) {
			return 0, ErrStructFieldInexact
		}
		if f < -two63 || f >= two63 {
			return 0, ErrStructFieldOverflow
		}
		return int64(f), nil
	}
	return 0, ErrStructFieldInexact
}

func toUint64Exact(v any) (uint64, error) {
	switch x := v.(type) {
	case int64, int32, int16, int8:
		i := toI64(x)
		if i < 0 {
			return 0, ErrStructFieldOverflow
		}
		return uint64(i), nil
	case float64, float32:
		f := toF64(x)
		if f != math.Trunc(f) || math.IsInf(f, 0) {
			return 0, ErrStructFieldInexact
		}
		if f < 0 || f >= 2*two63 {
			return 0, ErrStructFieldOverflow
		}
		return uint64(f), nil
	}
	return 0, ErrStructFieldInexact
}

// toFloatExact converts an integer to float64 (or float32 when narrow)
// only if the conversion round-trips.
func toFloatExact(v any, narrow bool) (float64, error) {
	switch x := v.(type) {
	case int64, int32, int16, int8:
		i := toI64(x)
		var f float64
		if narrow {
			f = float64(float32(i))
		} else {
			f = float64(i)
		}
		if f < -two63 || f >= two63 || int64(f) != i {
			return 0, ErrStructFieldInexact
		}
		return f, nil
	case uint64, uint32, uint16, uint8:
		u := toU64(x)
		var f float64
		if narrow {
			f = float64(float32(u))
		} else {
			f = float64(u)
		}
		if f >= 2*two63 || uint64(f) != u {
			return 0, ErrStructFieldInexact
		}
		return f, nil
	case float32:
		return float64(x), nil
	}
	return 0, ErrStructFieldInexact
}

func toI64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	}
	return 0
}

func toU64(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case uint32:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint8:
		return uint64(x)
	}
	return 0
}

func toF64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	}
	return 0
}

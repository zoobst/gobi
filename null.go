package gobi

// Null is a nullable struct-field value for ToStructs and FromStructs.
// V holds the value and Valid reports whether the cell is non-null;
// the layout matches database/sql.Null[T].
//
// It is the value-typed alternative to a pointer field. ToStructs
// decodes a non-null cell into a *T field by allocating a fresh T; a
// Null[T] field holds the value inline, so it decodes at the same cost
// as a plain T field — half the allocations of *T on a mostly non-null
// column:
//
//	type Ping struct {
//	    ICAO24 string            `parquet:"icao24"`
//	    Alt    gobi.Null[int64]  `parquet:"altitude_baro"`
//	}
//
// ToStructs sets Valid=false (and leaves V at its zero value) for a
// null cell. FromStructs writes null when Valid is false and V
// otherwise, including a zero V: Null[time.Time]{Valid: true} writes
// the zero instant rather than the null a bare zero time.Time field
// becomes, so the column needs a unit that can hold it (e.g.
// `timestamp(microsecond)`).
//
// T is any type a plain field may have, except a pointer or a slice
// other than []byte: slices already carry nullability through nil.
// A Null[T] field can't be tagged required.
type Null[T any] struct {
	V     T
	Valid bool
}

// isGobiNull marks Null[T] for the struct planner. It is unexported so
// that only Null[T] satisfies nullMarker.
func (Null[T]) isGobiNull() {}

// nullMarker is the interface planStructFields uses to recognize a
// Null[T] field type, whatever T is.
type nullMarker interface{ isGobiNull() }

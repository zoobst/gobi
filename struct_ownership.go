package gobi

import (
	"bytes"
	"reflect"
	"strings"
	"sync"
)

// StructCopyValues makes ToStructs deep-copy every string and []byte
// it writes into a struct, instead of aliasing the Frame's Arrow
// buffers.
//
// By default ToStructs is zero-copy: a string or []byte field points
// into the column buffer it was read from. That is the fastest option
// when the structs are short-lived, but any one surviving row keeps
// the whole buffer reachable — a few long-lived rows can pin every
// batch they came from. It also means the Frame's buffers must stay
// valid for as long as the structs are used (they do with the default
// Go allocator; a pooling allocator would recycle them on Release).
// With StructCopyValues each value owns its memory, so the Frame can
// be released and its buffers collected as soon as ToStructs returns.
//
// Fields tagged `intern` are interned rather than copied, with or
// without this option (see StructInterner).
func StructCopyValues() StructOption {
	return func(o *structOpts) { o.copyValues = true }
}

// StructInterner supplies the interner used for fields tagged with
// the `intern` option, e.g. `parquet:"os,intern"` or
// `gobi:"carrier,intern"`. Pass the same StringInterner to every
// ToStructs call so rows from different batches share one copy of
// each distinct value. Without this option each ToStructs call uses
// its own uncapped interner.
//
// Interning suits low-cardinality columns (enums, device models,
// country codes): every row holding "Android" shares one string. On
// high-cardinality columns it only adds a map lookup — use
// StructCopyValues instead.
func StructInterner(in *StringInterner) StructOption {
	return func(o *structOpts) { o.interner = in }
}

// StringInterner deduplicates strings: Intern returns one shared,
// owned copy per distinct value. Safe for concurrent use.
//
// The cap bounds memory on a column that turns out to be
// high-cardinality: once the interner holds maxEntries values, new
// values are still copied (so the result never aliases the caller's
// memory) but are not remembered. Existing entries keep being shared.
type StringInterner struct {
	mu  sync.RWMutex
	m   map[string]string
	max int
}

// NewStringInterner returns an interner that remembers up to
// maxEntries distinct values. maxEntries <= 0 means no cap.
func NewStringInterner(maxEntries int) *StringInterner {
	return &StringInterner{m: make(map[string]string), max: maxEntries}
}

// Intern returns the shared copy of s, adding one if s is new and the
// interner is under its cap. The result never aliases s's memory, so
// s may point into a buffer that is about to be freed or reused.
func (in *StringInterner) Intern(s string) string {
	in.mu.RLock()
	v, ok := in.m[s]
	in.mu.RUnlock()
	if ok {
		return v
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if v, ok := in.m[s]; ok {
		return v
	}
	c := strings.Clone(s)
	if in.max <= 0 || len(in.m) < in.max {
		in.m[c] = c
	}
	return c
}

// Len reports how many distinct values the interner holds.
func (in *StringInterner) Len() int {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return len(in.m)
}

// structReader carries ToStructs' per-call ownership policy into the
// field readers.
type structReader struct {
	copyValues bool
	interner   *StringInterner
	coerce     bool // StructCoerceNumbers
}

// ownString applies the ownership policy to a string read from an
// Arrow buffer: interned when the field asks for it, cloned under
// StructCopyValues, passed through (aliased) otherwise.
func (r *structReader) ownString(s string, intern bool) string {
	if intern {
		return r.interner.Intern(s)
	}
	if r.copyValues {
		return strings.Clone(s)
	}
	return s
}

// ownBytes is ownString for []byte values (Binary columns, geometry
// WKB).
func (r *structReader) ownBytes(b []byte) []byte {
	if r.copyValues && b != nil {
		return bytes.Clone(b)
	}
	return b
}

// assignOwned sets fv from a value read out of an Arrow buffer,
// applying the ownership policy. Strings and []byte are assigned
// directly (re-boxing an owned copy into an interface would cost a
// second allocation per cell); everything else goes through
// assignScalar's type and overflow checks.
func (r *structReader) assignOwned(fv reflect.Value, v any, intern bool) error {
	switch x := v.(type) {
	case string:
		if fv.Kind() == reflect.String {
			fv.SetString(r.ownString(x, intern))
			return nil
		}
	case []byte:
		if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.Uint8 {
			fv.SetBytes(r.ownBytes(x))
			return nil
		}
	}
	if r.coerce {
		if handled, err := coerceNumber(fv, v); handled {
			return err
		}
	}
	return assignScalar(fv, v)
}

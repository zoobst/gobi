package geometry

import (
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
)

// CRS identifies a coordinate reference system by its EPSG code.
//
// It's a 4-byte handle: every vertex (Point) carries one, so its size
// is multiplied by every coordinate gobi holds. The label and the
// projected / geographic flag live in a process-wide registry and are
// read through Name and Projected. Register custom systems with
// RegisterCRS; an unregistered code has no name and reports
// Projected() == false.
//
// Only the EPSG code is authoritative for equality.
type CRS struct {
	EPSG int32
}

// Known CRSes. The set is intentionally small; add as needed.
var (
	WGS84          = CRS{EPSG: 4326}
	PseudoMercator = CRS{EPSG: 3857}
)

// crsInfo is the registry's per-code metadata.
type crsInfo struct {
	name      string
	projected bool
}

// builtinCRS seeds the registry. A plain package-level map literal, so
// it exists before any init() in this package runs (project.go
// registers the UTM zones from its init).
var builtinCRS = map[int32]crsInfo{
	4326: {name: "WGS 84", projected: false},
	3857: {name: "WGS 84 / Pseudo-Mercator", projected: true},
}

// registry is copy-on-write: lookups (Projected / Name, on hot paths)
// are a single atomic load; RegisterCRS copies the map under a mutex
// and swaps it in. nil means "still just builtinCRS".
var (
	registry   atomic.Pointer[map[int32]crsInfo]
	registryMu sync.Mutex
)

func registryMap() map[int32]crsInfo {
	if m := registry.Load(); m != nil {
		return *m
	}
	return builtinCRS
}

// LookupCRS returns the CRS for the given EPSG code, or ErrUnknownCRS
// if the code isn't registered.
func LookupCRS(epsg int32) (CRS, error) {
	if _, ok := registryMap()[epsg]; ok {
		return CRS{EPSG: epsg}, nil
	}
	return CRS{}, fmt.Errorf("%w: EPSG:%d", ErrUnknownCRS, epsg)
}

// RegisterCRS adds (or replaces) the registry entry for epsg and
// returns its handle. Safe for concurrent use with lookups.
func RegisterCRS(epsg int32, name string, projected bool) CRS {
	registryMu.Lock()
	defer registryMu.Unlock()
	next := maps.Clone(registryMap())
	next[epsg] = crsInfo{name: name, projected: projected}
	registry.Store(&next)
	return CRS{EPSG: epsg}
}

// Name is the registered human-readable label, or "" for an
// unregistered code.
func (c CRS) Name() string { return registryMap()[c.EPSG].name }

// Projected reports whether the CRS is projected (planar, linear
// units) rather than geographic. False for unregistered codes.
func (c CRS) Projected() bool { return registryMap()[c.EPSG].projected }

// Equal reports whether two CRSes refer to the same system.
func (c CRS) Equal(o CRS) bool { return c.EPSG == o.EPSG }

// Zero reports whether the CRS is the zero value.
func (c CRS) Zero() bool { return c == CRS{} }

func (c CRS) String() string {
	if c.Zero() {
		return "CRS:unset"
	}
	return fmt.Sprintf("EPSG:%d", c.EPSG)
}

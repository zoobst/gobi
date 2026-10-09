package geometry

import (
	_ "embed"
	json "encoding/json/v2"
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

// projjsonData is the raw JSON table mapping EPSG code (as string) →
// canonical PROJJSON object for every projected CRS gobi supports
// (EPSG:3857 plus all WGS 84 / UTM zones, 32601-32660 and 32701-32760).
// Extracted from pyproj at build-authoring time via
//
//	from pyproj import CRS
//	json.dump({str(c): json.loads(CRS.from_epsg(c).to_json()) for c in codes}, ...)
//
// Embedding the full pyproj output rather than hand-rolling a minimal
// blob is deliberate — pyproj rejects PROJJSON that lacks required
// fields (base_crs, conversion, coordinate_system, datum_ensemble), and
// synthesizing all of those correctly for each zone is more error-prone
// than just checking in what PROJ produces.
//
//go:embed projjson_data.json
var projjsonData []byte

var (
	projjsonOnce  sync.Once
	projjsonTable map[string]map[string]any
	projjsonErr   error
)

// PROJJSONFor returns the canonical PROJJSON object for the given EPSG
// code, or nil if we don't have one on file. Callers include this in
// GeoParquet's "geo" metadata so downstream readers (geopandas via
// pyproj) can round-trip the CRS.
func PROJJSONFor(epsg int32) map[string]any {
	projjsonOnce.Do(func() {
		projjsonTable = make(map[string]map[string]any, 121)
		projjsonErr = json.Unmarshal(projjsonData, &projjsonTable)
	})
	if projjsonErr != nil {
		return nil
	}
	key := itoa32(epsg)
	return projjsonTable[key]
}

// itoa32 is a small non-allocating int32→string helper. strconv.Itoa
// works fine too but this keeps crs.go free of the extra import.
func itoa32(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

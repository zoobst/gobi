package geometry

import (
	_ "embed"
	"encoding/json"
	"sync"
)

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
// works fine too but this keeps projjson.go free of the extra import.
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

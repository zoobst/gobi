package geometry

import "fmt"

// CRS identifies a coordinate reference system. Only EPSG code is authoritative
// for equality — Name is a human-readable label.
type CRS struct {
	EPSG      int32
	Name      string
	Projected bool
}

// Known CRSes. The set is intentionally small; add as needed.
var (
	WGS84          = CRS{EPSG: 4326, Name: "WGS 84", Projected: false}
	PseudoMercator = CRS{EPSG: 3857, Name: "WGS 84 / Pseudo-Mercator", Projected: true}
)

var registry = map[int32]CRS{
	WGS84.EPSG:          WGS84,
	PseudoMercator.EPSG: PseudoMercator,
}

// LookupCRS returns the CRS for the given EPSG code, or ErrUnknownCRS.
func LookupCRS(epsg int32) (CRS, error) {
	if c, ok := registry[epsg]; ok {
		return c, nil
	}
	return CRS{}, fmt.Errorf("%w: EPSG:%d", ErrUnknownCRS, epsg)
}

// RegisterCRS adds a CRS to the runtime registry. Overwrites any prior entry
// for the same EPSG code.
func RegisterCRS(c CRS) {
	registry[c.EPSG] = c
}

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

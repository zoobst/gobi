package geometry

import "strings"

// MultiPolygon is a collection of Polygons.
type MultiPolygon struct {
	Polygons []Polygon
	CRSValue CRS
	HasZ     bool
}

// NewMultiPolygon returns a 2D MultiPolygon wrapping polys.
func NewMultiPolygon(polys []Polygon, crs CRS) MultiPolygon {
	return MultiPolygon{Polygons: polys, CRSValue: crs}
}

// NewMultiPolygonZ returns a 3D MultiPolygon.
func NewMultiPolygonZ(polys []Polygon, crs CRS) MultiPolygon {
	return MultiPolygon{Polygons: polys, CRSValue: crs, HasZ: true}
}

func (m MultiPolygon) Type() Type { return TypeMultiPolygon }
func (m MultiPolygon) CRS() CRS   { return m.CRSValue }
func (m MultiPolygon) Is3D() bool { return m.HasZ }

// EstimateUTMCRS returns the CRS of the UTM zone covering the multipolygon's
// bounds midpoint.
func (m MultiPolygon) EstimateUTMCRS() (CRS, error) {
	b := m.Bounds()
	if b.Empty() {
		return CRS{}, ErrEmptyGeometry
	}
	return estimateUTMFromXY((b.MinX+b.MaxX)/2, (b.MinY+b.MaxY)/2, m.CRSValue)
}

// Centroid returns the area-weighted centroid of the multipolygon. Each
// component contributes its own centroid weighted by its planar area (or
// spherical area for geographic CRSes). Empty geometry returns the zero
// Point.
func (m MultiPolygon) Centroid() Point {
	var cx, cy, totalArea float64
	for _, p := range m.Polygons {
		p.CRSValue = m.CRSValue
		a, err := p.Area(UnitMeters)
		if err != nil || a == 0 {
			continue
		}
		c := p.Centroid()
		cx += c.X * a
		cy += c.Y * a
		totalArea += a
	}
	if totalArea == 0 {
		return Point{CRSValue: m.CRSValue}
	}
	return Point{X: cx / totalArea, Y: cy / totalArea, CRSValue: m.CRSValue}
}

func (m MultiPolygon) Bounds() Bounds {
	b := EmptyBounds()
	for _, p := range m.Polygons {
		b = b.Union(p.Bounds())
	}
	return b
}

// Area sums the planar (XY) area of every component polygon in u².
func (m MultiPolygon) Area(u Unit) (float64, error) {
	var total float64
	for _, p := range m.Polygons {
		p.CRSValue = m.CRSValue
		a, err := p.Area(u)
		if err != nil {
			return 0, err
		}
		total += a
	}
	return total, nil
}

// Contains reports whether pt lies inside any polygon in the collection.
func (m MultiPolygon) Contains(pt Point) bool {
	for _, p := range m.Polygons {
		if p.Contains(pt) {
			return true
		}
	}
	return false
}

func (m MultiPolygon) WKT() string {
	if len(m.Polygons) == 0 {
		if m.HasZ {
			return "MULTIPOLYGON Z EMPTY"
		}
		return "MULTIPOLYGON EMPTY"
	}
	var b strings.Builder
	if m.HasZ {
		b.WriteString("MULTIPOLYGON Z (")
	} else {
		b.WriteString("MULTIPOLYGON (")
	}
	for i, p := range m.Polygons {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, ring := range p.Rings {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('(')
			for k, pt := range ring {
				if k > 0 {
					b.WriteString(", ")
				}
				b.WriteString(formatCoord(pt.X))
				b.WriteByte(' ')
				b.WriteString(formatCoord(pt.Y))
				if m.HasZ {
					b.WriteByte(' ')
					b.WriteString(formatCoord(pt.Z))
				}
			}
			b.WriteByte(')')
		}
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String()
}

func (m MultiPolygon) AppendWKB(buf []byte) []byte {
	if m.HasZ {
		buf = appendWKBHeader(buf, wkbMultiPolygonZ)
	} else {
		buf = appendWKBHeader(buf, wkbMultiPolygon)
	}
	buf = appendUint32LE(buf, uint32(len(m.Polygons)))
	for _, p := range m.Polygons {
		// Force each inner polygon's dimensionality to match the container.
		p.HasZ = m.HasZ
		buf = p.AppendWKB(buf)
	}
	return buf
}

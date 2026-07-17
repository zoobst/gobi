package geometry

import "strings"

// MultiPoint is an unordered collection of points. Set HasZ to true to
// encode Z on WKB/WKT output.
type MultiPoint struct {
	Points   []Point
	CRSValue CRS
	HasZ     bool
}

func NewMultiPoint(pts []Point, crs CRS) MultiPoint {
	return MultiPoint{Points: pts, CRSValue: crs}
}

func NewMultiPointZ(pts []Point, crs CRS) MultiPoint {
	return MultiPoint{Points: pts, CRSValue: crs, HasZ: true}
}

func (m MultiPoint) Type() Type { return TypeMultiPoint }
func (m MultiPoint) CRS() CRS   { return m.CRSValue }
func (m MultiPoint) Is3D() bool { return m.HasZ }

// EstimateUTMCRS returns the CRS of the UTM zone covering the multipoint's
// bounds midpoint.
func (m MultiPoint) EstimateUTMCRS() (CRS, error) {
	b := m.Bounds()
	if b.Empty() {
		return CRS{}, ErrEmptyGeometry
	}
	return estimateUTMFromXY((b.MinX+b.MaxX)/2, (b.MinY+b.MaxY)/2, m.CRSValue)
}

// Centroid returns the arithmetic mean of the multipoint's points.
func (m MultiPoint) Centroid() Point {
	if len(m.Points) == 0 {
		return Point{CRSValue: m.CRSValue}
	}
	var sx, sy float64
	for _, p := range m.Points {
		sx += p.X
		sy += p.Y
	}
	n := float64(len(m.Points))
	return Point{X: sx / n, Y: sy / n, CRSValue: m.CRSValue}
}

func (m MultiPoint) Bounds() Bounds {
	b := EmptyBounds()
	for _, p := range m.Points {
		b = b.Extend(p.X, p.Y)
	}
	return b
}

func (m MultiPoint) WKT() string {
	if len(m.Points) == 0 {
		if m.HasZ {
			return "MULTIPOINT Z EMPTY"
		}
		return "MULTIPOINT EMPTY"
	}
	var b strings.Builder
	if m.HasZ {
		b.WriteString("MULTIPOINT Z (")
	} else {
		b.WriteString("MULTIPOINT (")
	}
	for i, p := range m.Points {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		b.WriteString(formatCoord(p.X))
		b.WriteByte(' ')
		b.WriteString(formatCoord(p.Y))
		if m.HasZ {
			b.WriteByte(' ')
			b.WriteString(formatCoord(p.Z))
		}
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String()
}

func (m MultiPoint) AppendWKB(buf []byte) []byte {
	if m.HasZ {
		buf = appendWKBHeader(buf, wkbMultiPointZ)
	} else {
		buf = appendWKBHeader(buf, wkbMultiPoint)
	}
	buf = appendUint32LE(buf, uint32(len(m.Points)))
	for _, p := range m.Points {
		// Ensure the inner point's dimensionality matches the container.
		p.HasZ = m.HasZ
		buf = p.AppendWKB(buf)
	}
	return buf
}

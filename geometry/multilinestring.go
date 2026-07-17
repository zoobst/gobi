package geometry

import (
	"math"
	"strings"
)

// MultiLineString is a collection of LineStrings.
type MultiLineString struct {
	Lines    []LineString
	CRSValue CRS
	HasZ     bool
}

// NewMultiLineString returns a 2D MultiLineString wrapping lines.
func NewMultiLineString(lines []LineString, crs CRS) MultiLineString {
	return MultiLineString{Lines: lines, CRSValue: crs}
}

// NewMultiLineStringZ returns a 3D MultiLineString.
func NewMultiLineStringZ(lines []LineString, crs CRS) MultiLineString {
	return MultiLineString{Lines: lines, CRSValue: crs, HasZ: true}
}

func (m MultiLineString) Type() Type { return TypeMultiLineString }
func (m MultiLineString) CRS() CRS   { return m.CRSValue }
func (m MultiLineString) Is3D() bool { return m.HasZ }

// EstimateUTMCRS returns the CRS of the UTM zone covering the multiline's
// bounds midpoint.
func (m MultiLineString) EstimateUTMCRS() (CRS, error) {
	b := m.Bounds()
	if b.Empty() {
		return CRS{}, ErrEmptyGeometry
	}
	return estimateUTMFromXY((b.MinX+b.MaxX)/2, (b.MinY+b.MaxY)/2, m.CRSValue)
}

// Centroid returns the length-weighted centroid of the multi-linestring,
// where each component contributes its centroid weighted by its planar
// length. Empty geometry returns the zero Point.
func (m MultiLineString) Centroid() Point {
	var cx, cy, total float64
	for _, l := range m.Lines {
		if len(l.Points) < 2 {
			continue
		}
		var segLen float64
		for i := 0; i < len(l.Points)-1; i++ {
			dx := l.Points[i+1].X - l.Points[i].X
			dy := l.Points[i+1].Y - l.Points[i].Y
			segLen += math.Sqrt(dx*dx + dy*dy)
		}
		if segLen == 0 {
			continue
		}
		c := l.Centroid()
		cx += c.X * segLen
		cy += c.Y * segLen
		total += segLen
	}
	if total == 0 {
		return Point{CRSValue: m.CRSValue}
	}
	return Point{X: cx / total, Y: cy / total, CRSValue: m.CRSValue}
}

func (m MultiLineString) Bounds() Bounds {
	b := EmptyBounds()
	for _, l := range m.Lines {
		b = b.Union(l.Bounds())
	}
	return b
}

// Length sums the length of every line in the requested unit (XY only).
func (m MultiLineString) Length(u Unit) (float64, error) {
	var total float64
	for _, l := range m.Lines {
		l.CRSValue = m.CRSValue
		d, err := l.Length(u)
		if err != nil {
			return 0, err
		}
		total += d
	}
	return total, nil
}

func (m MultiLineString) WKT() string {
	if len(m.Lines) == 0 {
		if m.HasZ {
			return "MULTILINESTRING Z EMPTY"
		}
		return "MULTILINESTRING EMPTY"
	}
	var b strings.Builder
	if m.HasZ {
		b.WriteString("MULTILINESTRING Z (")
	} else {
		b.WriteString("MULTILINESTRING (")
	}
	for i, l := range m.Lines {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, p := range l.Points {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatCoord(p.X))
			b.WriteByte(' ')
			b.WriteString(formatCoord(p.Y))
			if m.HasZ {
				b.WriteByte(' ')
				b.WriteString(formatCoord(p.Z))
			}
		}
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String()
}

func (m MultiLineString) AppendWKB(buf []byte) []byte {
	if m.HasZ {
		buf = appendWKBHeader(buf, wkbMultiLineStringZ)
	} else {
		buf = appendWKBHeader(buf, wkbMultiLineString)
	}
	buf = appendUint32LE(buf, uint32(len(m.Lines)))
	for _, l := range m.Lines {
		// Force each inner line's dimensionality to match the container.
		l.HasZ = m.HasZ
		buf = l.AppendWKB(buf)
	}
	return buf
}

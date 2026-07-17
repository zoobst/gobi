package geometry

import (
	"math"
	"strings"
)

// LineString is an ordered sequence of two or more points. Set HasZ to true
// to encode Z values in WKB/WKT output.
type LineString struct {
	Points   []Point
	CRSValue CRS
	HasZ     bool
}

// NewLineString returns a 2D LineString sharing the underlying slice.
func NewLineString(pts []Point, crs CRS) LineString {
	return LineString{Points: pts, CRSValue: crs}
}

// NewLineStringZ returns a 3D LineString.
func NewLineStringZ(pts []Point, crs CRS) LineString {
	return LineString{Points: pts, CRSValue: crs, HasZ: true}
}

func (l LineString) Type() Type { return TypeLineString }
func (l LineString) CRS() CRS   { return l.CRSValue }
func (l LineString) Is3D() bool { return l.HasZ }

// Bounds returns the XY bounding box. Z is ignored.
func (l LineString) Bounds() Bounds {
	b := EmptyBounds()
	for _, p := range l.Points {
		b = b.Extend(p.X, p.Y)
	}
	return b
}

// EstimateUTMCRS returns the CRS of the UTM zone covering the linestring's
// bounds midpoint. If the linestring is in a projected CRS, the midpoint is
// inverse-projected to WGS84 first.
func (l LineString) EstimateUTMCRS() (CRS, error) {
	b := l.Bounds()
	if b.Empty() {
		return CRS{}, ErrEmptyGeometry
	}
	return estimateUTMFromXY((b.MinX+b.MaxX)/2, (b.MinY+b.MaxY)/2, l.CRSValue)
}

// ToCRS reprojects the linestring into target.
func (l LineString) ToCRS(target CRS) (LineString, error) {
	g, err := Project(l, target)
	if err != nil {
		return LineString{}, err
	}
	return g.(LineString), nil
}

// Centroid returns the length-weighted midpoint of the linestring in
// planar XY. Each segment contributes its midpoint weighted by its planar
// length; the result carries l's CRS.
func (l LineString) Centroid() Point {
	if len(l.Points) == 0 {
		return Point{CRSValue: l.CRSValue}
	}
	if len(l.Points) == 1 {
		return Point{X: l.Points[0].X, Y: l.Points[0].Y, CRSValue: l.CRSValue}
	}
	var cx, cy, total float64
	for i := 0; i < len(l.Points)-1; i++ {
		a, b := l.Points[i], l.Points[i+1]
		dx := b.X - a.X
		dy := b.Y - a.Y
		segLen := math.Sqrt(dx*dx + dy*dy)
		if segLen == 0 {
			continue
		}
		mx := (a.X + b.X) / 2
		my := (a.Y + b.Y) / 2
		cx += mx * segLen
		cy += my * segLen
		total += segLen
	}
	if total == 0 {
		// All points identical; fall back to first point.
		return Point{X: l.Points[0].X, Y: l.Points[0].Y, CRSValue: l.CRSValue}
	}
	return Point{X: cx / total, Y: cy / total, CRSValue: l.CRSValue}
}

// Length returns the total planar (XY) length of the linestring in the
// requested unit. Z is ignored. Geographic CRSes use haversine; projected
// CRSes use planar distance.
func (l LineString) Length(u Unit) (float64, error) {
	if len(l.Points) < 2 {
		return 0, nil
	}
	total := 0.0
	for i := 0; i < len(l.Points)-1; i++ {
		a, b := l.Points[i], l.Points[i+1]
		var d float64
		var err error
		if l.CRSValue.Projected {
			d, err = Euclidean(a.X, a.Y, b.X, b.Y, u)
		} else {
			d, err = Haversine(a.X, a.Y, b.X, b.Y, u)
		}
		if err != nil {
			return 0, err
		}
		total += d
	}
	return total, nil
}

func (l LineString) WKT() string {
	if len(l.Points) == 0 {
		if l.HasZ {
			return "LINESTRING Z EMPTY"
		}
		return "LINESTRING EMPTY"
	}
	var b strings.Builder
	if l.HasZ {
		b.WriteString("LINESTRING Z (")
	} else {
		b.WriteString("LINESTRING (")
	}
	for i, p := range l.Points {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(formatCoord(p.X))
		b.WriteByte(' ')
		b.WriteString(formatCoord(p.Y))
		if l.HasZ {
			b.WriteByte(' ')
			b.WriteString(formatCoord(p.Z))
		}
	}
	b.WriteByte(')')
	return b.String()
}

func (l LineString) AppendWKB(buf []byte) []byte {
	if l.HasZ {
		buf = appendWKBHeader(buf, wkbLineStringZ)
	} else {
		buf = appendWKBHeader(buf, wkbLineString)
	}
	buf = appendUint32LE(buf, uint32(len(l.Points)))
	for _, p := range l.Points {
		buf = appendFloat64LE(buf, p.X)
		buf = appendFloat64LE(buf, p.Y)
		if l.HasZ {
			buf = appendFloat64LE(buf, p.Z)
		}
	}
	return buf
}

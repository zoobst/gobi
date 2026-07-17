package geometry

import (
	"math"
	"sort"
	"strings"
)

// Polygon is a planar surface defined by one exterior linear ring and zero or
// more interior rings (holes). Rings are lists of points; the first and last
// point of each ring must coincide (Polygon operations close rings implicitly
// if they don't).
type Polygon struct {
	Rings    [][]Point
	CRSValue CRS
	HasZ     bool
}

// NewPolygon returns a 2D Polygon with the given rings. The first ring is the
// exterior boundary; any subsequent rings are holes.
func NewPolygon(rings [][]Point, crs CRS) Polygon {
	return Polygon{Rings: rings, CRSValue: crs}
}

// NewPolygonZ returns a 3D Polygon.
func NewPolygonZ(rings [][]Point, crs CRS) Polygon {
	return Polygon{Rings: rings, CRSValue: crs, HasZ: true}
}

// SimplePolygon returns a Polygon with a single exterior ring.
func SimplePolygon(exterior []Point, crs CRS) Polygon {
	return Polygon{Rings: [][]Point{exterior}, CRSValue: crs}
}

func (p Polygon) Type() Type { return TypePolygon }
func (p Polygon) CRS() CRS   { return p.CRSValue }
func (p Polygon) Is3D() bool { return p.HasZ }

func (p Polygon) Bounds() Bounds {
	b := EmptyBounds()
	if len(p.Rings) == 0 {
		return b
	}
	for _, pt := range p.Rings[0] {
		b = b.Extend(pt.X, pt.Y)
	}
	return b
}

// Exterior returns the exterior ring, or nil if the polygon has no rings.
func (p Polygon) Exterior() []Point {
	if len(p.Rings) == 0 {
		return nil
	}
	return p.Rings[0]
}

// EstimateUTMCRS returns the CRS of the UTM zone covering the polygon's
// centroid. If the polygon is in a projected CRS, the centroid is
// inverse-projected to WGS84 first.
func (p Polygon) EstimateUTMCRS() (CRS, error) {
	if len(p.Rings) == 0 || len(p.Rings[0]) == 0 {
		return CRS{}, ErrEmptyGeometry
	}
	c := p.Centroid()
	return estimateUTMFromXY(c.X, c.Y, p.CRSValue)
}

// ToCRS reprojects the polygon into target.
func (p Polygon) ToCRS(target CRS) (Polygon, error) {
	g, err := Project(p, target)
	if err != nil {
		return Polygon{}, err
	}
	return g.(Polygon), nil
}

// Area returns the polygon area. For geographic CRSes the area is computed on
// a sphere of Earth radius and returned in u² (u ∈ km/mi/nmi/m/ft). For
// projected CRSes the area is planar (signed absolute value) in the CRS's
// linear unit², converted to u².
func (p Polygon) Area(u Unit) (float64, error) {
	if len(p.Rings) == 0 || len(p.Rings[0]) < 3 {
		return 0, nil
	}
	if p.CRSValue.Projected {
		a := planarRingArea(p.Rings[0])
		for _, hole := range p.Rings[1:] {
			a -= planarRingArea(hole)
		}
		perM, err := metersPerUnit(u)
		if err != nil {
			return 0, err
		}
		return a / (perM * perM), nil
	}
	// geographic — spherical excess
	perM, err := metersPerUnit(u)
	if err != nil {
		return 0, err
	}
	rMeters := EarthRadiusKM * 1000
	a := sphericalRingArea(p.Rings[0], rMeters)
	for _, hole := range p.Rings[1:] {
		a -= sphericalRingArea(hole, rMeters)
	}
	return math.Abs(a) / (perM * perM), nil
}

// Perimeter returns the length of the polygon's exterior ring in u.
func (p Polygon) Perimeter(u Unit) (float64, error) {
	if len(p.Rings) == 0 {
		return 0, nil
	}
	ring := closedRing(p.Rings[0])
	l := LineString{Points: ring, CRSValue: p.CRSValue}
	return l.Length(u)
}

// Centroid returns the area-weighted centroid of the exterior ring, using the
// planar (shoelace) formula. For small geographic polygons the result is a
// close approximation.
func (p Polygon) Centroid() Point {
	ring := p.Exterior()
	if len(ring) == 0 {
		return Point{CRSValue: p.CRSValue}
	}
	ring = closedRing(ring)
	var (
		cx, cy  float64
		areaTwo float64
		n               = len(ring) - 1
		sx, sy  float64 = 0, 0
	)
	for i := range n {
		x0, y0 := ring[i].X, ring[i].Y
		x1, y1 := ring[i+1].X, ring[i+1].Y
		cross := x0*y1 - x1*y0
		areaTwo += cross
		cx += (x0 + x1) * cross
		cy += (y0 + y1) * cross
		sx += x0
		sy += y0
	}
	if areaTwo == 0 {
		return Point{X: sx / float64(n), Y: sy / float64(n), CRSValue: p.CRSValue}
	}
	return Point{
		X:        cx / (3 * areaTwo),
		Y:        cy / (3 * areaTwo),
		CRSValue: p.CRSValue,
	}
}

// Contains reports whether pt lies inside p (exterior ring, minus any holes).
// The check uses the winding-parity (crossing) rule. Points on the boundary
// have undefined containment.
func (p Polygon) Contains(pt Point) bool {
	if len(p.Rings) == 0 || !p.Bounds().Contains(pt.X, pt.Y) {
		return false
	}
	if !pointInRing(pt, p.Rings[0]) {
		return false
	}
	for _, hole := range p.Rings[1:] {
		if pointInRing(pt, hole) {
			return false
		}
	}
	return true
}

func (p Polygon) WKT() string {
	if len(p.Rings) == 0 {
		if p.HasZ {
			return "POLYGON Z EMPTY"
		}
		return "POLYGON EMPTY"
	}
	var b strings.Builder
	if p.HasZ {
		b.WriteString("POLYGON Z (")
	} else {
		b.WriteString("POLYGON (")
	}
	for i, ring := range p.Rings {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, pt := range ring {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatCoord(pt.X))
			b.WriteByte(' ')
			b.WriteString(formatCoord(pt.Y))
			if p.HasZ {
				b.WriteByte(' ')
				b.WriteString(formatCoord(pt.Z))
			}
		}
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String()
}

func (p Polygon) AppendWKB(buf []byte) []byte {
	if p.HasZ {
		buf = appendWKBHeader(buf, wkbPolygonZ)
	} else {
		buf = appendWKBHeader(buf, wkbPolygon)
	}
	buf = appendUint32LE(buf, uint32(len(p.Rings)))
	for _, ring := range p.Rings {
		ring = closedRing(ring)
		buf = appendUint32LE(buf, uint32(len(ring)))
		for _, pt := range ring {
			buf = appendFloat64LE(buf, pt.X)
			buf = appendFloat64LE(buf, pt.Y)
			if p.HasZ {
				buf = appendFloat64LE(buf, pt.Z)
			}
		}
	}
	return buf
}

// ConvexHull returns the convex hull of the polygon's exterior points using
// the Graham scan. Points on collinear edges are dropped.
func (p Polygon) ConvexHull() Polygon {
	src := p.Exterior()
	if len(src) < 3 {
		return Polygon{Rings: [][]Point{append([]Point(nil), src...)}, CRSValue: p.CRSValue}
	}
	pts := append([]Point(nil), src...)

	lowIdx := 0
	for i, pt := range pts {
		if pt.Y < pts[lowIdx].Y || (pt.Y == pts[lowIdx].Y && pt.X < pts[lowIdx].X) {
			lowIdx = i
		}
	}
	pts[0], pts[lowIdx] = pts[lowIdx], pts[0]
	pivot := pts[0]

	rest := pts[1:]
	sort.Slice(rest, func(i, j int) bool {
		c := cross(pivot, rest[i], rest[j])
		if c == 0 {
			return distSq(pivot, rest[i]) < distSq(pivot, rest[j])
		}
		return c > 0
	})

	hull := make([]Point, 0, len(pts))
	hull = append(hull, pivot)
	for _, pt := range rest {
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], pt) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, pt)
	}
	return Polygon{Rings: [][]Point{closedRing(hull)}, CRSValue: p.CRSValue}
}

// closedRing returns r if it is already closed, otherwise a copy with the
// first point appended.
func closedRing(r []Point) []Point {
	if len(r) < 2 {
		return r
	}
	first, last := r[0], r[len(r)-1]
	if first.X == last.X && first.Y == last.Y {
		return r
	}
	out := make([]Point, len(r)+1)
	copy(out, r)
	out[len(r)] = first
	return out
}

func planarRingArea(ring []Point) float64 {
	if len(ring) < 3 {
		return 0
	}
	ring = closedRing(ring)
	var a float64
	for i := 0; i < len(ring)-1; i++ {
		a += ring[i].X*ring[i+1].Y - ring[i+1].X*ring[i].Y
	}
	return math.Abs(a) / 2
}

func sphericalRingArea(ring []Point, radiusM float64) float64 {
	if len(ring) < 3 {
		return 0
	}
	ring = closedRing(ring)
	var total float64
	for i := 0; i < len(ring)-1; i++ {
		λ1 := degToRad(ring[i].X)
		λ2 := degToRad(ring[i+1].X)
		φ1 := degToRad(ring[i].Y)
		φ2 := degToRad(ring[i+1].Y)
		total += (λ2 - λ1) * (math.Sin(φ1) + math.Sin(φ2))
	}
	return math.Abs(total*radiusM*radiusM) / 2
}

func pointInRing(pt Point, ring []Point) bool {
	if len(ring) < 3 {
		return false
	}
	ring = closedRing(ring)
	inside := false
	for i := 0; i < len(ring)-1; i++ {
		xi, yi := ring[i].X, ring[i].Y
		xj, yj := ring[i+1].X, ring[i+1].Y
		if (yi > pt.Y) != (yj > pt.Y) {
			xIntersect := (xj-xi)*(pt.Y-yi)/(yj-yi) + xi
			if pt.X < xIntersect {
				inside = !inside
			}
		}
	}
	return inside
}

func cross(a, b, c Point) float64 {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

func distSq(a, b Point) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return dx*dx + dy*dy
}

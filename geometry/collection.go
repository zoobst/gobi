package geometry

import "strings"

// GeometryCollection is a heterogeneous set of geometries. Per the OGC spec
// and PostGIS's practical guarantee, a GeometryCollection may not directly
// contain another GeometryCollection. Setting HasZ marks the collection as
// 3D; each contained geometry is expected to share that dimensionality.
type GeometryCollection struct {
	Geometries []Geometry
	CRSValue   CRS
	HasZ       bool
}

// NewGeometryCollection returns a 2D GeometryCollection wrapping the given
// geometries.
func NewGeometryCollection(gs []Geometry, crs CRS) GeometryCollection {
	return GeometryCollection{Geometries: gs, CRSValue: crs}
}

// NewGeometryCollectionZ returns a 3D GeometryCollection.
func NewGeometryCollectionZ(gs []Geometry, crs CRS) GeometryCollection {
	return GeometryCollection{Geometries: gs, CRSValue: crs, HasZ: true}
}

func (c GeometryCollection) Type() Type { return TypeGeometryCollection }
func (c GeometryCollection) CRS() CRS   { return c.CRSValue }
func (c GeometryCollection) Is3D() bool { return c.HasZ }

// EstimateUTMCRS returns the CRS of the UTM zone covering the collection's
// bounds midpoint.
func (c GeometryCollection) EstimateUTMCRS() (CRS, error) {
	b := c.Bounds()
	if b.Empty() {
		return CRS{}, ErrEmptyGeometry
	}
	return estimateUTMFromXY((b.MinX+b.MaxX)/2, (b.MinY+b.MaxY)/2, c.CRSValue)
}

// Centroid returns the midpoint of the collection's overall bounding box.
// A meaningful centroid for a heterogeneous collection is not universally
// defined; the bounds midpoint is a stable, cheap approximation.
func (c GeometryCollection) Centroid() Point {
	b := c.Bounds()
	if b.Empty() {
		return Point{CRSValue: c.CRSValue}
	}
	return Point{
		X:        (b.MinX + b.MaxX) / 2,
		Y:        (b.MinY + b.MaxY) / 2,
		CRSValue: c.CRSValue,
	}
}

func (c GeometryCollection) Bounds() Bounds {
	b := EmptyBounds()
	for _, g := range c.Geometries {
		b = b.Union(g.Bounds())
	}
	return b
}

func (c GeometryCollection) WKT() string {
	if len(c.Geometries) == 0 {
		if c.HasZ {
			return "GEOMETRYCOLLECTION Z EMPTY"
		}
		return "GEOMETRYCOLLECTION EMPTY"
	}
	var b strings.Builder
	if c.HasZ {
		b.WriteString("GEOMETRYCOLLECTION Z (")
	} else {
		b.WriteString("GEOMETRYCOLLECTION (")
	}
	for i, g := range c.Geometries {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(g.WKT())
	}
	b.WriteByte(')')
	return b.String()
}

func (c GeometryCollection) AppendWKB(buf []byte) []byte {
	if c.HasZ {
		buf = appendWKBHeader(buf, wkbGeometryCollectionZ)
	} else {
		buf = appendWKBHeader(buf, wkbGeometryCollection)
	}
	buf = appendUint32LE(buf, uint32(len(c.Geometries)))
	for _, g := range c.Geometries {
		buf = g.AppendWKB(buf)
	}
	return buf
}

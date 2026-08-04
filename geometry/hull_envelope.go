package geometry

// ConvexHull returns the convex hull of every vertex in g as a Polygon.
// Handles all concrete geometry types by extracting vertices and running
// a Graham scan over the union. Returns an empty Polygon if g has fewer
// than 3 unique vertices.
func ConvexHull(g Geometry) Polygon {
	if g == nil {
		return Polygon{}
	}
	pts := collectVertices(g)
	if len(pts) < 3 {
		return Polygon{CRSValue: g.CRS()}
	}
	// Polygon.ConvexHull runs Graham scan over its exterior ring —
	// which is exactly what we want. The temporary polygon isn't
	// otherwise a valid shape but ConvexHull doesn't care.
	tmp := Polygon{Rings: [][]Point{pts}, CRSValue: g.CRS()}
	return tmp.ConvexHull()
}

// Envelope returns the axis-aligned bounding-box polygon of g. Matches
// geopandas's GeoSeries.envelope: 5-vertex closed ring (MinXY, MaxX/MinY,
// MaxXY, MinX/MaxY, close). Empty input returns a zero-ring Polygon.
func Envelope(g Geometry) Polygon {
	if g == nil {
		return Polygon{}
	}
	b := g.Bounds()
	if b.Empty() {
		return Polygon{CRSValue: g.CRS()}
	}
	crs := g.CRS()
	ring := []Point{
		{X: b.MinX, Y: b.MinY, CRSValue: crs},
		{X: b.MaxX, Y: b.MinY, CRSValue: crs},
		{X: b.MaxX, Y: b.MaxY, CRSValue: crs},
		{X: b.MinX, Y: b.MaxY, CRSValue: crs},
		{X: b.MinX, Y: b.MinY, CRSValue: crs},
	}
	return Polygon{Rings: [][]Point{ring}, CRSValue: crs}
}

// collectVertices flattens every vertex out of g into a single slice.
// The output slice is fresh and doesn't alias g's ring storage, so
// callers may mutate it freely.
func collectVertices(g Geometry) []Point {
	switch t := g.(type) {
	case Point:
		return []Point{t}
	case MultiPoint:
		return append([]Point(nil), t.Points...)
	case LineString:
		return append([]Point(nil), t.Points...)
	case MultiLineString:
		var out []Point
		for _, l := range t.Lines {
			out = append(out, l.Points...)
		}
		return out
	case Polygon:
		var out []Point
		for _, r := range t.Rings {
			out = append(out, r...)
		}
		return out
	case MultiPolygon:
		var out []Point
		for _, p := range t.Polygons {
			for _, r := range p.Rings {
				out = append(out, r...)
			}
		}
		return out
	case GeometryCollection:
		var out []Point
		for _, inner := range t.Geometries {
			out = append(out, collectVertices(inner)...)
		}
		return out
	}
	return nil
}

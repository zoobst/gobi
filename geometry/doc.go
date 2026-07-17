// Package geometry provides 2D geometry primitives (Point, LineString, Polygon,
// MultiPoint) with WKB and WKT encoding, a coordinate reference system model,
// and common spatial operations (area, distance, centroid, convex hull,
// intersection tests).
//
// All coordinates are 2D. XY order (X = longitude/easting, Y = latitude/northing)
// matches the WKB, WKT, and GeoJSON specifications. Angles are always in
// degrees at the API surface; internal trig uses radians.
package geometry

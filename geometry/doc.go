// Package geometry provides 2D geometry primitives (Point, LineString, Polygon,
// MultiPoint) with WKB and WKT encoding, a coordinate reference system model,
// and common spatial operations (area, distance, centroid, convex hull,
// intersection tests, polygon boolean ops).
//
// All coordinates are 2D. XY order (X = longitude/easting, Y = latitude/northing)
// matches the WKB, WKT, and GeoJSON specifications. Angles are always in
// degrees at the API surface; internal trig uses radians.
//
// # The same results on every platform
//
// Every function here returns the same bits on arm64, amd64 and wasm,
// so a buffer or a reprojection computed in a browser is byte-for-byte
// the one a server computes. Two rules keep it so, and each has a test:
//
//   - A product is rounded before it is added to or subtracted from
//     anything: float64(x*y) + z, never x*y + z. Left as it is, the
//     compiler fuses the second form into one instruction on some
//     platforms and not others, and the last bit differs.
//     TestNoFusedMultiplyAdd fails if any is left.
//   - Sin, Cos, Atan2, Hypot and the other transcendental functions
//     come from internal/detmath, not from math, whose own results
//     differ between platforms for the same reason.
//
// TestSameResultsOnEveryPlatform checks the outcome end to end.
package geometry

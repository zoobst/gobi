package geometry

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseWKT parses a Well-Known Text geometry. Whitespace and case around the
// type keyword are tolerated; a " Z" qualifier after the keyword (e.g.
// "POINT Z (1 2 3)") switches the parser into 3D mode. Coordinates must be
// numeric.
func ParseWKT(s string) (Geometry, error) {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "POINT"):
		return parsePointWKT(detectZ(s[len("POINT"):]))
	case strings.HasPrefix(upper, "LINESTRING"):
		return parseLineStringWKT(detectZ(s[len("LINESTRING"):]))
	case strings.HasPrefix(upper, "POLYGON"):
		return parsePolygonWKT(detectZ(s[len("POLYGON"):]))
	case strings.HasPrefix(upper, "MULTIPOINT"):
		return parseMultiPointWKT(detectZ(s[len("MULTIPOINT"):]))
	case strings.HasPrefix(upper, "MULTILINESTRING"):
		return parseMultiLineStringWKT(detectZ(s[len("MULTILINESTRING"):]))
	case strings.HasPrefix(upper, "MULTIPOLYGON"):
		return parseMultiPolygonWKT(detectZ(s[len("MULTIPOLYGON"):]))
	case strings.HasPrefix(upper, "GEOMETRYCOLLECTION"):
		return parseGeometryCollectionWKT(detectZ(s[len("GEOMETRYCOLLECTION"):]))
	default:
		return nil, fmt.Errorf("%w: unknown geometry keyword", ErrInvalidWKT)
	}
}

// detectZ strips a leading " Z " qualifier (case-insensitive) from rest and
// returns (remaining, hasZ). Also tolerates "Z(" with no space.
func detectZ(rest string) (string, bool) {
	r := strings.TrimLeft(rest, " \t")
	if len(r) == 0 {
		return rest, false
	}
	if (r[0] == 'Z' || r[0] == 'z') &&
		(len(r) == 1 || r[1] == ' ' || r[1] == '\t' || r[1] == '(') {
		return strings.TrimSpace(r[1:]), true
	}
	return rest, false
}

func parsePointWKT(rest string, hasZ bool) (Point, error) {
	body, err := trimParens(rest)
	if err != nil {
		return Point{}, err
	}
	x, y, z, gotZ, err := parseCoord(body)
	if err != nil {
		return Point{}, err
	}
	if hasZ && !gotZ {
		return Point{}, fmt.Errorf("%w: POINT Z requires a Z value", ErrInvalidWKT)
	}
	p := Point{X: x, Y: y}
	if hasZ {
		p.Z = z
		p.HasZ = true
	}
	return p, nil
}

func parseLineStringWKT(rest string, hasZ bool) (LineString, error) {
	body, err := trimParens(rest)
	if err != nil {
		return LineString{}, err
	}
	pts, err := parseCoordList(body, hasZ)
	if err != nil {
		return LineString{}, err
	}
	return LineString{Points: pts, HasZ: hasZ}, nil
}

func parseMultiPointWKT(rest string, hasZ bool) (MultiPoint, error) {
	body, err := trimParens(rest)
	if err != nil {
		return MultiPoint{}, err
	}
	// MultiPoint can be "MULTIPOINT (x y, x y)" or "MULTIPOINT ((x y), (x y))".
	// Strip inner parens if present.
	body = strings.ReplaceAll(body, "(", "")
	body = strings.ReplaceAll(body, ")", "")
	pts, err := parseCoordList(body, hasZ)
	if err != nil {
		return MultiPoint{}, err
	}
	return MultiPoint{Points: pts, HasZ: hasZ}, nil
}

func parsePolygonWKT(rest string, hasZ bool) (Polygon, error) {
	body, err := trimParens(rest)
	if err != nil {
		return Polygon{}, err
	}
	rings := splitRings(body)
	out := make([][]Point, 0, len(rings))
	for _, r := range rings {
		pts, err := parseCoordList(r, hasZ)
		if err != nil {
			return Polygon{}, err
		}
		out = append(out, pts)
	}
	return Polygon{Rings: out, HasZ: hasZ}, nil
}

func parseMultiLineStringWKT(rest string, hasZ bool) (MultiLineString, error) {
	body, err := trimParens(rest)
	if err != nil {
		return MultiLineString{}, err
	}
	rings := splitRings(body)
	lines := make([]LineString, 0, len(rings))
	for _, r := range rings {
		pts, err := parseCoordList(r, hasZ)
		if err != nil {
			return MultiLineString{}, err
		}
		lines = append(lines, LineString{Points: pts, HasZ: hasZ})
	}
	return MultiLineString{Lines: lines, HasZ: hasZ}, nil
}

func parseMultiPolygonWKT(rest string, hasZ bool) (MultiPolygon, error) {
	body, err := trimParens(rest)
	if err != nil {
		return MultiPolygon{}, err
	}
	polyExprs := splitAtDepth(body, 0)
	polys := make([]Polygon, 0, len(polyExprs))
	for _, pe := range polyExprs {
		// Each poly expression is "(ring)" or "((ring),(hole))" — parse it
		// with parsePolygonWKT under the same hasZ flag.
		poly, err := parsePolygonWKT(pe, hasZ)
		if err != nil {
			return MultiPolygon{}, err
		}
		polys = append(polys, poly)
	}
	return MultiPolygon{Polygons: polys, HasZ: hasZ}, nil
}

func parseGeometryCollectionWKT(rest string, hasZ bool) (GeometryCollection, error) {
	body, err := trimParens(rest)
	if err != nil {
		return GeometryCollection{}, err
	}
	parts := splitAtDepth(body, 0)
	gs := make([]Geometry, 0, len(parts))
	for _, p := range parts {
		g, err := ParseWKT(strings.TrimSpace(p))
		if err != nil {
			return GeometryCollection{}, err
		}
		gs = append(gs, g)
	}
	return GeometryCollection{Geometries: gs, HasZ: hasZ}, nil
}

// splitAtDepth splits s on commas that occur at the given paren-nesting
// depth (0 = outermost).
func splitAtDepth(s string, depth int) []string {
	var out []string
	var buf strings.Builder
	d := 0
	for _, r := range s {
		switch r {
		case '(':
			d++
			buf.WriteRune(r)
		case ')':
			d--
			buf.WriteRune(r)
		case ',':
			if d == depth {
				out = append(out, strings.TrimSpace(buf.String()))
				buf.Reset()
				continue
			}
			buf.WriteRune(r)
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}

// trimParens strips whitespace and one outer set of parentheses.
func trimParens(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return "", fmt.Errorf("%w: missing parentheses", ErrInvalidWKT)
	}
	return s[1 : len(s)-1], nil
}

// splitRings splits a polygon body on the ring boundary while preserving
// contents inside each ring's parens.
func splitRings(s string) []string {
	var (
		out   []string
		depth int
		start int
	)
	for i, r := range s {
		switch r {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				out = append(out, s[start:i])
			}
		}
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

func parseCoordList(s string, hasZ bool) ([]Point, error) {
	parts := strings.Split(s, ",")
	pts := make([]Point, 0, len(parts))
	for _, p := range parts {
		x, y, z, gotZ, err := parseCoord(p)
		if err != nil {
			return nil, err
		}
		if hasZ && !gotZ {
			return nil, fmt.Errorf("%w: Z-flagged geometry missing Z value in %q",
				ErrInvalidWKT, p)
		}
		pt := Point{X: x, Y: y}
		if hasZ {
			pt.Z = z
			pt.HasZ = true
		}
		pts = append(pts, pt)
	}
	return pts, nil
}

// parseCoord parses a whitespace-separated coordinate tuple with 2 or 3
// numeric components and returns (x, y, z, gotZ, err). Extra components
// beyond 3 are rejected.
func parseCoord(s string) (float64, float64, float64, bool, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 2 {
		return 0, 0, 0, false, fmt.Errorf("%w: expected 2 or 3 numbers, got %q",
			ErrInvalidWKT, s)
	}
	if len(fields) > 3 {
		return 0, 0, 0, false, fmt.Errorf("%w: too many numbers in %q", ErrInvalidWKT, s)
	}
	x, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("%w: %v", ErrInvalidWKT, err)
	}
	y, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("%w: %v", ErrInvalidWKT, err)
	}
	if len(fields) == 3 {
		z, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return 0, 0, 0, false, fmt.Errorf("%w: %v", ErrInvalidWKT, err)
		}
		return x, y, z, true, nil
	}
	return x, y, 0, false, nil
}

// formatCoord formats a float without trailing zeros, matching typical
// PostGIS WKT output.
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// detectZTuple wraps detectZ into a signature suitable for parsers that
// return (string, bool). Preserved for reference / future use.
var _ = detectZ

package shpio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// WriteFile writes f to base+".shp", base+".shx", base+".dbf" (and a
// minimal base+".prj" when the geometry column carries a known EPSG).
// base may include or omit ".shp"; both are accepted.
//
// The geometry column is picked automatically (the first tagged geometry
// column, or "geometry" if present). All rows must share a compatible
// shape type — mixed Point / LineString / Polygon rows are rejected.
// Pass nil opts for defaults.
func WriteFile(f *gobi.Frame, base string, opts *WriteOptions) error {
	_ = opts // reserved
	base = strings.TrimSuffix(base, ".shp")

	geomCol, geomName, err := findGeometryColumn(f)
	if err != nil {
		return err
	}
	shapeType, hasZ, err := detectShapeType(geomCol)
	if err != nil {
		return err
	}

	// Assemble the .shp records + a bbox for the file header. Every
	// record's file-offset (in 16-bit words) also feeds the .shx.
	var (
		shpBody    bytes.Buffer
		shxEntries []shxEntry
		fileBbox   = geometry.EmptyBounds()
		minZ       = math.Inf(1)
		maxZ       = math.Inf(-1)
		hasAnyZ    = false
	)

	n := f.NumRows()
	for row := range n {
		g, err := geomCol.Geometry(row)
		if err != nil {
			return err
		}
		before := shpBody.Len()
		recordOffsetWords := (100 + before) / 2 // .shx wants file offset in 16-bit words

		// Record header: recordNumber (BE u32, 1-based) + contentLengthWords (BE u32).
		// Length is computed after the body is serialized.
		var recBody bytes.Buffer
		if err := writeRecordBody(&recBody, shapeType, hasZ, g); err != nil {
			return err
		}
		contentBytes := recBody.Len()
		if contentBytes%2 != 0 {
			// spec requires 16-bit word alignment; pad if needed.
			recBody.WriteByte(0)
			contentBytes++
		}
		contentWords := uint32(contentBytes / 2)

		if err := writeUint32BE(&shpBody, uint32(row+1)); err != nil {
			return err
		}
		if err := writeUint32BE(&shpBody, contentWords); err != nil {
			return err
		}
		if _, err := shpBody.Write(recBody.Bytes()); err != nil {
			return err
		}

		// Track file bbox / z range and record the .shx entry.
		if g != nil {
			b := g.Bounds()
			fileBbox = fileBbox.Union(b)
			if hasZ {
				trackZ(g, &minZ, &maxZ, &hasAnyZ)
			}
		}
		shxEntries = append(shxEntries, shxEntry{
			offsetWords:  uint32(recordOffsetWords),
			contentWords: contentWords,
		})
	}

	// File header (100 bytes). fileLength is expressed in 16-bit words.
	fileLenWords := uint32((100 + shpBody.Len()) / 2)
	shpOut := new(bytes.Buffer)
	if err := writeSHPHeader(shpOut, shapeType, fileLenWords, fileBbox, hasZ, minZ, maxZ, hasAnyZ); err != nil {
		return err
	}
	shpOut.Write(shpBody.Bytes())

	// .shx: same 100-byte header, then 8 bytes per record (offset + length).
	shxOut := new(bytes.Buffer)
	shxLenWords := uint32((100 + 8*len(shxEntries)) / 2)
	if err := writeSHPHeader(shxOut, shapeType, shxLenWords, fileBbox, hasZ, minZ, maxZ, hasAnyZ); err != nil {
		return err
	}
	for _, e := range shxEntries {
		if err := writeUint32BE(shxOut, e.offsetWords); err != nil {
			return err
		}
		if err := writeUint32BE(shxOut, e.contentWords); err != nil {
			return err
		}
	}

	// .dbf: attribute columns (everything except the geometry column).
	dbfOut, err := buildDBF(f, geomName)
	if err != nil {
		return err
	}

	// Write all three files.
	if err := os.WriteFile(base+".shp", shpOut.Bytes(), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(base+".shx", shxOut.Bytes(), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(base+".dbf", dbfOut, 0644); err != nil {
		return err
	}

	// Optional .prj sidecar based on the geometry column's tagged EPSG.
	if prj := prjTextForCol(geomCol); prj != "" {
		if err := os.WriteFile(base+".prj", []byte(prj), 0644); err != nil {
			return err
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// SHP writer helpers
// -----------------------------------------------------------------------------

type shxEntry struct {
	offsetWords  uint32
	contentWords uint32
}

// findGeometryColumn returns the first geometry-tagged column in f, or an
// error if none exists.
func findGeometryColumn(f *gobi.Frame) (gobi.Series, string, error) {
	for _, name := range f.ColumnNames() {
		s, err := f.Column(name)
		if err != nil {
			return gobi.Series{}, "", err
		}
		if s.IsGeometry() {
			return s, name, nil
		}
	}
	return gobi.Series{}, "", fmt.Errorf("shpio: frame has no geometry column")
}

// detectShapeType inspects the first non-null geometry in col and picks a
// Shapefile shape type. All rows must share a compatible type; mixed
// primitive shapes trigger an error.
func detectShapeType(col gobi.Series) (int, bool, error) {
	var (
		shapeType = -1
		hasZ      = false
	)
	for i := range col.Len() {
		g, err := col.Geometry(i)
		if err != nil {
			return 0, false, err
		}
		if g == nil {
			continue
		}
		st, z, err := gobiToShape(g)
		if err != nil {
			return 0, false, err
		}
		if shapeType == -1 {
			shapeType = st
			hasZ = z
			continue
		}
		// Point / MultiPoint may coexist as MultiPoint; treat that as a
		// case of unified MultiPoint. Otherwise require the exact same
		// type.
		if st != shapeType {
			return 0, false, fmt.Errorf("shpio: mixed geometry types in column (%d vs %d) — Shapefile requires one shape type per file", shapeType, st)
		}
		if z != hasZ {
			return 0, false, fmt.Errorf("shpio: mixed 2D/3D geometries in column — Shapefile requires uniform dimension")
		}
	}
	if shapeType == -1 {
		return ShapeNull, false, nil
	}
	return shapeType, hasZ, nil
}

// gobiToShape maps a gobi geometry to the Shapefile shape type + hasZ.
func gobiToShape(g geometry.Geometry) (int, bool, error) {
	switch t := g.(type) {
	case geometry.Point:
		if t.HasZ {
			return ShapePointZ, true, nil
		}
		return ShapePoint, false, nil
	case geometry.MultiPoint:
		if t.HasZ {
			return ShapeMultiPointZ, true, nil
		}
		return ShapeMultiPoint, false, nil
	case geometry.LineString:
		if t.HasZ {
			return ShapePolyLineZ, true, nil
		}
		return ShapePolyLine, false, nil
	case geometry.MultiLineString:
		if t.HasZ {
			return ShapePolyLineZ, true, nil
		}
		return ShapePolyLine, false, nil
	case geometry.Polygon:
		if t.HasZ {
			return ShapePolygonZ, true, nil
		}
		return ShapePolygon, false, nil
	case geometry.MultiPolygon:
		if t.HasZ {
			return ShapePolygonZ, true, nil
		}
		return ShapePolygon, false, nil
	}
	return 0, false, fmt.Errorf("shpio: unsupported geometry %T", g)
}

// writeSHPHeader emits the 100-byte file header used by both .shp and .shx.
func writeSHPHeader(w *bytes.Buffer, shapeType int, fileLenWords uint32,
	bb geometry.Bounds, hasZ bool, minZ, maxZ float64, hasAnyZ bool) error {
	// Bytes 0-3: file code (big-endian 9994)
	writeUint32BE(w, shpFileCode)
	// Bytes 4-23: 5 unused big-endian uint32 zeros
	for range 5 {
		writeUint32BE(w, 0)
	}
	// Bytes 24-27: file length (big-endian, in 16-bit words)
	writeUint32BE(w, fileLenWords)
	// Bytes 28-31: version (little-endian 1000)
	writeUint32LE(w, 1000)
	// Bytes 32-35: shape type (little-endian)
	writeUint32LE(w, uint32(shapeType))
	// Bounds: Xmin Ymin Xmax Ymax (little-endian float64)
	if bb.Empty() {
		bb = geometry.Bounds{}
	}
	writeFloat64LE(w, bb.MinX)
	writeFloat64LE(w, bb.MinY)
	writeFloat64LE(w, bb.MaxX)
	writeFloat64LE(w, bb.MaxY)
	// Zmin / Zmax
	if hasZ && hasAnyZ {
		writeFloat64LE(w, minZ)
		writeFloat64LE(w, maxZ)
	} else {
		writeFloat64LE(w, 0)
		writeFloat64LE(w, 0)
	}
	// Mmin / Mmax (unused — we don't support M variants)
	writeFloat64LE(w, 0)
	writeFloat64LE(w, 0)
	return nil
}

// writeRecordBody serializes a single geometry into a shape body (starts
// with the little-endian shape type followed by the shape-specific body).
func writeRecordBody(w *bytes.Buffer, shapeType int, hasZ bool, g geometry.Geometry) error {
	if g == nil {
		return writeUint32LE(w, ShapeNull)
	}
	writeUint32LE(w, uint32(shapeType))
	switch t := g.(type) {
	case geometry.Point:
		writeFloat64LE(w, t.X)
		writeFloat64LE(w, t.Y)
		if hasZ {
			writeFloat64LE(w, t.Z)
		}
		return nil
	case geometry.MultiPoint:
		return writeMultiPoint(w, t.Points, hasZ)
	case geometry.LineString:
		return writePolyLike(w, [][]geometry.Point{t.Points}, hasZ)
	case geometry.MultiLineString:
		parts := make([][]geometry.Point, len(t.Lines))
		for i, l := range t.Lines {
			parts[i] = l.Points
		}
		return writePolyLike(w, parts, hasZ)
	case geometry.Polygon:
		rings := orientPolygonRings(t.Rings)
		return writePolyLike(w, rings, hasZ)
	case geometry.MultiPolygon:
		var rings [][]geometry.Point
		for _, p := range t.Polygons {
			rings = append(rings, orientPolygonRings(p.Rings)...)
		}
		return writePolyLike(w, rings, hasZ)
	}
	return fmt.Errorf("shpio: unsupported geometry %T in writer", g)
}

// writeMultiPoint emits a MultiPoint / MultiPointZ body starting after the
// shape-type header.
func writeMultiPoint(w *bytes.Buffer, pts []geometry.Point, hasZ bool) error {
	bb := boundsOf(pts)
	writeFloat64LE(w, bb.MinX)
	writeFloat64LE(w, bb.MinY)
	writeFloat64LE(w, bb.MaxX)
	writeFloat64LE(w, bb.MaxY)
	writeUint32LE(w, uint32(len(pts)))
	for _, p := range pts {
		writeFloat64LE(w, p.X)
		writeFloat64LE(w, p.Y)
	}
	if hasZ {
		minZ, maxZ := zRange(pts)
		writeFloat64LE(w, minZ)
		writeFloat64LE(w, maxZ)
		for _, p := range pts {
			writeFloat64LE(w, p.Z)
		}
	}
	return nil
}

// writePolyLike emits a PolyLine / Polygon / *Z body (parts + points). The
// caller supplies the ring/part list already oriented as needed.
func writePolyLike(w *bytes.Buffer, parts [][]geometry.Point, hasZ bool) error {
	var all []geometry.Point
	starts := make([]int, len(parts))
	for i, p := range parts {
		starts[i] = len(all)
		all = append(all, p...)
	}
	bb := boundsOf(all)
	writeFloat64LE(w, bb.MinX)
	writeFloat64LE(w, bb.MinY)
	writeFloat64LE(w, bb.MaxX)
	writeFloat64LE(w, bb.MaxY)
	writeUint32LE(w, uint32(len(parts)))
	writeUint32LE(w, uint32(len(all)))
	for _, s := range starts {
		writeUint32LE(w, uint32(s))
	}
	for _, p := range all {
		writeFloat64LE(w, p.X)
		writeFloat64LE(w, p.Y)
	}
	if hasZ {
		minZ, maxZ := zRange(all)
		writeFloat64LE(w, minZ)
		writeFloat64LE(w, maxZ)
		for _, p := range all {
			writeFloat64LE(w, p.Z)
		}
	}
	return nil
}

// orientPolygonRings ensures exterior rings are clockwise and interior
// rings are counter-clockwise, per the Shapefile spec.
func orientPolygonRings(rings [][]geometry.Point) [][]geometry.Point {
	out := make([][]geometry.Point, len(rings))
	for i, r := range rings {
		wantCW := (i == 0) // exterior
		if ringIsCW(r) == wantCW {
			out[i] = r
			continue
		}
		// Reverse.
		rev := make([]geometry.Point, len(r))
		for j := range r {
			rev[j] = r[len(r)-1-j]
		}
		out[i] = rev
	}
	return out
}

// boundsOf computes the XY bounding box of a point slice.
func boundsOf(pts []geometry.Point) geometry.Bounds {
	b := geometry.EmptyBounds()
	for _, p := range pts {
		b = b.Extend(p.X, p.Y)
	}
	if b.Empty() {
		return geometry.Bounds{}
	}
	return b
}

// zRange returns min/max Z across a point slice, ignoring HasZ (the
// caller has already decided this record is 3D).
func zRange(pts []geometry.Point) (mn, mx float64) {
	mn, mx = math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		if p.Z < mn {
			mn = p.Z
		}
		if p.Z > mx {
			mx = p.Z
		}
	}
	if math.IsInf(mn, 1) {
		return 0, 0
	}
	return mn, mx
}

func trackZ(g geometry.Geometry, minZ, maxZ *float64, hasAny *bool) {
	visit := func(pts []geometry.Point) {
		for _, p := range pts {
			*hasAny = true
			if p.Z < *minZ {
				*minZ = p.Z
			}
			if p.Z > *maxZ {
				*maxZ = p.Z
			}
		}
	}
	switch t := g.(type) {
	case geometry.Point:
		*hasAny = true
		if t.Z < *minZ {
			*minZ = t.Z
		}
		if t.Z > *maxZ {
			*maxZ = t.Z
		}
	case geometry.MultiPoint:
		visit(t.Points)
	case geometry.LineString:
		visit(t.Points)
	case geometry.MultiLineString:
		for _, l := range t.Lines {
			visit(l.Points)
		}
	case geometry.Polygon:
		for _, r := range t.Rings {
			visit(r)
		}
	case geometry.MultiPolygon:
		for _, p := range t.Polygons {
			for _, r := range p.Rings {
				visit(r)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// DBF writer
// -----------------------------------------------------------------------------

// buildDBF serializes every non-geometry column of f into a dBase III
// blob. String columns become 'C' fields, numeric columns become 'N',
// boolean columns 'L'. Field names are truncated to 10 ASCII bytes as
// required by dBase III.
func buildDBF(f *gobi.Frame, geomColName string) ([]byte, error) {
	type dbfField struct {
		name    string
		typ     byte
		length  int
		decimal int
		series  gobi.Series
	}
	var fields []dbfField
	for _, name := range f.ColumnNames() {
		if name == geomColName {
			continue
		}
		s, err := f.Column(name)
		if err != nil {
			return nil, err
		}
		fd := dbfField{name: dbfFieldName(name), series: s}
		switch s.DataType().ID() {
		case arrow.STRING:
			fd.typ = 'C'
			fd.length = 80 // reasonable default; dBase supports up to 254
		case arrow.INT64, arrow.INT32:
			fd.typ = 'N'
			fd.length = 18
			fd.decimal = 0
		case arrow.FLOAT64, arrow.FLOAT32:
			fd.typ = 'N'
			fd.length = 19
			fd.decimal = 6
		case arrow.BOOL:
			fd.typ = 'L'
			fd.length = 1
		default:
			// Fallback: format the cell as a string and store as 'C'.
			fd.typ = 'C'
			fd.length = 80
		}
		fields = append(fields, fd)
	}

	n := f.NumRows()
	recordSize := 1 // leading deletion flag
	for _, fd := range fields {
		recordSize += fd.length
	}
	headerSize := 32 + 32*len(fields) + 1 // +1 for the 0x0D terminator

	buf := new(bytes.Buffer)
	// -- Header --
	buf.WriteByte(0x03)                       // dBase III without memo
	buf.WriteByte(byte((2026 - 1900) & 0xff)) // year offset (2026 → 126)
	buf.WriteByte(1)                          // month
	buf.WriteByte(1)                          // day
	binary.Write(buf, binary.LittleEndian, uint32(n))
	binary.Write(buf, binary.LittleEndian, uint16(headerSize))
	binary.Write(buf, binary.LittleEndian, uint16(recordSize))
	// Reserved bytes.
	buf.Write(make([]byte, 20))

	// -- Field descriptors --
	for _, fd := range fields {
		name := fd.name
		nameBytes := make([]byte, 11)
		copy(nameBytes, name)
		buf.Write(nameBytes)
		buf.WriteByte(fd.typ)
		binary.Write(buf, binary.LittleEndian, uint32(0)) // field data address (unused)
		buf.WriteByte(byte(fd.length))
		buf.WriteByte(byte(fd.decimal))
		buf.Write(make([]byte, 14)) // reserved
	}
	buf.WriteByte(0x0D) // terminator

	// -- Records --
	for row := range n {
		buf.WriteByte(' ') // not deleted
		for _, fd := range fields {
			cell := formatDBFCell(fd.series, row, fd.decimal)
			// Pad / truncate to field length exactly.
			if len(cell) > fd.length {
				cell = cell[:fd.length]
			} else {
				cell = cell + strings.Repeat(" ", fd.length-len(cell))
			}
			buf.WriteString(cell)
		}
	}
	// -- EOF marker --
	buf.WriteByte(0x1A)
	return buf.Bytes(), nil
}

// dbfFieldName sanitizes a column name for dBase III: uppercase ASCII, up
// to 10 bytes, non-alphanumerics replaced with '_'.
func dbfFieldName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z',
			r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 10 {
			break
		}
	}
	out := strings.ToUpper(b.String())
	if out == "" {
		out = "COL"
	}
	return out
}

// formatDBFCell converts one cell to its DBF-serialized ASCII form (not
// yet padded to field length; the caller pads).
func formatDBFCell(s gobi.Series, i int, decimal int) string {
	col := s.Column()
	if col == nil {
		return ""
	}
	offset := 0
	for _, chunk := range col.Data().Chunks() {
		if i < offset+chunk.Len() {
			local := i - offset
			if chunk.IsNull(local) {
				return ""
			}
			return chunkCellToDBF(chunk, local, decimal)
		}
		offset += chunk.Len()
	}
	return ""
}

func chunkCellToDBF(chunk arrow.Array, local int, decimal int) string {
	switch a := chunk.(type) {
	case *array.String:
		return a.Value(local)
	case *array.Int64:
		return strconv.FormatInt(a.Value(local), 10)
	case *array.Int32:
		return strconv.FormatInt(int64(a.Value(local)), 10)
	case *array.Float64:
		return strconv.FormatFloat(a.Value(local), 'f', decimal, 64)
	case *array.Float32:
		return strconv.FormatFloat(float64(a.Value(local)), 'f', decimal, 32)
	case *array.Boolean:
		if a.Value(local) {
			return "T"
		}
		return "F"
	}
	return ""
}

// -----------------------------------------------------------------------------
// PRJ helper
// -----------------------------------------------------------------------------

// prjTextForCol returns a minimal WKT string for the geometry column's
// CRS if the EPSG code is known and common. Returns "" (skip PRJ) for
// anything else — better to omit than write a wrong WKT.
func prjTextForCol(col gobi.Series) string {
	if !col.IsGeometry() {
		return ""
	}
	epsg := geometryCRSFromField(col)
	switch epsg {
	case 4326:
		return `GEOGCS["WGS 84",DATUM["WGS_1984",SPHEROID["WGS 84",6378137,298.257223563]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433]]`
	case 3857:
		return `PROJCS["WGS 84 / Pseudo-Mercator",GEOGCS["WGS 84",DATUM["WGS_1984",SPHEROID["WGS 84",6378137,298.257223563]],PRIMEM["Greenwich",0],UNIT["degree",0.0174532925199433]],PROJECTION["Mercator_1SP"],PARAMETER["central_meridian",0],PARAMETER["scale_factor",1],PARAMETER["false_easting",0],PARAMETER["false_northing",0],UNIT["metre",1]]`
	}
	return ""
}

// geometryCRSFromField extracts the EPSG code from a Series' geometry
// column field metadata. Duplicates the private helper in gobi.
func geometryCRSFromField(col gobi.Series) int32 {
	// Unfortunately the field's Metadata methods aren't exposed via
	// Series; but the tag on the arrow.Field is public.
	f := col.Column().Field()
	if !f.HasMetadata() {
		return 0
	}
	v, ok := f.Metadata.GetValue(gobi.MetaGeometryCRS)
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

// Package kmlio reads and writes KML (Keyhole Markup Language,
// OGC 12-007r2) and KMZ (zipped KML) as gobi Frames.
//
// KML always stores coordinates in WGS 84 (lon, lat[, alt]) per the
// OGC spec, so output frames have their geometry column tagged with
// EPSG:4326.
//
// The reader flattens every <Placemark> in the document. Each
// placemark contributes one row with columns:
//
//   - name         (string, from <name>)
//   - description  (string, from <description>)
//   - geometry     (WKB Binary, tagged EPSG:4326)
//   - <ext-key>    one string column per distinct <ExtendedData><Data>
//     name= attribute seen in the document
//
// The writer emits one <Placemark> per row. String / numeric columns
// other than "name", "description", and the geometry column become
// <ExtendedData> entries.
//
// KMZ support. KMZ is a zip archive containing one primary .kml
// file (by convention "doc.kml") plus optional resources. ReadFile
// and WriteFile auto-detect the format from the file extension
// (.kmz → KMZ). For Reader/Writer flows without a filename hint,
// set ReadOptions.Format / WriteOptions.Format to FormatKMZ. The
// reader prefers "doc.kml" but falls back to the first .kml entry
// in the archive so KMZ files produced by other tools (with names
// like "layer.kml") still parse.
//
// Not implemented (out of scope):
//
//   - <Style>, <StyleMap>, <NetworkLink>, <Document> nesting semantics
//   - Time / TimeSpan primitives
//   - KMZ resource files (icons, images) — only the primary .kml
//     entry is read; other entries in the archive are ignored
//   - Non-primitive KML geometries (Model, gx:Track)
package kmlio

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// ErrInvalidKML is returned when the input isn't a well-formed KML or uses
// a construct this package doesn't support.
var ErrInvalidKML = errors.New("kmlio: invalid input")

// ReadOptions controls the KML/KMZ reader.
type ReadOptions struct {
	// Format selects the on-disk shape. FormatAuto (the default)
	// picks by file extension (.kmz → KMZ, otherwise KML) in
	// ReadFile. For Read (io.Reader-based), FormatAuto behaves as
	// FormatKML — a raw Reader has no filename hint. Set Format
	// explicitly when reading KMZ from a Reader.
	Format Format
}

// WriteOptions controls the KML/KMZ writer.
type WriteOptions struct {
	// Format selects the on-disk shape. Same auto-detect rules as
	// ReadOptions.Format — .kmz extension → KMZ, otherwise KML.
	Format Format
}

// Format identifies the KML container flavor: raw XML or a
// zip-packaged .kmz. KMZ is a zip archive containing a single
// primary .kml file (by convention "doc.kml") plus optional
// resources (icons, images). Gobi's writer emits just the
// "doc.kml" entry; the reader accepts any archive containing at
// least one .kml file.
type Format uint8

const (
	// FormatAuto picks by file extension (.kmz → KMZ, otherwise
	// KML). Only meaningful for ReadFile / WriteFile; io.Reader /
	// io.Writer paths treat it as FormatKML.
	FormatAuto Format = iota
	// FormatKML is uncompressed XML — the canonical KML document.
	FormatKML
	// FormatKMZ is a zip archive containing a doc.kml entry.
	FormatKMZ
)

// -----------------------------------------------------------------------------
// XML structs (subset of KML 2.3 we actually decode)
// -----------------------------------------------------------------------------

type kmlPlacemark struct {
	Name         string           `xml:"name"`
	Description  string           `xml:"description"`
	Point        *kmlPoint        `xml:"Point"`
	LineString   *kmlLineString   `xml:"LineString"`
	Polygon      *kmlPolygon      `xml:"Polygon"`
	MultiGeom    *kmlMultiGeom    `xml:"MultiGeometry"`
	ExtendedData *kmlExtendedData `xml:"ExtendedData"`
}

type kmlPoint struct {
	Coordinates string `xml:"coordinates"`
}

type kmlLineString struct {
	Coordinates string `xml:"coordinates"`
}

type kmlPolygon struct {
	Outer struct {
		LinearRing struct {
			Coordinates string `xml:"coordinates"`
		} `xml:"LinearRing"`
	} `xml:"outerBoundaryIs"`
	Inner []struct {
		LinearRing struct {
			Coordinates string `xml:"coordinates"`
		} `xml:"LinearRing"`
	} `xml:"innerBoundaryIs"`
}

type kmlMultiGeom struct {
	Points      []kmlPoint      `xml:"Point"`
	LineStrings []kmlLineString `xml:"LineString"`
	Polygons    []kmlPolygon    `xml:"Polygon"`
}

type kmlExtendedData struct {
	Data []kmlData `xml:"Data"`
}

type kmlData struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value"`
}

type kmlContainer struct {
	Placemarks []kmlPlacemark `xml:"Placemark"`
	Folders    []kmlContainer `xml:"Folder"`
}

type kmlRoot struct {
	XMLName  xml.Name      `xml:"kml"`
	Document *kmlContainer `xml:"Document"`
	// Some producers put Placemarks directly under <kml> without a
	// <Document> wrapper; capture those too.
	Placemarks []kmlPlacemark `xml:"Placemark"`
	Folders    []kmlContainer `xml:"Folder"`
}

// collectPlacemarks walks any nesting of <Document>/<Folder> and returns
// the flat list of Placemarks.
func (r *kmlRoot) collectPlacemarks() []kmlPlacemark {
	out := append([]kmlPlacemark(nil), r.Placemarks...)
	if r.Document != nil {
		out = append(out, collectFromContainer(*r.Document)...)
	}
	for _, f := range r.Folders {
		out = append(out, collectFromContainer(f)...)
	}
	return out
}

func collectFromContainer(c kmlContainer) []kmlPlacemark {
	out := append([]kmlPlacemark(nil), c.Placemarks...)
	for _, f := range c.Folders {
		out = append(out, collectFromContainer(f)...)
	}
	return out
}

// -----------------------------------------------------------------------------
// Reader
// -----------------------------------------------------------------------------

// ReadFile parses path and returns a Frame. See package doc for the
// column shape. Pass nil opts for defaults. File extension picks
// between KML and KMZ (.kmz → KMZ) when opts.Format is FormatAuto.
func ReadFile(path string, opts *ReadOptions) (*gobi.Frame, error) {
	format := resolveReadFormat(path, opts)
	if format == FormatKMZ {
		return readKMZFile(path, opts)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readXML(f, opts)
}

// Read parses a KML document from r into a Frame. Pass nil opts
// for defaults.
//
// For a KMZ archive from a Reader, set opts.Format = FormatKMZ.
// The reader will buffer the input to a bytes.Buffer (archive/zip
// needs io.ReaderAt with a known size) and locate the primary
// .kml entry inside the archive. FormatAuto on a Reader defaults
// to FormatKML — a raw Reader carries no filename hint.
func Read(r io.Reader, opts *ReadOptions) (*gobi.Frame, error) {
	if opts != nil && opts.Format == FormatKMZ {
		return readKMZReader(r, opts)
	}
	return readXML(r, opts)
}

// readXML is the raw-KML XML decoder — shared by the KML path and
// (after unzipping) the KMZ path.
func readXML(r io.Reader, opts *ReadOptions) (*gobi.Frame, error) {
	_ = opts // reserved
	var root kmlRoot
	dec := xml.NewDecoder(r)
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKML, err)
	}
	placemarks := root.collectPlacemarks()

	// Pass 1: discover the union of ExtendedData keys.
	extKeys := map[string]struct{}{}
	for _, p := range placemarks {
		if p.ExtendedData == nil {
			continue
		}
		for _, d := range p.ExtendedData.Data {
			if d.Name != "" {
				extKeys[d.Name] = struct{}{}
			}
		}
	}
	sortedExt := make([]string, 0, len(extKeys))
	for k := range extKeys {
		sortedExt = append(sortedExt, k)
	}
	sort.Strings(sortedExt)

	// Pass 2: build the columns.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	descB := array.NewStringBuilder(pool)
	defer descB.Release()
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	extBs := make([]*array.StringBuilder, len(sortedExt))
	for i := range extBs {
		extBs[i] = array.NewStringBuilder(pool)
	}
	defer func() {
		for _, b := range extBs {
			b.Release()
		}
	}()

	for _, p := range placemarks {
		nameB.Append(p.Name)
		descB.Append(p.Description)
		g, err := placemarkGeometry(p)
		if err != nil {
			return nil, err
		}
		if g == nil {
			geomB.AppendNull()
		} else {
			geomB.Append(geometry.WKB(g))
		}
		// ExtendedData columns.
		var extMap map[string]string
		if p.ExtendedData != nil {
			extMap = make(map[string]string, len(p.ExtendedData.Data))
			for _, d := range p.ExtendedData.Data {
				extMap[d.Name] = d.Value
			}
		}
		for i, k := range sortedExt {
			if v, ok := extMap[k]; ok {
				extBs[i].Append(v)
			} else {
				extBs[i].AppendNull()
			}
		}
	}

	// Assemble the Frame.
	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "description", Type: arrow.BinaryTypes.String, Nullable: true},
		gobi.GeometryField("geometry", 4326),
	}
	arrs := []arrow.Array{nameB.NewArray(), descB.NewArray(), geomB.NewArray()}
	for i, k := range sortedExt {
		fields = append(fields, arrow.Field{Name: k, Type: arrow.BinaryTypes.String, Nullable: true})
		arrs = append(arrs, extBs[i].NewArray())
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	schema := arrow.NewSchema(fields, nil)
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	return gobi.NewFrame(schema, cols)
}

// placemarkGeometry converts a Placemark's KML geometry (any one of Point,
// LineString, Polygon, MultiGeometry) into a gobi Geometry. Nil is returned
// when the Placemark has no geometry set.
func placemarkGeometry(p kmlPlacemark) (geometry.Geometry, error) {
	switch {
	case p.Point != nil:
		pts, err := parseCoordinates(p.Point.Coordinates)
		if err != nil {
			return nil, err
		}
		if len(pts) == 0 {
			return nil, fmt.Errorf("%w: empty Point coordinates", ErrInvalidKML)
		}
		pts[0].CRSValue = geometry.WGS84
		return pts[0], nil
	case p.LineString != nil:
		pts, err := parseCoordinates(p.LineString.Coordinates)
		if err != nil {
			return nil, err
		}
		return geometry.LineString{Points: pts, CRSValue: geometry.WGS84}, nil
	case p.Polygon != nil:
		outer, err := parseCoordinates(p.Polygon.Outer.LinearRing.Coordinates)
		if err != nil {
			return nil, err
		}
		rings := [][]geometry.Point{outer}
		for _, inner := range p.Polygon.Inner {
			ring, err := parseCoordinates(inner.LinearRing.Coordinates)
			if err != nil {
				return nil, err
			}
			rings = append(rings, ring)
		}
		return geometry.Polygon{Rings: rings, CRSValue: geometry.WGS84}, nil
	case p.MultiGeom != nil:
		return multiGeomToGeometry(*p.MultiGeom)
	}
	return nil, nil
}

// multiGeomToGeometry chooses the most specific gobi type based on what
// the MultiGeometry contains. Homogeneous collections become MultiPoint /
// MultiLineString / MultiPolygon; heterogeneous ones become a
// GeometryCollection.
func multiGeomToGeometry(m kmlMultiGeom) (geometry.Geometry, error) {
	nP, nL, nG := len(m.Points), len(m.LineStrings), len(m.Polygons)
	switch {
	case nP > 0 && nL == 0 && nG == 0:
		pts := make([]geometry.Point, 0, nP)
		for _, kp := range m.Points {
			p, err := parseCoordinates(kp.Coordinates)
			if err != nil {
				return nil, err
			}
			if len(p) > 0 {
				pts = append(pts, p[0])
			}
		}
		return geometry.MultiPoint{Points: pts, CRSValue: geometry.WGS84}, nil
	case nL > 0 && nP == 0 && nG == 0:
		lines := make([]geometry.LineString, 0, nL)
		for _, kl := range m.LineStrings {
			p, err := parseCoordinates(kl.Coordinates)
			if err != nil {
				return nil, err
			}
			lines = append(lines, geometry.LineString{Points: p, CRSValue: geometry.WGS84})
		}
		return geometry.MultiLineString{Lines: lines, CRSValue: geometry.WGS84}, nil
	case nG > 0 && nP == 0 && nL == 0:
		polys := make([]geometry.Polygon, 0, nG)
		for _, kp := range m.Polygons {
			outer, err := parseCoordinates(kp.Outer.LinearRing.Coordinates)
			if err != nil {
				return nil, err
			}
			rings := [][]geometry.Point{outer}
			for _, inner := range kp.Inner {
				r, err := parseCoordinates(inner.LinearRing.Coordinates)
				if err != nil {
					return nil, err
				}
				rings = append(rings, r)
			}
			polys = append(polys, geometry.Polygon{Rings: rings, CRSValue: geometry.WGS84})
		}
		return geometry.MultiPolygon{Polygons: polys, CRSValue: geometry.WGS84}, nil
	}
	// Heterogeneous collection.
	var members []geometry.Geometry
	for _, kp := range m.Points {
		p, err := parseCoordinates(kp.Coordinates)
		if err != nil {
			return nil, err
		}
		if len(p) > 0 {
			members = append(members, geometry.Point{X: p[0].X, Y: p[0].Y, CRSValue: geometry.WGS84})
		}
	}
	for _, kl := range m.LineStrings {
		p, err := parseCoordinates(kl.Coordinates)
		if err != nil {
			return nil, err
		}
		members = append(members, geometry.LineString{Points: p, CRSValue: geometry.WGS84})
	}
	for _, kp := range m.Polygons {
		outer, err := parseCoordinates(kp.Outer.LinearRing.Coordinates)
		if err != nil {
			return nil, err
		}
		rings := [][]geometry.Point{outer}
		for _, inner := range kp.Inner {
			r, err := parseCoordinates(inner.LinearRing.Coordinates)
			if err != nil {
				return nil, err
			}
			rings = append(rings, r)
		}
		members = append(members, geometry.Polygon{Rings: rings, CRSValue: geometry.WGS84})
	}
	return geometry.GeometryCollection{Geometries: members, CRSValue: geometry.WGS84}, nil
}

// parseCoordinates parses a KML coordinate list. The KML spec says tuples
// are separated by whitespace and components within a tuple are
// comma-separated: "lon,lat[,alt] lon,lat[,alt] …". Whitespace between the
// tuples is any run of spaces, tabs, or newlines.
func parseCoordinates(s string) ([]geometry.Point, error) {
	tuples := strings.Fields(s)
	pts := make([]geometry.Point, 0, len(tuples))
	for _, tup := range tuples {
		parts := strings.Split(tup, ",")
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: coordinate %q has fewer than 2 components", ErrInvalidKML, tup)
		}
		lon, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidKML, err)
		}
		lat, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidKML, err)
		}
		p := geometry.Point{X: lon, Y: lat, CRSValue: geometry.WGS84}
		if len(parts) >= 3 && strings.TrimSpace(parts[2]) != "" {
			z, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
			if err == nil {
				p.Z = z
				p.HasZ = true
			}
		}
		pts = append(pts, p)
	}
	return pts, nil
}

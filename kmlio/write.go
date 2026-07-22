package kmlio

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// WriteFile writes f to path as KML. See package doc for the column
// conventions the writer looks for. Pass nil opts for defaults.
func WriteFile(f *gobi.Frame, path string, opts *WriteOptions) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	return Write(f, out, opts)
}

// Write encodes f to w as a KML document (namespace 2.2). Pass nil
// opts for defaults.
func Write(f *gobi.Frame, w io.Writer, opts *WriteOptions) error {
	_ = opts // reserved
	geomCol, geomName, err := findGeometryColumn(f)
	if err != nil {
		return err
	}
	nameCol, _ := f.Column("name")
	descCol, _ := f.Column("description")

	// Every non-geometry, non-name, non-description column becomes an
	// ExtendedData entry. Preserve column order for reproducibility.
	extNames := make([]string, 0, f.NumCols())
	extCols := make([]gobi.Series, 0, f.NumCols())
	for _, colName := range f.ColumnNames() {
		if colName == geomName || colName == "name" || colName == "description" {
			continue
		}
		s, err := f.Column(colName)
		if err != nil {
			return err
		}
		extNames = append(extNames, colName)
		extCols = append(extCols, s)
	}

	if _, err := io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `<kml xmlns="http://www.opengis.net/kml/2.2"><Document>`+"\n"); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("  ", "  ")

	n := f.NumRows()
	for row := range n {
		if err := writePlacemark(enc, row, geomCol, nameCol, descCol, extNames, extCols); err != nil {
			return err
		}
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n</Document></kml>\n")
	return err
}

// findGeometryColumn returns the frame's tagged geometry column and its
// name. Errors if the frame has no geometry column.
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
	return gobi.Series{}, "", fmt.Errorf("kmlio: frame has no geometry column")
}

func writePlacemark(
	enc *xml.Encoder,
	row int,
	geomCol, nameCol, descCol gobi.Series,
	extNames []string,
	extCols []gobi.Series,
) error {
	pm := xml.StartElement{Name: xml.Name{Local: "Placemark"}}
	if err := enc.EncodeToken(pm); err != nil {
		return err
	}

	if nameCol.Len() > row {
		if v, ok, _ := stringAt(nameCol, row); ok && v != "" {
			if err := enc.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "name"}}); err != nil {
				return err
			}
		}
	}
	if descCol.Len() > row {
		if v, ok, _ := stringAt(descCol, row); ok && v != "" {
			if err := enc.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "description"}}); err != nil {
				return err
			}
		}
	}

	// ExtendedData block.
	if len(extNames) > 0 {
		anyValue := false
		for _, s := range extCols {
			if _, ok, _ := stringAt(s, row); ok {
				anyValue = true
				break
			}
		}
		if anyValue {
			ed := xml.StartElement{Name: xml.Name{Local: "ExtendedData"}}
			if err := enc.EncodeToken(ed); err != nil {
				return err
			}
			for i, s := range extCols {
				v, ok, _ := stringAt(s, row)
				if !ok {
					continue
				}
				data := xml.StartElement{
					Name: xml.Name{Local: "Data"},
					Attr: []xml.Attr{{Name: xml.Name{Local: "name"}, Value: extNames[i]}},
				}
				if err := enc.EncodeToken(data); err != nil {
					return err
				}
				if err := enc.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "value"}}); err != nil {
					return err
				}
				if err := enc.EncodeToken(data.End()); err != nil {
					return err
				}
			}
			if err := enc.EncodeToken(ed.End()); err != nil {
				return err
			}
		}
	}

	// Geometry.
	g, err := geomCol.Geometry(row)
	if err != nil {
		return err
	}
	if g != nil {
		if err := encodeGeometry(enc, g); err != nil {
			return err
		}
	}

	if err := enc.EncodeToken(pm.End()); err != nil {
		return err
	}
	return enc.Flush()
}

// encodeGeometry emits the KML XML for a gobi geometry.
func encodeGeometry(enc *xml.Encoder, g geometry.Geometry) error {
	switch t := g.(type) {
	case geometry.Point:
		return encodeKMLPoint(enc, t)
	case geometry.LineString:
		return encodeKMLLineString(enc, t)
	case geometry.Polygon:
		return encodeKMLPolygon(enc, t)
	case geometry.MultiPoint:
		return encodeMulti(enc, func() error {
			for _, p := range t.Points {
				if err := encodeKMLPoint(enc, p); err != nil {
					return err
				}
			}
			return nil
		})
	case geometry.MultiLineString:
		return encodeMulti(enc, func() error {
			for _, l := range t.Lines {
				if err := encodeKMLLineString(enc, l); err != nil {
					return err
				}
			}
			return nil
		})
	case geometry.MultiPolygon:
		return encodeMulti(enc, func() error {
			for _, p := range t.Polygons {
				if err := encodeKMLPolygon(enc, p); err != nil {
					return err
				}
			}
			return nil
		})
	case geometry.GeometryCollection:
		return encodeMulti(enc, func() error {
			for _, inner := range t.Geometries {
				if err := encodeGeometry(enc, inner); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return fmt.Errorf("kmlio: unsupported geometry %T", g)
}

func encodeKMLPoint(enc *xml.Encoder, p geometry.Point) error {
	start := xml.StartElement{Name: xml.Name{Local: "Point"}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if err := enc.EncodeElement(coordString(p), xml.StartElement{Name: xml.Name{Local: "coordinates"}}); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

func encodeKMLLineString(enc *xml.Encoder, l geometry.LineString) error {
	start := xml.StartElement{Name: xml.Name{Local: "LineString"}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if err := enc.EncodeElement(coordsString(l.Points), xml.StartElement{Name: xml.Name{Local: "coordinates"}}); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

func encodeKMLPolygon(enc *xml.Encoder, p geometry.Polygon) error {
	start := xml.StartElement{Name: xml.Name{Local: "Polygon"}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if len(p.Rings) > 0 {
		if err := encodeBoundary(enc, "outerBoundaryIs", p.Rings[0]); err != nil {
			return err
		}
		for _, hole := range p.Rings[1:] {
			if err := encodeBoundary(enc, "innerBoundaryIs", hole); err != nil {
				return err
			}
		}
	}
	return enc.EncodeToken(start.End())
}

func encodeBoundary(enc *xml.Encoder, tag string, ring []geometry.Point) error {
	outer := xml.StartElement{Name: xml.Name{Local: tag}}
	if err := enc.EncodeToken(outer); err != nil {
		return err
	}
	lr := xml.StartElement{Name: xml.Name{Local: "LinearRing"}}
	if err := enc.EncodeToken(lr); err != nil {
		return err
	}
	if err := enc.EncodeElement(coordsString(ring), xml.StartElement{Name: xml.Name{Local: "coordinates"}}); err != nil {
		return err
	}
	if err := enc.EncodeToken(lr.End()); err != nil {
		return err
	}
	return enc.EncodeToken(outer.End())
}

func encodeMulti(enc *xml.Encoder, body func() error) error {
	start := xml.StartElement{Name: xml.Name{Local: "MultiGeometry"}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if err := body(); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

func coordString(p geometry.Point) string {
	if p.HasZ {
		return fmt.Sprintf("%s,%s,%s", ff(p.X), ff(p.Y), ff(p.Z))
	}
	return fmt.Sprintf("%s,%s", ff(p.X), ff(p.Y))
}

func coordsString(pts []geometry.Point) string {
	var b strings.Builder
	for i, p := range pts {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(coordString(p))
	}
	return b.String()
}

// ff formats a float without trailing zeros, matching typical KML output.
func ff(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// stringAt extracts row i of s as a string. String columns are returned
// verbatim; numeric / bool columns are formatted via strconv. Returns
// ok=false for null rows or unsupported types.
func stringAt(s gobi.Series, i int) (string, bool, error) {
	if s.Len() == 0 || i >= s.Len() {
		return "", false, nil
	}
	col := s.Column()
	if col == nil {
		return "", false, nil
	}
	offset := 0
	for _, chunk := range col.Data().Chunks() {
		if i < offset+chunk.Len() {
			local := i - offset
			if chunk.IsNull(local) {
				return "", false, nil
			}
			switch a := chunk.(type) {
			case *array.String:
				return a.Value(local), true, nil
			case *array.Int64:
				return strconv.FormatInt(a.Value(local), 10), true, nil
			case *array.Int32:
				return strconv.FormatInt(int64(a.Value(local)), 10), true, nil
			case *array.Float64:
				return ff(a.Value(local)), true, nil
			case *array.Float32:
				return ff(float64(a.Value(local))), true, nil
			case *array.Boolean:
				return strconv.FormatBool(a.Value(local)), true, nil
			}
			return "", false, nil
		}
		offset += chunk.Len()
	}
	return "", false, nil
}

// avoid an unused-import lint if the file gets trimmed later.
var _ = arrow.BinaryTypes

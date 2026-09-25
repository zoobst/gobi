package parquetio

import (
	"fmt"
	"math"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// Covering names existing columns that already hold a geometry
// column's per-row bounding box, for WriteOptions.Coverings. The file's
// GeoParquet covering.bbox points at them and no <geom>_bbox_* columns
// are generated for that geometry column. Readers (gobi included) prune
// row groups from these columns' min / max statistics.
type Covering struct {
	Xmin, Ymin, Xmax, Ymax string
}

// PointCovering is the covering for a point geometry column whose
// coordinates are also stored as x / y columns (e.g. lon / lat): a
// point's bbox is (x, y, x, y), so the x and y columns' row-group
// min / max already are its covering.
func PointCovering(x, y string) Covering {
	return Covering{Xmin: x, Ymin: y, Xmax: x, Ymax: y}
}

func (c Covering) columns() []string { return []string{c.Xmin, c.Ymin, c.Xmax, c.Ymax} }

// arrowSchemaKey is the footer key pqarrow writes the stored Arrow
// schema under; WriteOptions.KeyValueMetadata can't set it. ("geo" is
// likewise gobi's whenever the frame has a geometry column.)
const arrowSchemaKey = "ARROW:schema"

// validateGeoOptions checks Coverings and KeyValueMetadata against the
// schema of the frames to be written. hasGeometry reports whether gobi
// will write its own "geo" footer entry.
func validateGeoOptions(schema *arrow.Schema, opts *WriteOptions) error {
	hasGeometry := false
	for _, f := range schema.Fields() {
		if isGeometryField(f) {
			hasGeometry = true
		}
	}
	for geom, cov := range opts.Coverings {
		idx := schema.FieldIndices(geom)
		if len(idx) == 0 {
			return fmt.Errorf("parquetio: Coverings[%q]: no such column", geom)
		}
		if !isGeometryField(schema.Field(idx[0])) {
			return fmt.Errorf("parquetio: Coverings[%q]: not a geometry column", geom)
		}
		for _, c := range cov.columns() {
			ci := schema.FieldIndices(c)
			if len(ci) == 0 {
				return fmt.Errorf("parquetio: Coverings[%q]: covering column %q not in the frame", geom, c)
			}
			switch schema.Field(ci[0]).Type.ID() {
			case arrow.FLOAT64, arrow.FLOAT32, arrow.INT64, arrow.INT32:
			default:
				return fmt.Errorf("parquetio: Coverings[%q]: covering column %q is %s, want a numeric column",
					geom, c, schema.Field(ci[0]).Type)
			}
		}
	}
	for k := range opts.KeyValueMetadata {
		switch {
		case k == arrowSchemaKey:
			return fmt.Errorf("parquetio: KeyValueMetadata[%q] is written by the Arrow writer and can't be set", k)
		case k == gobi.GeoParquetMetadataKey && hasGeometry:
			return fmt.Errorf("parquetio: KeyValueMetadata[%q]: gobi writes the GeoParquet entry for frames with a geometry column; use Coverings to point the covering at existing columns", k)
		}
	}
	return nil
}

// isGeometryField mirrors gobi's geometry tag check for a bare field:
// a Binary column carrying the geometry-type metadata key.
func isGeometryField(f arrow.Field) bool {
	if f.Type.ID() != arrow.BINARY {
		return false
	}
	_, ok := f.Metadata.GetValue(gobi.MetaGeometryType)
	return ok
}

// prepareGeo augments f for writing: generated bbox covering columns
// for geometry columns without a declared Covering (unless
// SkipBboxCovering), and the GeoParquet metadata with every covering
// filled in. Declared coverings are checked row by row against the
// geometries. Returns f itself (retained) when nothing is added; the
// caller releases the result. meta is nil when f has no geometry.
func prepareGeo(f *gobi.Frame, opts *WriteOptions) (*gobi.Frame, *gobi.GeoParquetMetadata, error) {
	var generate []string
	for _, name := range f.ColumnNames() {
		s, _ := f.Column(name)
		if !s.IsGeometry() {
			continue
		}
		if _, declared := opts.Coverings[name]; !declared && !opts.SkipBboxCovering {
			generate = append(generate, name)
		}
	}

	var (
		aug  *gobi.Frame
		meta *gobi.GeoParquetMetadata
		err  error
	)
	if len(generate) > 0 {
		aug, meta, err = gobi.WithBboxCoveringColumnsFor(f, opts.Allocator, generate...)
		if err != nil {
			return nil, nil, err
		}
	} else {
		if meta, err = gobi.BuildGeoParquetMetadata(f); err != nil {
			return nil, nil, err
		}
		f.Retain()
		aug = f
	}

	// Declared coverings, in a stable order for error messages.
	names := make([]string, 0, len(opts.Coverings))
	for name := range opts.Coverings {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		cov := opts.Coverings[name]
		if err := checkCovering(f, name, cov); err != nil {
			aug.Release()
			return nil, nil, err
		}
		cm := meta.Columns[name]
		cm.Covering = &gobi.GeoParquetCovering{Bbox: &gobi.GeoParquetBboxCovering{
			Xmin: []string{cov.Xmin}, Ymin: []string{cov.Ymin},
			Xmax: []string{cov.Xmax}, Ymax: []string{cov.Ymax},
		}}
		meta.Columns[name] = cm
	}
	return aug, meta, nil
}

// checkCovering verifies that every non-null, non-empty geometry in
// geom lies inside its row's declared covering. A covering that
// doesn't contain its geometry would let readers prune row groups that
// hold matching rows, silently dropping them, so this is an error, not
// a warning. Costs one WKB bounds scan per declared column.
func checkCovering(f *gobi.Frame, geom string, cov Covering) error {
	g, err := f.Column(geom)
	if err != nil {
		return err
	}
	var cols [4]*seqFloats
	for i, c := range cov.columns() {
		s, err := f.Column(c)
		if err != nil {
			return err
		}
		cols[i] = newSeqFloats(s.Column().Data().Chunks())
	}
	row := 0
	for _, chunk := range g.Column().Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return fmt.Errorf("parquetio: geometry column %q is %T, want Binary", geom, chunk)
		}
		for i := range bin.Len() {
			var v [4]float64
			var valid [4]bool
			for k := range cols {
				v[k], valid[k] = cols[k].next()
			}
			if bin.IsNull(i) {
				row++
				continue
			}
			b, err := geometry.BoundsFromWKB(bin.Value(i))
			if err != nil {
				return fmt.Errorf("parquetio: %q row %d: %w", geom, row, err)
			}
			if b.Empty() {
				row++
				continue
			}
			ok := valid[0] && valid[1] && valid[2] && valid[3] &&
				v[0] <= b.MinX && v[1] <= b.MinY && v[2] >= b.MaxX && v[3] >= b.MaxY
			if !ok {
				return fmt.Errorf("parquetio: Coverings[%q]: row %d geometry bbox [%g %g %g %g] is not inside covering (%s, %s, %s, %s) = [%s]",
					geom, row, b.MinX, b.MinY, b.MaxX, b.MaxY,
					cov.Xmin, cov.Ymin, cov.Xmax, cov.Ymax, fmtCovering(v, valid))
			}
			row++
		}
	}
	return nil
}

func fmtCovering(v [4]float64, valid [4]bool) string {
	out := ""
	for i := range v {
		if i > 0 {
			out += " "
		}
		if valid[i] {
			out += fmt.Sprintf("%g", v[i])
		} else {
			out += "null"
		}
	}
	return out
}

// seqFloats reads a numeric column's values in row order as float64,
// walking its chunks without per-row lookups.
type seqFloats struct {
	chunks []arrow.Array
	ci, i  int
}

func newSeqFloats(chunks []arrow.Array) *seqFloats { return &seqFloats{chunks: chunks} }

// next returns the next row's value; ok=false for null (or NaN).
func (s *seqFloats) next() (float64, bool) {
	for s.ci < len(s.chunks) && s.i >= s.chunks[s.ci].Len() {
		s.ci++
		s.i = 0
	}
	if s.ci >= len(s.chunks) {
		return 0, false
	}
	arr, i := s.chunks[s.ci], s.i
	s.i++
	if arr.IsNull(i) {
		return 0, false
	}
	var v float64
	switch a := arr.(type) {
	case *array.Float64:
		v = a.Value(i)
	case *array.Float32:
		v = float64(a.Value(i))
	case *array.Int64:
		v = float64(a.Value(i))
	case *array.Int32:
		v = float64(a.Value(i))
	default:
		return 0, false
	}
	return v, !math.IsNaN(v)
}

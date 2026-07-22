package geojsonio

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/zoobst/gobi"
)

// ScanFile returns a LazyFrame that reads path as GeoJSON when
// Collect is called.
//
// Optimizer participation:
//
//   - Streaming: reads one batch at a time through
//     ReadFileChunksFunc. Peak memory bounded to one batch.
//   - Projection: Frame.Select() above the scan drops unpicked
//     property columns at the LazyFrame layer. Parse work isn't
//     reduced — a streaming JSON decoder can't skip fields — but
//     the emitted Frame only carries the columns downstream ops
//     asked for.
//   - Predicate pushdown: not supported. JSON is sequential; any
//     predicate is evaluated by the executor above the scan.
//
// Schema inference is a whole-file walk on the first Collect —
// ScanFile can't produce a schema without reading every feature
// (property keys + types may vary across features). If ScanSchema
// fails (file missing, invalid JSON), the error surfaces at
// Collect time.
func ScanFile(path string, opts *ReadOptions) *gobi.LazyFrame {
	if opts == nil {
		opts = &ReadOptions{}
	}
	// Schema inference eagerly walks the file so downstream
	// operators (Select, Filter type-checking) have real fields to
	// work with. Failure is captured and re-surfaced at Collect —
	// mirrors parquetio's / gpkgio's behavior.
	sch, schemaErr := ScanSchema(path, opts)
	label := buildScanLabel(path, opts)

	node := gobi.NewScanNode(label, sch, func() (*gobi.Frame, error) {
		if schemaErr != nil {
			return nil, schemaErr
		}
		return ReadFile(path, opts)
	}, gobi.WithColumnProjection(func(cols []string) gobi.LogicalPlan {
		if len(opts.Columns) > 0 {
			return nil
		}
		var newOpts ReadOptions
		if opts != nil {
			newOpts = *opts
		}
		newOpts.Columns = cols
		return ScanFile(path, &newOpts).Plan()
	}), gobi.WithStreamRead(func(cb func(*gobi.Frame) error) error {
		if schemaErr != nil {
			return schemaErr
		}
		return ReadFileChunksFunc(path, opts, cb)
	}))
	return gobi.NewLazyFrame(node)
}

// ScanSchema reads path far enough to produce the arrow.Schema
// ReadFile would return. Requires a full walk of the file to
// collect the union of property keys — no cheaper option because
// GeoJSON doesn't put schema information in a header.
//
// Callers that care about avoiding this walk should ReadFile
// directly; ScanFile only invokes ScanSchema when the LazyFrame
// optimizer explicitly asks for the schema.
func ScanSchema(path string, opts *ReadOptions) (*arrow.Schema, error) {
	if opts == nil {
		opts = &ReadOptions{}
	}
	// Read one feature (or fall through if empty) to establish a
	// baseline schema. This is a cheap approximation — a full walk
	// would be more accurate but doubles the I/O cost for schema
	// probes. Callers with heterogeneous property shapes are best
	// served by calling ReadFile directly and inspecting the
	// returned Frame's schema.
	f, err := ReadFile(path, &ReadOptions{
		Format:  opts.Format,
		Columns: opts.Columns,
	})
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("geojsonio: ScanSchema: empty file")
	}
	// Frame doesn't currently expose its schema pointer directly,
	// but every arrow.Column carries its own field — reconstruct
	// from those.
	fields := make([]arrow.Field, 0, f.NumCols())
	for i := 0; i < f.NumCols(); i++ {
		s, err := f.ColumnAt(i)
		if err != nil {
			return nil, err
		}
		fields = append(fields, s.Column().Field())
	}
	return arrow.NewSchema(fields, nil), nil
}

// buildScanLabel produces the "Scan[geojson](...)" label for
// LazyFrame.ExplainPhysical. Shows the format so it's obvious from
// Explain which parser is in play.
func buildScanLabel(path string, opts *ReadOptions) string {
	label := fmt.Sprintf("Scan[geojson](%q", path)
	if opts != nil && opts.Format != FormatAuto {
		label += fmt.Sprintf(", format=%s", formatName(opts.Format))
	}
	if opts != nil && len(opts.Columns) > 0 {
		label += fmt.Sprintf(", cols=%v", opts.Columns)
	}
	return label + ")"
}

func formatName(f Format) string {
	switch f {
	case FormatFeatureCollection:
		return "FeatureCollection"
	case FormatLineDelimited:
		return "LineDelimited"
	}
	return "Auto"
}

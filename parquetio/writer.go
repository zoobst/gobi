package parquetio

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/zoobst/gobi"
)

// Writer writes one parquet file incrementally: any number of Write
// calls, with row-group boundaries chosen by the caller, then Close.
// Use it when a file is too large to hold as one Frame, or when row
// groups must follow the data — e.g. one row group per spatial cell or
// per day, so readers can skip whole groups on a predicate.
//
//	w, err := parquetio.NewWriter(out, schema, &parquetio.WriteOptions{Codec: parquetio.CodecZstd})
//	for _, cell := range cells {
//	    if err := w.Write(cell.Frame); err != nil { ... }
//	    w.EndRowGroup() // cell boundary
//	}
//	stats, err := w.Close() // row count, size, per-column bounds
//
// Rows accumulate in the current row group across Write calls until
// EndRowGroup, or until the group reaches WriteOptions.RowGroupRows
// (then it is split automatically; 0 = arrow-go's 64Mi-row cap). A
// row group is held in memory, encoded and compressed, until it is
// closed — size groups to fit.
//
// The output matches Write for the same options and rows: same
// codec, bloom filters, timestamp coercion, stored Arrow schema,
// GeoParquet "geo" metadata (bbox and geometry types merged across
// every Write), declared Coverings (checked on every Write), the bbox
// covering columns for the rest unless SkipBboxCovering, and
// KeyValueMetadata. HilbertSort is not supported — it needs the whole file at
// once; sort each batch (or the input) before writing.
//
// Not safe for concurrent use. After any error the Writer is unusable:
// later calls return the same error, and Close still closes the
// underlying parquet writer.
type Writer struct {
	fw     *pqarrow.FileWriter
	cw     *countingWriter
	schema *arrow.Schema // input schema: every Write's frame must match

	opts *WriteOptions            // coverings / footer entries, applied per Write and at Close
	geo  *gobi.GeoParquetMetadata // merged across writes; nil if no geometry

	rows         int64 // rows written so far, all row groups
	rowGroupRows int64 // rows in the current row group
	cutPending   bool  // EndRowGroup was called; start a new group on the next row
	closed       bool
	err          error // sticky
}

// NewWriter starts a parquet file on w for frames with the given
// schema. opts is interpreted as for Write (nil = defaults), except
// that HilbertSort is rejected. Invalid options fail here, before
// anything is written.
func NewWriter(w io.Writer, schema *arrow.Schema, opts *WriteOptions) (*Writer, error) {
	if schema == nil {
		return nil, fmt.Errorf("parquetio: NewWriter: nil schema")
	}
	if opts == nil {
		opts = &WriteOptions{}
	}
	// Snapshot the options: Write and Close read them later, and a
	// caller editing their struct mid-file shouldn't change the file.
	o := *opts
	opts = &o
	if opts.HilbertSort {
		return nil, fmt.Errorf("parquetio: NewWriter: HilbertSort needs the whole file; sort frames before writing them")
	}
	writerProps, arrowProps, err := writerProperties(opts)
	if err != nil {
		return nil, err
	}

	if err := validateGeoOptions(schema, opts); err != nil {
		return nil, err
	}
	// Derive the on-disk schema and the starting "geo" metadata by
	// running the same preparation Write uses on an empty frame, so the
	// covering column layout can't drift from Write's.
	empty, err := emptyFrame(schema)
	if err != nil {
		return nil, err
	}
	defer empty.Release()
	aug, geo, err := prepareGeo(empty, opts)
	if err != nil {
		return nil, err
	}
	outSchema := aug.Schema()
	aug.Release()

	cw := &countingWriter{w: w}
	fw, err := pqarrow.NewFileWriter(
		writerSchema(outSchema, footerKeys(geo != nil, opts)),
		cw,
		parquet.NewWriterProperties(writerProps...),
		pqarrow.NewArrowWriterProperties(arrowProps...),
	)
	if err != nil {
		return nil, err
	}
	return &Writer{fw: fw, cw: cw, schema: schema, opts: opts, geo: geo}, nil
}

// Write appends f's rows to the current row group. f's schema must
// match the Writer's (same fields, types and field metadata; schema-
// level metadata is ignored). Zero-row frames are a no-op. The Writer
// doesn't retain f.
func (w *Writer) Write(f *gobi.Frame) error {
	if err := w.usable(); err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("parquetio: Writer.Write: nil frame")
	}
	if !f.Schema().Equal(w.schema) {
		return fmt.Errorf("parquetio: Writer.Write: frame schema does not match the writer's\nframe:  %s\nwriter: %s",
			f.Schema(), w.schema)
	}
	if f.NumRows() == 0 {
		return nil
	}

	aug, meta, err := prepareGeo(f, w.opts)
	if err != nil {
		// Nothing buffered yet for this frame (e.g. a covering check
		// failed): reject it without poisoning the writer.
		return fmt.Errorf("parquetio: Writer.Write: %w", err)
	}
	defer aug.Release()

	if w.cutPending && w.rowGroupRows > 0 {
		if err := w.fw.NewBufferedRowGroupChecked(); err != nil {
			return w.fail(err)
		}
		w.rowGroupRows = 0
	}
	w.cutPending = false

	tbl := aug.Table()
	defer tbl.Release()
	tr := array.NewTableReader(tbl, tbl.NumRows())
	defer tr.Release()
	for tr.Next() {
		if err := w.fw.WriteBuffered(tr.RecordBatch()); err != nil {
			return w.fail(err)
		}
	}
	n, err := w.fw.RowGroupNumRows()
	if err != nil {
		return w.fail(err)
	}
	w.rowGroupRows = int64(n)
	w.rows += int64(f.NumRows())
	mergeGeoMeta(w.geo, meta)
	return nil
}

// EndRowGroup ends the current row group: rows from the next Write
// start a new one. Calling it with nothing buffered (at the start,
// twice in a row, or right before Close) does nothing, so no empty row
// groups are written.
func (w *Writer) EndRowGroup() error {
	if err := w.usable(); err != nil {
		return err
	}
	w.cutPending = true
	return nil
}

// NumRows is the number of rows written so far, including the open
// row group (arrow-go's own count covers closed groups only).
func (w *Writer) NumRows() int64 { return w.rows }

// Close writes the footer (with the merged GeoParquet metadata, when
// the schema has geometry) and returns the file's statistics. It does
// not close the underlying io.Writer. Calling Close again returns an
// error.
func (w *Writer) Close() (*FileStats, error) {
	if w.closed {
		return nil, fmt.Errorf("parquetio: Writer already closed")
	}
	w.closed = true
	if w.err != nil {
		return nil, errors.Join(w.err, w.fw.Close())
	}
	if err := appendFooter(w.fw, w.geo, w.opts); err != nil {
		return nil, errors.Join(err, w.fw.Close())
	}
	if err := w.fw.Close(); err != nil {
		return nil, err
	}
	md, err := w.fw.FileMetadata()
	if err != nil {
		return nil, err
	}
	return StatsFromMetadata(md, w.cw.n)
}

func (w *Writer) usable() error {
	if w.closed {
		return fmt.Errorf("parquetio: Writer already closed")
	}
	return w.err
}

// fail records a sticky error: a partial row group can't be rolled
// back, so the file is unusable after any write error.
func (w *Writer) fail(err error) error {
	w.err = fmt.Errorf("parquetio: Writer: %w", err)
	return w.err
}

// mergeGeoMeta folds one batch's GeoParquet metadata into the file's:
// bboxes union, geometry types union (sorted). Encoding, CRS and
// covering come from the schema and are the same for every batch.
func mergeGeoMeta(dst, src *gobi.GeoParquetMetadata) {
	if dst == nil || src == nil {
		return
	}
	for name, s := range src.Columns {
		d, ok := dst.Columns[name]
		if !ok {
			continue
		}
		d.Bbox = unionBbox(d.Bbox, s.Bbox)
		for _, t := range s.GeometryTypes {
			if !slices.Contains(d.GeometryTypes, t) {
				d.GeometryTypes = append(d.GeometryTypes, t)
			}
		}
		slices.Sort(d.GeometryTypes)
		dst.Columns[name] = d
	}
}

// unionBbox unions two GeoParquet bboxes ([xmin, ymin, xmax, ymax];
// gobi writes 2D bboxes). An empty side yields the other.
func unionBbox(a, b []float64) []float64 {
	switch {
	case len(b) == 0:
		return a
	case len(a) == 0:
		return slices.Clone(b)
	}
	return []float64{min(a[0], b[0]), min(a[1], b[1]), max(a[2], b[2]), max(a[3], b[3])}
}

// emptyFrame builds a zero-row Frame with schema's fields (including
// field metadata such as geometry tags).
func emptyFrame(schema *arrow.Schema) (*gobi.Frame, error) {
	pool := memory.DefaultAllocator
	cols := make([]arrow.Column, schema.NumFields())
	for i, fld := range schema.Fields() {
		arr := array.MakeArrayOfNull(pool, fld.Type, 0)
		ch := arrow.NewChunked(fld.Type, []arrow.Array{arr})
		arr.Release()
		cols[i] = *arrow.NewColumn(fld, ch)
		ch.Release()
	}
	return gobi.NewFrame(schema, cols)
}

// countingWriter counts bytes written, for FileStats.FileSize. Like
// writeOnly, it hides any Seek/ReaderAt on the destination.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

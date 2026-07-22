package pgio

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/jackc/pgx/v5"

	"github.com/zoobst/gobi"
)

// WriteTable inserts df into the named table using pgx's CopyFrom
// protocol — 10-100× faster than a naive INSERT loop for bulk
// loads. The target table MUST exist; pgio doesn't attempt to
// create it because PostgreSQL DDL is too coupled to the caller's
// conventions (index strategy, constraints, tablespace, etc.).
//
// The Frame's arrow columns are mapped to PostgreSQL types via
// standard Go-value promotion in pgx: Int64 → int8, Float64 →
// float8, String → text, Boolean → bool, Timestamp → timestamptz,
// Binary → bytea. Geometry-tagged columns are encoded as EWKB
// (PostGIS's SRID-carrying WKB variant) with the SRID sourced from
// opts.SRID or the field's MetaGeometryCRS metadata.
//
// When opts.Truncate is true, WriteTable issues `TRUNCATE TABLE ...`
// before the copy — useful for full-refresh ETL. Truncate + copy
// aren't wrapped in a single transaction: if the copy fails after
// truncate, the table is left empty. Wrap the two in your own
// pgx.Tx if that's a problem for your workflow.
func WriteTable(ctx context.Context, conn Conn, table string, df *gobi.Frame, opts *WriteOptions) error {
	if opts == nil {
		opts = &WriteOptions{}
	}
	schema := defaultSchema(opts.Schema)

	// Detect the geometry column: explicit opts.GeomCol wins;
	// otherwise the first is-geometry column in the Frame.
	geomIdx := -1
	if opts.GeomCol != "" {
		names := df.ColumnNames()
		for i, n := range names {
			if n == opts.GeomCol {
				geomIdx = i
				break
			}
		}
		if geomIdx < 0 {
			return fmt.Errorf("pgio: WriteTable: GeomCol %q not found in frame", opts.GeomCol)
		}
	} else {
		for i := 0; i < df.NumCols(); i++ {
			s, _ := df.ColumnAt(i)
			if s.IsGeometry() {
				geomIdx = i
				break
			}
		}
	}

	// Resolve SRID: explicit > field metadata > 4326 default.
	srid := opts.SRID
	if geomIdx >= 0 && srid == 0 {
		s, _ := df.ColumnAt(geomIdx)
		srid = geometryEPSGFromField(s.Column().Field())
	}
	if srid == 0 {
		srid = 4326
	}

	if opts.Truncate {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`TRUNCATE TABLE %s.%s`,
			quoteIdent(schema), quoteIdent(table))); err != nil {
			return fmt.Errorf("pgio: truncate: %w", err)
		}
	}

	// Build row extractors — one per column, called per row to pull
	// the properly-typed Go value out of the arrow buffer.
	names := df.ColumnNames()
	extractors := make([]rowExtractor, df.NumCols())
	for i := 0; i < df.NumCols(); i++ {
		s, err := df.ColumnAt(i)
		if err != nil {
			return err
		}
		if i == geomIdx {
			ext, err := geometryExtractor(s, srid)
			if err != nil {
				return err
			}
			extractors[i] = ext
			continue
		}
		ext, err := scalarExtractor(s)
		if err != nil {
			return fmt.Errorf("pgio: column %q: %w", names[i], err)
		}
		extractors[i] = ext
	}

	// CopyFromSource iterates one row at a time; we implement it as
	// a stateful cursor over the Frame's rows.
	src := &frameCopySource{
		nRows:      df.NumRows(),
		extractors: extractors,
	}
	_, err := conn.CopyFrom(ctx,
		pgx.Identifier{schema, table},
		names,
		src)
	if src.err != nil {
		return src.err // extractor error surfaces here
	}
	if err != nil {
		return fmt.Errorf("pgio: CopyFrom: %w", err)
	}
	return nil
}

// rowExtractor pulls the value at row from a Series in its
// PostgreSQL-appropriate Go type. Returns nil for a null cell.
type rowExtractor func(row int) (any, error)

// scalarExtractor picks the right per-row reader for a non-geometry
// column, based on its arrow type.
func scalarExtractor(s gobi.Series) (rowExtractor, error) {
	chunks := s.Column().Data().Chunks()
	if len(chunks) != 1 {
		return nil, fmt.Errorf("multi-chunk columns not supported by WriteTable (call frame.Coalesce first)")
	}
	switch a := chunks[0].(type) {
	case *array.Int16:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Int32:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Int64:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Float32:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Float64:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Boolean:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.String:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Binary:
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return a.Value(row), nil
		}, nil
	case *array.Timestamp:
		unit := a.DataType().(*arrow.TimestampType).Unit
		return func(row int) (any, error) {
			if a.IsNull(row) {
				return nil, nil
			}
			return arrowTimestampToTime(int64(a.Value(row)), unit), nil
		}, nil
	}
	return nil, fmt.Errorf("unsupported arrow type %s for WriteTable", s.DataType())
}

// geometryExtractor wraps a Binary column with the EWKB header
// PostGIS expects on the wire. Each row's raw WKB gets the SRID
// prefix added; nulls pass through as nil.
func geometryExtractor(s gobi.Series, srid int32) (rowExtractor, error) {
	chunks := s.Column().Data().Chunks()
	if len(chunks) != 1 {
		return nil, fmt.Errorf("multi-chunk geometry columns not supported by WriteTable")
	}
	a, ok := chunks[0].(*array.Binary)
	if !ok {
		return nil, fmt.Errorf("geometry column must be arrow Binary, got %T", chunks[0])
	}
	return func(row int) (any, error) {
		if a.IsNull(row) {
			return nil, nil
		}
		return encodeEWKB(a.Value(row), srid), nil
	}, nil
}

// encodeEWKB wraps a WKB blob in the EWKB SRID header PostGIS
// expects. Layout is the inverse of decodeEWKB — sets the SRID flag
// in the type field and inserts the 4-byte SRID at offset 5.
//
// Only handles little-endian WKB input (byte-order flag = 1). Most
// tools produce little-endian WKB; gobi's geometry codec always does.
// Big-endian input is rare enough that a future need can add a
// branch here.
func encodeEWKB(wkb []byte, srid int32) []byte {
	if len(wkb) < 5 {
		return wkb // truncated; let PostGIS reject it
	}
	// Read the WKB type (uint32 LE at bytes 1-4) so we can OR in
	// the SRID flag.
	le := wkb[0] == 1
	var typeBits uint32
	if le {
		typeBits = binary.LittleEndian.Uint32(wkb[1:5])
	} else {
		typeBits = binary.BigEndian.Uint32(wkb[1:5])
	}
	const sridFlag = 0x20000000
	typeBits |= sridFlag

	out := make([]byte, 0, len(wkb)+4)
	out = append(out, wkb[0]) // byte order
	if le {
		out = binary.LittleEndian.AppendUint32(out, typeBits)
		out = binary.LittleEndian.AppendUint32(out, uint32(srid))
	} else {
		out = binary.BigEndian.AppendUint32(out, typeBits)
		out = binary.BigEndian.AppendUint32(out, uint32(srid))
	}
	out = append(out, wkb[5:]...)
	return out
}

// arrowTimestampToTime converts an arrow timestamp integer + unit
// to a time.Time. pgx expects time.Time for timestamptz columns.
func arrowTimestampToTime(v int64, unit arrow.TimeUnit) time.Time {
	switch unit {
	case arrow.Second:
		return time.Unix(v, 0).UTC()
	case arrow.Millisecond:
		return time.Unix(v/1000, (v%1000)*int64(time.Millisecond)).UTC()
	case arrow.Microsecond:
		return time.Unix(v/1_000_000, (v%1_000_000)*int64(time.Microsecond)).UTC()
	case arrow.Nanosecond:
		return time.Unix(0, v).UTC()
	}
	return time.Time{}
}

// geometryEPSGFromField reads the "gobi:crs_epsg" metadata a
// gobi.GeometryField sets. Duplicates the gpkgio helper because
// pgio can't import gpkgio (would introduce a cycle risk).
// Bad values fall through to 0 (unknown SRID).
func geometryEPSGFromField(f arrow.Field) int32 {
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

// frameCopySource adapts a Frame into pgx's CopyFromSource
// interface. Iterates rows one at a time, running the per-column
// extractors to produce the []any pgx expects. Any extractor error
// is stashed in the struct so WriteTable can return it — pgx's
// CopyFrom will halt and return a generic error otherwise.
type frameCopySource struct {
	nRows      int
	extractors []rowExtractor
	row        int
	buf        []any
	err        error
}

func (s *frameCopySource) Next() bool {
	if s.err != nil {
		return false
	}
	if s.row >= s.nRows {
		return false
	}
	return true
}

func (s *frameCopySource) Values() ([]any, error) {
	if s.buf == nil {
		s.buf = make([]any, len(s.extractors))
	}
	for i, ext := range s.extractors {
		v, err := ext(s.row)
		if err != nil {
			s.err = fmt.Errorf("row %d col %d: %w", s.row, i, err)
			return nil, s.err
		}
		s.buf[i] = v
	}
	s.row++
	return s.buf, nil
}

func (s *frameCopySource) Err() error { return s.err }

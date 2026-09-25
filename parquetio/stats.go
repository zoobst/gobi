package parquetio

import (
	"bytes"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/metadata"
)

// FileStats summarizes a written parquet file from its footer: the
// numbers a table format needs to register the file (row count, size,
// per-column value / null counts and bounds) without re-reading it.
// Writer.Close returns one; StatsFromMetadata builds one from any
// footer.
type FileStats struct {
	// NumRows is the file's total row count.
	NumRows int64
	// FileSize is the file's size in bytes, footer included. Zero when
	// built by StatsFromMetadata without a size.
	FileSize int64
	// NumRowGroups is the number of row groups in the file.
	NumRowGroups int
	// Columns has one entry per parquet leaf column, in schema order.
	Columns []ColumnStats
	// Metadata is the raw footer, for anything not summarized here
	// (per-row-group stats, encodings, key-value metadata).
	Metadata *metadata.FileMetaData
}

// ColumnStats is one leaf column's statistics merged across every row
// group.
type ColumnStats struct {
	// Path is the dotted parquet column path — the column name for a
	// top-level column.
	Path string
	// PhysicalType is the parquet physical type; it says how Min / Max
	// and MinBytes / MaxBytes are represented.
	PhysicalType parquet.Type
	// NumValues counts the column's values, nulls included. For a
	// top-level column this equals NumRows.
	NumValues int64
	// NullCount is the number of null values. Only meaningful when
	// HasNullCount is true (every row group recorded a null count).
	NullCount    int64
	HasNullCount bool
	// Min / Max are the column's bounds, decoded by physical type:
	// int32 (INT32, which also carries int8/int16/uint8/uint16/date),
	// int64 (INT64, incl. timestamps in the column's unit), float32,
	// float64, bool, or []byte (BYTE_ARRAY / FIXED_LEN_BYTE_ARRAY:
	// strings, binary, WKB, and float16 as its 2 raw bytes). Ordering
	// follows the column's parquet sort order, so unsigned columns
	// compare unsigned.
	//
	// Only set when HasMinMax is true.
	Min, Max any
	// MinBytes / MaxBytes are the same bounds PLAIN-encoded:
	// little-endian for numbers, raw bytes (no length prefix) for byte
	// arrays, one byte for bool. For int, long, float, double, date,
	// timestamp, string, binary and boolean columns this is also
	// Iceberg's single-value binary serialization. Decimal and UUID
	// columns differ.
	MinBytes, MaxBytes []byte
	// HasMinMax reports whether the bounds cover every non-null value
	// in the file. False when some row group with non-null values has
	// no min/max statistics (e.g. values too large for the writer to
	// record), or when the column is entirely null.
	HasMinMax bool
	// CompressedBytes / UncompressedBytes total the column's chunks.
	CompressedBytes, UncompressedBytes int64
}

// StatsFromMetadata summarizes a parquet footer. fileSize is recorded
// as FileSize (pass 0 if unknown). Works on any footer, not just ones
// gobi wrote — e.g. arrow-go's file.Reader.MetaData() for an existing
// file.
func StatsFromMetadata(md *metadata.FileMetaData, fileSize int64) (*FileStats, error) {
	if md == nil {
		return nil, fmt.Errorf("parquetio: StatsFromMetadata: nil metadata")
	}
	sc := md.Schema
	out := &FileStats{
		NumRows:      md.NumRows,
		FileSize:     fileSize,
		NumRowGroups: md.NumRowGroups(),
		Columns:      make([]ColumnStats, sc.NumColumns()),
		Metadata:     md,
	}
	for c := range sc.NumColumns() {
		descr := sc.Column(c)
		cs := ColumnStats{
			Path:         descr.ColumnPath().String(),
			PhysicalType: descr.PhysicalType(),
			HasNullCount: true,
			HasMinMax:    true,
		}
		merged := metadata.NewStatistics(descr, memory.DefaultAllocator)
		anyMinMax := false
		for rg := range md.NumRowGroups() {
			cc, err := md.RowGroup(rg).ColumnChunk(c)
			if err != nil {
				return nil, fmt.Errorf("parquetio: row group %d column %q: %w", rg, cs.Path, err)
			}
			cs.NumValues += cc.NumValues()
			cs.CompressedBytes += cc.TotalCompressedSize()
			cs.UncompressedBytes += cc.TotalUncompressedSize()

			set, err := cc.StatsSet()
			if err != nil {
				return nil, fmt.Errorf("parquetio: row group %d column %q stats: %w", rg, cs.Path, err)
			}
			if !set {
				cs.HasNullCount = false
				if cc.NumValues() > 0 {
					cs.HasMinMax = false
				}
				continue
			}
			st, err := cc.Statistics()
			if err != nil {
				return nil, fmt.Errorf("parquetio: row group %d column %q stats: %w", rg, cs.Path, err)
			}
			if !st.HasNullCount() {
				cs.HasNullCount = false
			} else {
				cs.NullCount += st.NullCount()
			}
			switch {
			case st.HasMinMax():
				anyMinMax = true
				merged.Merge(st)
			case st.HasNullCount() && st.NullCount() == cc.NumValues():
				// All-null row group: no values to bound.
			default:
				cs.HasMinMax = false
			}
		}
		if !cs.HasNullCount {
			cs.NullCount = 0
		}
		if cs.HasMinMax && anyMinMax {
			cs.Min, cs.Max = statsBounds(merged)
			cs.MinBytes = bytes.Clone(merged.EncodeMin())
			cs.MaxBytes = bytes.Clone(merged.EncodeMax())
		} else {
			cs.HasMinMax = false
		}
		out.Columns[c] = cs
	}
	return out, nil
}

// statsBounds decodes typed min / max from merged statistics.
// Byte-array bounds are cloned: the stats object owns their memory.
func statsBounds(st metadata.TypedStatistics) (any, any) {
	switch s := st.(type) {
	case *metadata.Int32Statistics:
		return s.Min(), s.Max()
	case *metadata.Int64Statistics:
		return s.Min(), s.Max()
	case *metadata.Float32Statistics:
		return s.Min(), s.Max()
	case *metadata.Float64Statistics:
		return s.Min(), s.Max()
	case *metadata.BooleanStatistics:
		return s.Min(), s.Max()
	case *metadata.ByteArrayStatistics:
		return bytes.Clone(s.Min()), bytes.Clone(s.Max())
	case *metadata.FixedLenByteArrayStatistics:
		return bytes.Clone(s.Min()), bytes.Clone(s.Max())
	case *metadata.Float16Statistics:
		return bytes.Clone(s.Min()), bytes.Clone(s.Max())
	}
	return nil, nil
}

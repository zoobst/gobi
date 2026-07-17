// Package parquetio reads and writes gobi Frames as Apache Parquet.
//
// Compression is delegated to Parquet's built-in codecs. When a Frame
// contains geometry columns, WriteFile emits a GeoParquet 1.1 metadata
// blob under the Parquet file-level "geo" key; ReadFile re-hydrates it
// into the returned Frame's schema.
package parquetio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/memory"
	"github.com/apache/arrow/go/v18/parquet"
	"github.com/apache/arrow/go/v18/parquet/compress"
	"github.com/apache/arrow/go/v18/parquet/file"
	"github.com/apache/arrow/go/v18/parquet/pqarrow"

	"github.com/zoobst/gobi"
)

// Codec identifies a Parquet-level compression codec.
type Codec string

const (
	CodecUncompressed Codec = "uncompressed"
	CodecSnappy       Codec = "snappy"
	CodecGzip         Codec = "gzip"
	CodecBrotli       Codec = "brotli"
	CodecLZ4          Codec = "lz4"
	CodecZstd         Codec = "zstd"
)

// ErrUnknownCodec is returned by ParseCodec when the codec name is not
// recognized.
var ErrUnknownCodec = errors.New("parquetio: unknown compression codec")

// ParseCodec resolves a codec by name (case-insensitive). Empty and "none"
// map to CodecUncompressed.
func ParseCodec(s string) (Codec, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "uncompressed":
		return CodecUncompressed, nil
	case "snappy":
		return CodecSnappy, nil
	case "gzip", "gz":
		return CodecGzip, nil
	case "brotli", "br":
		return CodecBrotli, nil
	case "lz4":
		return CodecLZ4, nil
	case "zstd":
		return CodecZstd, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownCodec, s)
	}
}

func (c Codec) toArrow() (compress.Compression, error) {
	switch c {
	case CodecUncompressed:
		return compress.Codecs.Uncompressed, nil
	case CodecSnappy:
		return compress.Codecs.Snappy, nil
	case CodecGzip:
		return compress.Codecs.Gzip, nil
	case CodecBrotli:
		return compress.Codecs.Brotli, nil
	case CodecLZ4:
		return compress.Codecs.Lz4Raw, nil
	case CodecZstd:
		return compress.Codecs.Zstd, nil
	default:
		return compress.Codecs.Uncompressed, fmt.Errorf("%w: %q", ErrUnknownCodec, c)
	}
}

// ReadFile reads path into a Frame. The Parquet file's metadata determines
// compression; if the file has a GeoParquet "geo" key, it is re-attached to
// the Frame's Arrow schema so downstream code can detect geometry columns.
func ReadFile(path string) (*gobi.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pf, err := file.NewParquetReader(f)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	geoRaw := ""
	if kv := pf.MetaData().KeyValueMetadata(); kv != nil {
		if v := kv.FindValue(gobi.GeoParquetMetadataKey); v != nil {
			geoRaw = *v
		}
	}

	rdr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{Parallel: true}, memory.DefaultAllocator)
	if err != nil {
		return nil, err
	}
	table, err := rdr.ReadTable(context.Background())
	if err != nil {
		return nil, err
	}

	frame := gobi.NewFrameFromTable(table)
	if geoRaw != "" {
		schema, err := attachGeoKey(frame.Schema(), geoRaw)
		if err != nil {
			return nil, err
		}
		frame = gobi.NewFrameFromTable(rebuiltTable(schema, table))
	}
	return frame, nil
}

// WriteFile writes f to path with the requested codec. If f contains any
// geometry columns, the output includes a GeoParquet 1.1 metadata blob
// under the file-level "geo" key.
func WriteFile(f *gobi.Frame, path string, codec Codec) error {
	compression, err := codec.toArrow()
	if err != nil {
		return err
	}
	meta, err := gobi.BuildGeoParquetMetadata(f)
	if err != nil {
		return err
	}

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	writer, err := pqarrow.NewFileWriter(
		f.Schema(),
		out,
		parquet.NewWriterProperties(parquet.WithCompression(compression)),
		pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()),
	)
	if err != nil {
		return err
	}
	if err := writer.WriteTable(f.Table(), int64(f.NumRows())); err != nil {
		_ = writer.Close()
		return err
	}
	if meta != nil {
		blob, err := marshalGeoMeta(meta)
		if err != nil {
			_ = writer.Close()
			return err
		}
		if err := writer.AppendKeyValueMetadata(gobi.GeoParquetMetadataKey, blob); err != nil {
			_ = writer.Close()
			return err
		}
	}
	return writer.Close()
}

// marshalGeoMeta serializes the metadata blob. Kept here (rather than
// exposed on gobi) so the JSON layout stays a parquetio implementation
// detail.
func marshalGeoMeta(meta *gobi.GeoParquetMetadata) (string, error) {
	return gobi.MarshalGeoParquetMetadata(meta)
}

// attachGeoKey returns schema with the "geo" file-level metadata key set
// to raw.
func attachGeoKey(schema *arrow.Schema, raw string) (*arrow.Schema, error) {
	keys := []string{gobi.GeoParquetMetadataKey}
	values := []string{raw}
	if schema.HasMetadata() {
		old := schema.Metadata()
		for i, k := range old.Keys() {
			if k == gobi.GeoParquetMetadataKey {
				continue
			}
			keys = append(keys, k)
			values = append(values, old.Values()[i])
		}
	}
	md := arrow.NewMetadata(keys, values)
	return arrow.NewSchema(schema.Fields(), &md), nil
}

// rebuiltTable wraps table under a new schema, keeping column data intact.
func rebuiltTable(schema *arrow.Schema, table arrow.Table) arrow.Table {
	cols := make([]arrow.Column, table.NumCols())
	for i := int64(0); i < table.NumCols(); i++ {
		cols[i] = *table.Column(int(i))
	}
	return newTableFromColumns(schema, cols, table.NumRows())
}

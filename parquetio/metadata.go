package parquetio

import (
	"errors"
	"fmt"
	"io"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"
)

// ReadMetadata reads and parses the footer of the Parquet file r (size
// bytes long), for ReadOptions.Metadata: parse it once, then pass it to
// each read of the same file.
func ReadMetadata(r io.ReaderAt, size int64) (*metadata.FileMetaData, error) {
	pf, err := file.NewParquetReader(newReaderAtSeeker(r, size))
	if err != nil {
		return nil, err
	}
	md := pf.MetaData()
	_ = pf.Close()
	return md, nil
}

// errEncryptedMetadata: a read with ReadOptions.Metadata skips parsing
// the footer, which is where decryption is set up.
var errEncryptedMetadata = errors.New("parquetio: ReadOptions.Metadata isn't supported for encrypted files")

// metadataForRead returns a copy of md for one read to use. arrow-go
// fills the writer version in on first use when it's unset (parsing a
// footer sets it; metadata assembled otherwise may not), and both
// concurrent reads and one read's per-column goroutines would then
// write it at once. So each read gets its own shallow copy, filled in
// here before any goroutine sees it; the parsed footer itself is still
// shared, and only read.
func metadataForRead(md *metadata.FileMetaData) (*metadata.FileMetaData, error) {
	if md.IsSetEncryptionAlgorithm() || md.FileDecryptor != nil {
		return nil, errEncryptedMetadata
	}
	own := *md
	own.WriterVersion()
	return &own, nil
}

// checkMetadataFits verifies that the column chunks of rowGroups, as
// md places them, lie within a file of size bytes before its footer
// (ending with the footer's length and "PAR1"). It catches metadata
// from another or a since-replaced file without any reads; it can't
// catch a different file of at least the same size.
func checkMetadataFits(md *metadata.FileMetaData, rowGroups []int, size int64) error {
	limit := size - 8
	if n := int64(md.Size()); n > 0 {
		limit -= n
	}
	for _, i := range rowGroups {
		for j, cc := range md.RowGroups[i].Columns {
			m := cc.MetaData
			if m == nil {
				continue
			}
			start := m.DataPageOffset
			if m.DictionaryPageOffset != nil && *m.DictionaryPageOffset > 0 && *m.DictionaryPageOffset < start {
				start = *m.DictionaryPageOffset
			}
			if end := start + m.TotalCompressedSize; start < 0 || end > limit {
				return fmt.Errorf("parquetio: ReadOptions.Metadata doesn't match this file: row group %d column %d ends at byte %d, past its data (%d bytes)",
					i, j, end, limit)
			}
		}
	}
	return nil
}

// fileSize is rs's length, found by seeking to its end.
func fileSize(rs parquet.ReaderAtSeeker) (int64, error) {
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	_, err = rs.Seek(0, io.SeekStart)
	return size, err
}

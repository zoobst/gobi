package csvio

import (
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Codec selects the stream-compression codec used when reading a CSV.
type Codec string

const (
	// CodecAuto is the zero value: infer the codec from the filename in
	// ReadFile, or treat as uncompressed when the source is a plain
	// io.Reader (Read).
	CodecAuto Codec = ""
	// CodecNone forces no decompression, regardless of filename.
	CodecNone Codec = "none"
	// CodecGzip decompresses via compress/gzip (RFC 1952, ".gz" files).
	CodecGzip Codec = "gzip"
	// CodecZstd decompresses via klauspost/compress/zstd (".zst" files).
	CodecZstd Codec = "zstd"
	// CodecBzip2 decompresses via compress/bzip2 (".bz2" files). Read-only;
	// the Go stdlib doesn't ship a bzip2 writer.
	CodecBzip2 Codec = "bzip2"
)

// ErrUnknownCodec is returned when a codec name isn't recognized.
var ErrUnknownCodec = fmt.Errorf("csvio: unknown compression codec")

// detectCodecFromPath returns the codec implied by a filename's extension,
// or CodecNone when none matches. Extensions are matched case-insensitively.
func detectCodecFromPath(path string) Codec {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".csv.gz"), strings.HasSuffix(lower, ".gz"):
		return CodecGzip
	case strings.HasSuffix(lower, ".csv.zst"), strings.HasSuffix(lower, ".zst"),
		strings.HasSuffix(lower, ".zstd"):
		return CodecZstd
	case strings.HasSuffix(lower, ".csv.bz2"), strings.HasSuffix(lower, ".bz2"):
		return CodecBzip2
	}
	return CodecNone
}

// wrapCodec returns a reader that decompresses r using codec, plus a
// release function callers should defer. The release function is safe to
// call on any codec, including CodecNone.
func wrapCodec(r io.Reader, codec Codec) (io.Reader, func(), error) {
	switch codec {
	case CodecNone, CodecAuto:
		return r, func() {}, nil
	case CodecGzip:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("csvio: gzip: %w", err)
		}
		return gz, func() { _ = gz.Close() }, nil
	case CodecZstd:
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("csvio: zstd: %w", err)
		}
		return z, func() { z.Close() }, nil
	case CodecBzip2:
		// compress/bzip2 has no Close(); the underlying file is closed by
		// ReadFile's defer.
		return bzip2.NewReader(r), func() {}, nil
	default:
		return nil, nil, fmt.Errorf("%w: %q", ErrUnknownCodec, codec)
	}
}

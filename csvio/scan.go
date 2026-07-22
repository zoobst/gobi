package csvio

import (
	"fmt"
	"os"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/zoobst/gobi"
)

// ScanFile returns a LazyFrame that reads path as CSV when Collect
// is called. Schema is inferred from the type parameter T (same
// struct-tag conventions as ReadFile[T]).
//
// The scan participates in gobi's optimizer:
//
//   - Streaming: reads flow one batch at a time through
//     ReadFileChunksFunc[T]. Peak memory stays bounded to one
//     batch even on multi-GB inputs.
//   - Projection: gobi.Frame.Select() above the scan applies at the
//     LazyFrame layer — the Frame drops unused columns before the
//     executor sees them. CSV lacks random-access column projection
//     at the parser level (arrow-csv panics if IncludeColumns is
//     mixed with an explicit schema), so parse work is NOT reduced;
//     memory downstream of the parse is. Adding true parser-level
//     column skipping would require dropping the T-derived schema
//     and using arrow-csv's WithIncludeColumns — a v2 concern.
//   - Predicate pushdown: not supported. CSV is sequential, so any
//     predicate is evaluated by the executor above the scan.
//
// Compression is auto-detected from the filename extension unless
// ReadOptions.Compression is set explicitly — same rules as ReadFile[T].
func ScanFile[T any](path string, opts *ReadOptions) *gobi.LazyFrame {
	// Try to read the schema eagerly by peeking at the CSV header
	// with an empty-body read. If the file doesn't exist, defer the
	// error to Collect time — matches parquetio's / gpkgio's ScanFile
	// behavior.
	sch, schemaErr := scanSchemaFor[T](path, opts)

	label := buildScanLabel(path, opts)

	node := gobi.NewScanNode(label, sch, func() (*gobi.Frame, error) {
		if schemaErr != nil {
			return nil, schemaErr
		}
		return ReadFile[T](path, opts)
	}, gobi.WithStreamRead(func(cb func(*gobi.Frame) error) error {
		if schemaErr != nil {
			return schemaErr
		}
		return ReadFileChunksFunc[T](path, opts, cb)
	}))
	return gobi.NewLazyFrame(node)
}

// scanSchemaFor returns the arrow.Schema that would come out of
// ReadFile[T] against path/opts, without actually reading any rows.
// Achieved by peeking at the CSV header — the cheapest way to get a
// schema when T's tags plus header positions determine the layout.
//
// If the file can't be opened, the returned error is surfaced at
// Collect time via the read closure. Callers that want the schema
// eagerly can still get it by calling ReadFile[T] with LIMIT 0 (but
// there's no LIMIT in the current API; scanSchemaFor is the
// substitute).
func scanSchemaFor[T any](path string, opts *ReadOptions) (*arrow.Schema, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if opts == nil {
		opts = &ReadOptions{}
	}
	if opts.Compression == CodecAuto {
		local := *opts
		local.Compression = detectCodecFromPath(path)
		opts = &local
	}
	sc, err := setupReader[T](f, opts)
	if err != nil {
		return nil, err
	}
	defer sc.close()
	return sc.outSchema, nil
}

// buildScanLabel produces the "Scan[csv](...)" label for
// gobi.LazyFrame.ExplainPhysical(). Shows the codec (when explicit)
// so it's obvious from Explain whether decompression is happening.
func buildScanLabel(path string, opts *ReadOptions) string {
	label := fmt.Sprintf("Scan[csv](%q", path)
	if opts != nil && opts.Compression != "" && opts.Compression != CodecAuto {
		label += fmt.Sprintf(", codec=%s", opts.Compression)
	}
	return label + ")"
}

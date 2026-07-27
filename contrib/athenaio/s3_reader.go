package athenaio

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3ReaderAt implements io.ReaderAt over s3.GetObject with Range
// headers. Feeds parquetio.ReadReader / ReadReaderChunksFunc without
// requiring the caller to download the whole result file to disk.
//
// Concurrency: safe for concurrent ReadAt calls. Each ReadAt issues
// its own GetObject with a unique Range header, so requests don't
// interfere with each other. Callers who care about backpressure
// (many small ReadAt calls thrashing S3 API rate limits) should
// batch upstream or reduce parallelism.
type s3ReaderAt struct {
	ctx    context.Context
	s3     S3API
	bucket string
	key    string
	size   int64
}

// newS3ReaderAt looks up the object's size via HeadObject and returns
// a reader-at + size pair ready to hand to parquetio.ReadReader.
// Errors surface at construction time so callers don't discover
// missing objects on first Parquet footer read.
func newS3ReaderAt(ctx context.Context, api S3API, bucket, key string) (*s3ReaderAt, int64, error) {
	head, err := api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("athenaio: HeadObject s3://%s/%s: %w", bucket, key, err)
	}
	if head.ContentLength == nil {
		return nil, 0, fmt.Errorf("athenaio: HeadObject s3://%s/%s: nil ContentLength", bucket, key)
	}
	size := *head.ContentLength
	return &s3ReaderAt{
		ctx:    ctx,
		s3:     api,
		bucket: bucket,
		key:    key,
		size:   size,
	}, size, nil
}

// ReadAt reads len(p) bytes from the S3 object starting at offset off.
// Issues a single GetObject with a `bytes=off-off+len(p)-1` Range
// header. Short reads at EOF are surfaced with io.EOF; other errors
// wrap the underlying S3 error.
func (r *s3ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	if end >= r.size {
		end = r.size - 1
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d", off, end)
	out, err := r.s3.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return 0, fmt.Errorf("athenaio: GetObject s3://%s/%s Range=%s: %w",
			r.bucket, r.key, rangeHeader, err)
	}
	defer out.Body.Close()

	n, err := io.ReadFull(out.Body, p[:end-off+1])
	if err == io.ErrUnexpectedEOF {
		// Short read but not necessarily wrong — return what we got
		// with io.EOF if the read reached the object's tail.
		if off+int64(n) >= r.size {
			return n, io.EOF
		}
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("athenaio: read body s3://%s/%s: %w",
			r.bucket, r.key, err)
	}
	// If the caller asked for more than the object has left, surface
	// EOF alongside the partial read.
	if off+int64(n) >= r.size && n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// listBucketFiles enumerates all objects under prefix (an
// s3://bucket/prefix/ URI), returning fully-qualified s3://
// URIs to data files.
//
// Uses a negative filter (skip known non-data patterns; accept
// everything else) rather than a positive `.parquet` suffix filter,
// because Athena engine v2 CTAS writes parquet outputs *without* a
// `.parquet` extension — files look like
// `20240101_120000_00000_bucket-00000`. A positive suffix filter
// dropped every v2 data file and surfaced as a spurious "no result
// files" error at the caller. See the `isCTASDataKey` helper for
// the exclusion list.
//
// Handles ListObjectsV2 pagination via ContinuationToken. Returns
// results in the order ListObjectsV2 emits them (lexicographic by
// key) so downstream concatenation is deterministic across runs.
func listBucketFiles(ctx context.Context, api S3API, prefix string) ([]string, error) {
	bucket, keyPrefix, err := parseS3URI(prefix)
	if err != nil {
		return nil, fmt.Errorf("athenaio: listBucketFiles: %w", err)
	}
	// Force trailing slash so ListObjectsV2 scopes to genuine
	// children — e.g. prefix `tables/abc` would otherwise also
	// match a sibling `tables/abc-extra/…`. No-op when the caller
	// already supplied a slash; skipped for the empty-prefix case
	// (whole-bucket listing) to avoid a `//` double slash.
	if keyPrefix != "" && !strings.HasSuffix(keyPrefix, "/") {
		keyPrefix += "/"
	}
	var out []string
	var token *string
	for {
		resp, err := api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(keyPrefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("athenaio: ListObjectsV2 %s: %w", prefix, err)
		}
		for _, obj := range resp.Contents {
			if obj.Key == nil {
				continue
			}
			if !isCTASDataKey(*obj.Key) {
				continue
			}
			out = append(out, fmt.Sprintf("s3://%s/%s", bucket, *obj.Key))
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
}

// isCTASDataKey reports whether an S3 object key looks like a CTAS
// data file (as opposed to Iceberg metadata / Hive symlink manifest
// / zero-byte directory marker / checksum sidecar). The filter is
// deliberately permissive: Athena engine v2 writes parquet outputs
// with no extension, so any suffix-based positive filter would
// under-count. Anything not on this exclusion list is treated as
// data — downstream `parquetio.ReadReader` will fail loudly if a
// non-parquet blob slips through, which is more actionable than
// silently dropping the file at list time.
func isCTASDataKey(key string) bool {
	// Directory marker (zero-byte object whose key ends with `/`).
	if strings.HasSuffix(key, "/") {
		return false
	}
	// Iceberg metadata sits under a `metadata/` subdirectory.
	if strings.Contains(key, "/metadata/") {
		return false
	}
	// Hive symlink manifest layout.
	if strings.Contains(key, "/_symlink_format_manifest/") {
		return false
	}
	// Common non-data suffixes across Athena / Iceberg / Hive:
	//   - .metadata.json / .avro   → Iceberg manifests
	//   - .csv                     → Athena manifest CSVs, query
	//                                result CSVs (e.g.
	//                                `<queryID>-manifest.csv`);
	//                                CSVs are never CTAS data files.
	//   - .crc                     → Hadoop checksum sidecar
	//   - _SUCCESS / _committed_*  → job-marker files
	if strings.HasSuffix(key, ".metadata.json") ||
		strings.HasSuffix(key, ".avro") ||
		strings.HasSuffix(key, ".csv") ||
		strings.HasSuffix(key, ".crc") ||
		strings.HasSuffix(key, "/_SUCCESS") ||
		strings.Contains(key, "/_committed_") ||
		strings.Contains(key, "/_started_") {
		return false
	}
	return true
}

// parseS3URI splits an s3://bucket/key URI into (bucket, key). Result
// prefixes ending in "/" are legal and preserve the trailing slash on
// the key portion.
func parseS3URI(uri string) (bucket, key string, err error) {
	const prefix = "s3://"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", fmt.Errorf("athenaio: not an S3 URI: %q", uri)
	}
	rest := uri[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return rest, "", nil // bucket only, no key
	}
	return rest[:slash], rest[slash+1:], nil
}

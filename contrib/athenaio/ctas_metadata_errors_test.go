package athenaio

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

const rawBucketsExternal = "s3://test-bucket/raw-buckets/user-table/"

// newRawBucketsClient wires a Client whose Glue reports one table,
// "user-table", at rawBucketsExternal with numBuckets buckets, and
// whose S3 holds objects. mockCTASAthena reports 2048 scanned bytes.
func newRawBucketsClient(t *testing.T, numBuckets int32, objects map[string][]byte) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   &mockCTASAthena{pollsBeforeDone: 0},
		S3:       &mockS3{objects: objects},
		Glue: &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
			{Database: "test_db", Name: "user-table"}: {
				Name: aws.String("user-table"),
				StorageDescriptor: &gluetypes.StorageDescriptor{
					NumberOfBuckets: numBuckets,
					Location:        aws.String(rawBucketsExternal),
				},
			},
		}},
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var rawBucketsSpec = RawCTASSpec{
	SQL:              "CREATE TABLE ... AS SELECT ...",
	TableName:        "user-table",
	ExternalLocation: rawBucketsExternal,
}

// assertBilledOnError: a CTAS that completed reports its scan even
// though the call failed.
func assertBilledOnError(t *testing.T, meta CTASMetadata, err error, wantLoc string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if meta.ScannedBytes != 2048 {
		t.Errorf("meta.ScannedBytes = %d, want 2048 (CTAS completed and was billed)", meta.ScannedBytes)
	}
	if meta.QueryID == "" {
		t.Error("meta.QueryID is empty")
	}
	if meta.Location != wantLoc {
		t.Errorf("meta.Location = %q, want %q", meta.Location, wantLoc)
	}
	if meta.Duration <= 0 {
		t.Errorf("meta.Duration = %v, want > 0", meta.Duration)
	}
}

// TestCTASMetadataOnError_NotBucketed — the caller forgot
// bucketed_by: the CTAS ran and scanned, then the bucketing check
// fails before Glue location resolution.
func TestCTASMetadataOnError_NotBucketed(t *testing.T) {
	_, meta, err := newRawBucketsClient(t, 0, nil).RawCTASBucketsWithMetadata(context.Background(), rawBucketsSpec)
	assertBilledOnError(t, meta, err, "")

	_, meta, _, err = newRawBucketsClient(t, 0, nil).RawCTASBucketsManifest(context.Background(), rawBucketsSpec)
	assertBilledOnError(t, meta, err, "")
}

// TestCTASMetadataOnError_BucketReadFails — the CTAS and listing
// succeed, then a bucket file won't parse.
func TestCTASMetadataOnError_BucketReadFails(t *testing.T) {
	bad := map[string][]byte{"raw-buckets/user-table/000000_0.parquet": []byte("not parquet")}

	_, meta, err := newRawBucketsClient(t, 1, bad).RawCTASBucketsWithMetadata(context.Background(), rawBucketsSpec)
	assertBilledOnError(t, meta, err, rawBucketsExternal)

	_, meta, err = newRawBucketsClient(t, 1, bad).RawCTASWithMetadata(context.Background(), rawBucketsSpec)
	assertBilledOnError(t, meta, err, rawBucketsExternal)
}

// TestCTASMetadataOnError_PreCompletionIsZero — nothing ran, nothing
// billed: metadata stays zero.
func TestCTASMetadataOnError_PreCompletionIsZero(t *testing.T) {
	c := newRawBucketsClient(t, 1, nil)
	noSQL := rawBucketsSpec
	noSQL.SQL = ""
	if _, meta, err := c.RawCTASBucketsWithMetadata(context.Background(), noSQL); err == nil || meta != (CTASMetadata{}) {
		t.Errorf("RawCTASBucketsWithMetadata: (meta, err) = (%+v, %v), want zero + error", meta, err)
	}
	if _, meta, err := c.RawCTASWithMetadata(context.Background(), noSQL); err == nil || meta != (CTASMetadata{}) {
		t.Errorf("RawCTASWithMetadata: (meta, err) = (%+v, %v), want zero + error", meta, err)
	}
}

// TestCTASMetadata_DurationMatchesTotalTime — on eager paths the
// metadata and the Frames' QueryStats come from one stamp.
func TestCTASMetadata_DurationMatchesTotalTime(t *testing.T) {
	payload := buildMockParquet(t)
	objects := map[string][]byte{
		"raw-buckets/user-table/000000_0.parquet": payload,
		"raw-buckets/user-table/000001_0.parquet": payload,
	}

	results, meta, err := newRawBucketsClient(t, 2, objects).RawCTASBucketsWithMetadata(context.Background(), rawBucketsSpec)
	if err != nil {
		t.Fatalf("RawCTASBucketsWithMetadata: %v", err)
	}
	for i, r := range results {
		st, ok := StatsFor(r.Frame)
		if !ok {
			t.Fatalf("bucket %d: no QueryStats", i)
		}
		if st.TotalTime != meta.Duration || st.ScannedBytes != meta.ScannedBytes {
			t.Errorf("bucket %d: stats (%v, %d) != meta (%v, %d)",
				i, st.TotalTime, st.ScannedBytes, meta.Duration, meta.ScannedBytes)
		}
	}

	lf, meta, err := newRawBucketsClient(t, 2, objects).RawCTASWithMetadata(context.Background(), rawBucketsSpec)
	if err != nil {
		t.Fatalf("RawCTASWithMetadata: %v", err)
	}
	if st, _ := StatsFor(lf); st.TotalTime != meta.Duration {
		t.Errorf("RawCTAS: TotalTime %v != Duration %v", st.TotalTime, meta.Duration)
	}
}

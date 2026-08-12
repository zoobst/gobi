package athenaio

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"

	"github.com/zoobst/gobi"
)

// TestUnloadAndReadBuckets_HappyPath — CTAS happy path, 3 bucket files
// under external_location, expect 3 non-nil LazyFrames each carrying
// the same PartitionMetadata claim.
func TestUnloadAndReadBuckets_HappyPath(t *testing.T) {
	payload := buildMockParquet(t)

	mockA := &mockCTASAthena{pollsBeforeDone: 1}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}

	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results",
		Database: "test_db", ClientID: "abcd1234",
		Athena: mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql, outputLoc string) {
			tableName := extractCTASName(sql)
			location := outputLoc
			_, keyPrefix, _ := parseS3URI(location)
			// Three bucket files. Iceberg-shape names carry the bucket
			// index as the trailing zero-padded segment before ".parquet".
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00001-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00002-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name: aws.String(tableName),
				Parameters: map[string]string{
					"table_type": "ICEBERG",
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(location),
				},
			}
		},
	}
	c.athena = wrapper

	spec := UnloadSpec{
		SQL:         "SELECT id, v FROM base",
		PartitionBy: []string{"id"},
		BucketCount: 3,
		OrderBy:     []gobi.SortKey{{Column: "id"}},
		TableFormat: FormatIceberg,
	}

	results, err := c.UnloadAndReadBuckets(context.Background(), spec)
	if err != nil {
		t.Fatalf("UnloadAndReadBuckets: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results length = %d, want 3 (matches BucketCount)", len(results))
	}
	for i, lf := range results {
		if lf == nil {
			t.Errorf("bucket %d: LazyFrame is nil (no missing buckets expected)", i)
			continue
		}
		meta := lf.PartitionMetadata()
		if meta == nil {
			t.Errorf("bucket %d: PartitionMetadata missing", i)
			continue
		}
		if len(meta.Columns) != 1 || meta.Columns[0] != "id" {
			t.Errorf("bucket %d: Columns = %v, want [id]", i, meta.Columns)
		}
		if meta.HashFn != "athenaio/iceberg/murmur3-32/v1" {
			t.Errorf("bucket %d: HashFn = %q, want iceberg tag", i, meta.HashFn)
		}
		if !meta.SortEnforced {
			t.Errorf("bucket %d: SortEnforced should be true", i)
		}
		// Collect the frame — each is independently readable.
		f, err := lf.Collect()
		if err != nil {
			t.Errorf("bucket %d Collect: %v", i, err)
			continue
		}
		if rows, _ := f.Shape(); rows != 3 {
			t.Errorf("bucket %d row count = %d, want 3", i, rows)
		}
	}
}

// TestUnloadAndReadBuckets_MissingBucketAsNil — fewer S3 files than
// BucketCount. Missing bucket indices come through as nil slots.
func TestUnloadAndReadBuckets_MissingBucketAsNil(t *testing.T) {
	payload := buildMockParquet(t)

	mockA := &mockCTASAthena{pollsBeforeDone: 1}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results",
		Database: "test_db", ClientID: "abcd1234",
		Athena: mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql, outputLoc string) {
			tableName := extractCTASName(sql)
			location := outputLoc
			_, keyPrefix, _ := parseS3URI(location)
			// Only buckets 0 and 2 have data — bucket 1 is empty
			// (Athena wrote no file for it).
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00002-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name: aws.String(tableName),
				Parameters: map[string]string{
					"table_type": "ICEBERG",
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(location),
				},
			}
		},
	}
	c.athena = wrapper

	results, err := c.UnloadAndReadBuckets(context.Background(), UnloadSpec{
		SQL:         "SELECT id, v FROM base",
		PartitionBy: []string{"id"},
		BucketCount: 3,
		TableFormat: FormatIceberg,
	})
	if err != nil {
		t.Fatalf("UnloadAndReadBuckets: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results length = %d, want 3", len(results))
	}
	if results[0] == nil {
		t.Error("bucket 0 should be non-nil (has data)")
	}
	if results[1] != nil {
		t.Errorf("bucket 1 should be nil (no data), got %+v", results[1])
	}
	if results[2] == nil {
		t.Error("bucket 2 should be non-nil (has data)")
	}
}

// TestUnloadAndReadBuckets_RejectsEmptyPartitionBy — the aligned
// bucketing contract requires PartitionBy. Empty PartitionBy errors
// before any Athena work.
func TestUnloadAndReadBuckets_RejectsEmptyPartitionBy(t *testing.T) {
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/p/",
		Database: "db",
		Athena:   &mockCTASAthena{}, S3: &mockS3{}, Glue: &mockGlue{},
	})
	_, err := c.UnloadAndReadBuckets(context.Background(), UnloadSpec{
		SQL:         "SELECT * FROM t",
		BucketCount: 4,
	})
	if err == nil {
		t.Fatal("expected error for empty PartitionBy")
	}
	if !strings.Contains(err.Error(), "PartitionBy") {
		t.Errorf("error should mention PartitionBy; got %v", err)
	}
}

// TestUnloadAndReadBuckets_RejectsZeroBucketCount — BucketCount = 0
// (or missing) is rejected; per-bucket semantics require it > 0.
func TestUnloadAndReadBuckets_RejectsZeroBucketCount(t *testing.T) {
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/p/",
		Database: "db",
		Athena:   &mockCTASAthena{}, S3: &mockS3{}, Glue: &mockGlue{},
	})
	_, err := c.UnloadAndReadBuckets(context.Background(), UnloadSpec{
		SQL:         "SELECT * FROM t",
		PartitionBy: []string{"id"},
	})
	if err == nil {
		t.Fatal("expected error for zero BucketCount")
	}
	if !strings.Contains(err.Error(), "BucketCount") {
		t.Errorf("error should mention BucketCount; got %v", err)
	}
}

// TestUnloadAndReadBucketsWithMetadata_ReturnsPerBucketURIs — the
// metadata companion returns BucketResult{S3URI, Frame} pairs so
// callers can correlate per-bucket work with their S3 provenance.
func TestUnloadAndReadBucketsWithMetadata_ReturnsPerBucketURIs(t *testing.T) {
	payload := buildMockParquet(t)

	mockA := &mockCTASAthena{pollsBeforeDone: 1}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results",
		Database: "test_db", ClientID: "abcd1234",
		Athena: mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql, outputLoc string) {
			tableName := extractCTASName(sql)
			location := outputLoc
			_, keyPrefix, _ := parseS3URI(location)
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00001-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name: aws.String(tableName),
				Parameters: map[string]string{
					"table_type": "ICEBERG",
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(location),
				},
			}
		},
	}
	c.athena = wrapper

	results, err := c.UnloadAndReadBucketsWithMetadata(context.Background(), UnloadSpec{
		SQL:         "SELECT id, v FROM base",
		PartitionBy: []string{"id"},
		BucketCount: 2,
		TableFormat: FormatIceberg,
	})
	if err != nil {
		t.Fatalf("UnloadAndReadBucketsWithMetadata: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results length = %d, want 2", len(results))
	}
	for i, r := range results {
		if r.Frame == nil {
			t.Errorf("bucket %d Frame is nil", i)
			continue
		}
		if r.S3URI == "" {
			t.Errorf("bucket %d S3URI is empty", i)
		}
		if !strings.HasPrefix(r.S3URI, "s3://") {
			t.Errorf("bucket %d S3URI = %q, want s3:// prefix", i, r.S3URI)
		}
		// Size is captured from ListObjectsV2 and matches the mock's
		// stored payload length. Real S3 returns Size unconditionally
		// on ListObjectsV2 Contents entries.
		if r.Size <= 0 {
			t.Errorf("bucket %d Size = %d, want > 0", i, r.Size)
		}
		if r.Size != int64(len(payload)) {
			t.Errorf("bucket %d Size = %d, want %d (fixture payload length)",
				i, r.Size, len(payload))
		}
	}

	// Compute the caller-side average, exercising the divide-by-
	// non-nil pattern the doc-comment recommends.
	var total int64
	var populated int
	for _, r := range results {
		if r.Frame == nil {
			continue
		}
		total += r.Size
		populated++
	}
	if populated == 0 {
		t.Fatal("no populated buckets — expected at least 1")
	}
	avg := total / int64(populated)
	if avg != int64(len(payload)) {
		t.Errorf("avg bucket size = %d, want %d (identical fixture payload)",
			avg, len(payload))
	}
}

// TestRawCTASBuckets_RejectsUnbucketedTable — Glue reports
// NumberOfBuckets = 0 → RawCTASBuckets errors before returning any
// LazyFrame.
func TestRawCTASBuckets_RejectsUnbucketedTable(t *testing.T) {
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	// Glue returns a table with NumberOfBuckets = 0 → not bucketed.
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "unbucketed"}: {
			Name: aws.String("unbucketed"),
			StorageDescriptor: &gluetypes.StorageDescriptor{
				NumberOfBuckets: 0,
			},
		},
	}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   mockA, S3: &mockS3{}, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	_, err := c.RawCTASBuckets(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE ... AS SELECT ...",
		TableName:        "unbucketed",
		ExternalLocation: "s3://test-bucket/raw/unbucketed/",
	})
	if err == nil {
		t.Fatal("expected rejection for unbucketed table")
	}
	if !strings.Contains(err.Error(), "not bucketed") {
		t.Errorf("error should mention bucketing; got %v", err)
	}
}

// TestRawCTASBuckets_HappyPath — user-composed CTAS with bucketing
// clauses. Glue reports NumberOfBuckets > 0; RawCTASBuckets returns
// one LazyFrame per bucket file.
func TestRawCTASBuckets_HappyPath(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://test-bucket/raw-buckets/user-table/"

	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{
		objects: map[string][]byte{
			"raw-buckets/user-table/000000_0.parquet": payload,
			"raw-buckets/user-table/000001_0.parquet": payload,
			"raw-buckets/user-table/000002_0.parquet": payload,
		},
	}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "user-table"}: {
			Name: aws.String("user-table"),
			StorageDescriptor: &gluetypes.StorageDescriptor{
				NumberOfBuckets: 3,
				// resolveActualLocation reads this — must match
				// caller ExternalLocation (or the workgroup-override
				// warning path fires + we list an unrelated prefix).
				Location: aws.String(external),
			},
		},
	}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	userMeta := &gobi.PartitionMetadata{
		Columns: []string{"cell"},
		HashFn:  "athenaio/hive/bucket/v1",
	}
	results, err := c.RawCTASBuckets(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE ... WITH (bucketed_by = ARRAY['cell'], bucket_count = 3) AS SELECT ...",
		TableName:        "user-table",
		ExternalLocation: external,
		Metadata:         userMeta,
	})
	if err != nil {
		t.Fatalf("RawCTASBuckets: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results length = %d, want 3 (Glue NumberOfBuckets)", len(results))
	}
	for i, r := range results {
		if r.Frame == nil {
			t.Errorf("bucket %d Frame is nil", i)
			continue
		}
		meta := r.Frame.PartitionMetadata()
		if meta == nil || meta.HashFn != "athenaio/hive/bucket/v1" {
			t.Errorf("bucket %d metadata not attached from user spec: %+v", i, meta)
		}
	}
}

// TestBucketIndexFromURI_ParsesAthenaShapes — direct exercise of the
// filename → bucket-index parser. Covers Iceberg + Hive naming
// conventions and rejects non-numeric tails.
func TestBucketIndexFromURI_ParsesAthenaShapes(t *testing.T) {
	cases := []struct {
		uri  string
		want int
	}{
		{"s3://b/p/data/00000-0.parquet", 0},
		{"s3://b/p/data/00001-0.parquet", 1},
		{"s3://b/p/data/00042-0.parquet", 42},
		{"s3://b/p/000000_0.parquet", 0},
		{"s3://b/p/000123_0.parquet", 123},
		{"s3://b/p/somefile.parquet", -1}, // no trailing digits
		{"s3://b/p/no_digits", -1},
	}
	for _, tc := range cases {
		got := bucketIndexFromURI(tc.uri)
		if got != tc.want {
			t.Errorf("bucketIndexFromURI(%q) = %d, want %d", tc.uri, got, tc.want)
		}
	}
}

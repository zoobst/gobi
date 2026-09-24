package athenaio

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

// TestLongestCommonS3Prefix — unit test for the prefix helper. Covers
// the empty-input, single-URI, common-prefix, no-common-prefix, and
// degenerate `s3://` shapes the caller-visible Location value depends
// on.
func TestLongestCommonS3Prefix(t *testing.T) {
	cases := []struct {
		name string
		uris []string
		want string
	}{
		{"empty", nil, ""},
		{"single", []string{"s3://b/p/x.parquet"}, "s3://b/p/"},
		{"two-shared", []string{
			"s3://b/p/bucket_00000.parquet",
			"s3://b/p/bucket_00001.parquet",
		}, "s3://b/p/"},
		{"three-shared-deep", []string{
			"s3://b/p/data/00000-0.parquet",
			"s3://b/p/data/00001-0.parquet",
			"s3://b/p/data/00002-0.parquet",
		}, "s3://b/p/data/"},
		{"different-buckets", []string{
			"s3://a/p/x.parquet",
			"s3://b/p/y.parquet",
		}, ""},
		{"same-bucket-different-prefix", []string{
			"s3://b/one/x.parquet",
			"s3://b/two/y.parquet",
		}, "s3://b/"},
		{"scheme-only-shared", []string{
			"s3://a/x",
			"s3://b/y",
		}, ""},
		{"mid-filename-cut-avoided", []string{
			"s3://b/p/bucket_00000.parquet",
			"s3://b/p/bucket_00001.parquet",
		}, "s3://b/p/"},
		// The reviewer-flagged trap: two similar bucket names
		// (`bucket-a` vs `bucket-aa`) byte-compare to a prefix that
		// straddles the segment boundary. Segment-aware trim inside
		// the loop reduces to `s3://` before the trivial-prefix
		// guard rejects it — end result "" as expected.
		{"similar-bucket-names", []string{
			"s3://bucket-a/x.parquet",
			"s3://bucket-aa/x.parquet",
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := longestCommonS3Prefix(tc.uris)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBucketResultsFromS3URIs_HappyPath — hands the client a set of
// pre-existing S3 URIs (the shape a persisted RawCTASBucketsManifest
// would carry) and verifies the returned []BucketResult carries
// S3URI, Size, Location, and a readable LazyFrame per slot.
func TestBucketResultsFromS3URIs_HappyPath(t *testing.T) {
	payload := buildMockParquet(t)
	mockS := &mockS3{
		objects: map[string][]byte{
			"borrowed/x/000000_0.parquet": payload,
			"borrowed/x/000001_0.parquet": payload,
			"borrowed/x/000002_0.parquet": payload,
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   &mockCTASAthena{}, S3: mockS, Glue: &mockGlue{},
	})
	if err != nil {
		t.Fatal(err)
	}

	uris := []string{
		"s3://test-bucket/borrowed/x/000000_0.parquet",
		"s3://test-bucket/borrowed/x/000001_0.parquet",
		"s3://test-bucket/borrowed/x/000002_0.parquet",
	}
	results, err := c.BucketResultsFromS3URIs(context.Background(), uris, nil)
	if err != nil {
		t.Fatalf("BucketResultsFromS3URIs: %v", err)
	}
	if len(results) != len(uris) {
		t.Fatalf("results length = %d, want %d", len(results), len(uris))
	}
	wantLocation := "s3://test-bucket/borrowed/x/"
	for i, r := range results {
		if r.S3URI != uris[i] {
			t.Errorf("slot %d: S3URI = %q, want %q (listing-order slotting)", i, r.S3URI, uris[i])
		}
		if r.Frame == nil {
			t.Errorf("slot %d: Frame is nil", i)
		}
		if r.Size != int64(len(payload)) {
			t.Errorf("slot %d: Size = %d, want %d", i, r.Size, len(payload))
		}
		if r.Location != wantLocation {
			t.Errorf("slot %d: Location = %q, want %q", i, r.Location, wantLocation)
		}
	}

	// Cleanup contract: no Glue tables registered — these are borrowed
	// files, the Client doesn't own them.
	c.mu.Lock()
	trackedCount := len(c.createdTables)
	c.mu.Unlock()
	if trackedCount != 0 {
		t.Errorf("expected 0 tracked tables (borrowed files), got %d", trackedCount)
	}
}

// TestBucketResultsFromS3URIs_MixedBucketsGetEmptyLocation — URIs
// straddling different buckets share only `s3://`, which the helper
// rejects. Location comes back empty; every other field still populates.
//
// Coverage caveat: mockS3.HeadObject/GetObject key off the S3 key
// only (not bucket+key), so the two "different-bucket" URIs actually
// hit the same mock object slots by unique key. That's fine for
// exercising the Location-computation branch (which reads only the
// S3URI strings), but it does NOT exercise the read path against
// genuinely-different S3 buckets. If a bug in the S3 read layer
// treated bucket names inconsistently across concurrent URIs, this
// test wouldn't catch it — the mock would still succeed. A real-S3
// integration test is the honest coverage for that path.
func TestBucketResultsFromS3URIs_MixedBucketsGetEmptyLocation(t *testing.T) {
	payload := buildMockParquet(t)
	mockS := &mockS3{
		objects: map[string][]byte{
			"x.parquet": payload,
			"y.parquet": payload,
		},
	}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://ignored/",
		Database: "test_db",
		Athena:   &mockCTASAthena{}, S3: mockS, Glue: &mockGlue{},
	})
	uris := []string{
		"s3://a/x.parquet",
		"s3://b/y.parquet",
	}
	results, err := c.BucketResultsFromS3URIs(context.Background(), uris, nil)
	if err != nil {
		t.Fatalf("BucketResultsFromS3URIs: %v", err)
	}
	for i, r := range results {
		if r.Location != "" {
			t.Errorf("slot %d: Location = %q, want empty (mixed buckets)", i, r.Location)
		}
		if r.S3URI != uris[i] {
			t.Errorf("slot %d: S3URI = %q, want %q", i, r.S3URI, uris[i])
		}
	}
}

// TestBucketResultsFromS3URIs_HeadObjectFailureAborts — one bad URI in
// the batch aborts the whole call. Partial results would be misleading
// for the hand-off use case.
func TestBucketResultsFromS3URIs_HeadObjectFailureAborts(t *testing.T) {
	payload := buildMockParquet(t)
	mockS := &mockS3{
		objects: map[string][]byte{
			"good/x.parquet": payload,
			// "missing/y.parquet" intentionally absent — HeadObject fails.
		},
	}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/results/",
		Database: "test_db",
		Athena:   &mockCTASAthena{}, S3: mockS, Glue: &mockGlue{},
	})
	uris := []string{
		"s3://good/x.parquet",
		"s3://missing/y.parquet",
	}
	_, err := c.BucketResultsFromS3URIs(context.Background(), uris, nil)
	if err == nil {
		t.Fatal("expected error from missing URI, got nil")
	}
	if !strings.Contains(err.Error(), "BucketResultsFromS3URIs") {
		t.Errorf("error missing method name: %v", err)
	}
}

// TestRawCTASBucketsManifest_HappyPath — the two-phase RawCTAS variant.
// Phase 1 (this call) submits, polls, verifies, and lists; returns a
// Frame-nil manifest + CTASMetadata + a hydrate closure. Phase 2
// (hydrate) constructs the LazyFrames on demand.
func TestRawCTASBucketsManifest_HappyPath(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://test-bucket/manifest-test/user-table/"

	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{
		objects: map[string][]byte{
			"manifest-test/user-table/000000_0.parquet": payload,
			"manifest-test/user-table/000001_0.parquet": payload,
			"manifest-test/user-table/000002_0.parquet": payload,
		},
	}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "user-table"}: {
			Name: aws.String("user-table"),
			StorageDescriptor: &gluetypes.StorageDescriptor{
				NumberOfBuckets: 3,
				Location:        aws.String(external),
			},
		},
	}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	manifest, meta, hydrate, err := c.RawCTASBucketsManifest(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE ... WITH (bucketed_by = ARRAY['x'], bucket_count = 3) AS SELECT ...",
		TableName:        "user-table",
		ExternalLocation: external,
	})
	if err != nil {
		t.Fatalf("RawCTASBucketsManifest: %v", err)
	}

	// Manifest contract: Frame nil, S3URI + Size + Location populated.
	if len(manifest) != 3 {
		t.Fatalf("manifest length = %d, want 3", len(manifest))
	}
	for i, r := range manifest {
		if r.Frame != nil {
			t.Errorf("slot %d: Frame should be nil pre-hydrate, got %+v", i, r.Frame)
		}
		if r.Location != external {
			t.Errorf("slot %d: Location = %q, want %q", i, r.Location, external)
		}
		if r.S3URI == "" {
			t.Errorf("slot %d: S3URI is empty", i)
		}
		if r.Size <= 0 {
			t.Errorf("slot %d: Size = %d, want > 0", i, r.Size)
		}
	}

	// Metadata contract: Location, QueryID, Duration.
	if meta.Location != external {
		t.Errorf("meta.Location = %q, want %q", meta.Location, external)
	}
	if meta.QueryID == "" {
		t.Error("meta.QueryID is empty")
	}
	if meta.Duration <= 0 {
		t.Errorf("meta.Duration = %v, want > 0", meta.Duration)
	}
	if meta.ScannedBytes != 2048 {
		t.Errorf("meta.ScannedBytes = %d, want 2048 (mockCTASAthena)", meta.ScannedBytes)
	}

	// Cleanup registration still fires on the CTAS-side path.
	c.mu.Lock()
	trackedCount := len(c.createdTables)
	c.mu.Unlock()
	if trackedCount != 1 {
		t.Errorf("expected 1 tracked table (temp CTAS entry), got %d", trackedCount)
	}

	// Hydrate: LazyFrames should now be populated. Same S3URI slot
	// mapping as the manifest.
	hydrated, err := hydrate(context.Background())
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(hydrated) != len(manifest) {
		t.Fatalf("hydrated length = %d, want %d", len(hydrated), len(manifest))
	}
	for i, r := range hydrated {
		if r.Frame == nil {
			t.Errorf("hydrated slot %d: Frame is nil", i)
		}
		if r.S3URI != manifest[i].S3URI {
			t.Errorf("hydrated slot %d: S3URI = %q, want %q", i, r.S3URI, manifest[i].S3URI)
		}
	}

	// Idempotency: second hydrate returns fresh LazyFrames.
	hydrated2, err := hydrate(context.Background())
	if err != nil {
		t.Fatalf("second hydrate: %v", err)
	}
	// Same S3URIs but different LazyFrame instances.
	for i := range hydrated2 {
		if hydrated2[i].Frame == hydrated[i].Frame {
			t.Errorf("hydrated2 slot %d: expected fresh LazyFrame, got identical pointer", i)
		}
	}
}

// TestRawCTASBucketsManifest_HydrateNeverCalled — verifies the manifest
// path completes without hydrate being invoked. Downstream process
// might have shipped the manifest via BucketResultsFromS3URIs; the
// upstream Client shouldn't fault.
func TestRawCTASBucketsManifest_HydrateNeverCalled(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://test-bucket/manifest-test/nohydrate/"

	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{
		objects: map[string][]byte{
			"manifest-test/nohydrate/000000_0.parquet": payload,
		},
	}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "no-hydrate"}: {
			Name: aws.String("no-hydrate"),
			StorageDescriptor: &gluetypes.StorageDescriptor{
				NumberOfBuckets: 1,
				Location:        aws.String(external),
			},
		},
	}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	manifest, _, _, err := c.RawCTASBucketsManifest(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE ... WITH (bucket_count = 1) AS SELECT ...",
		TableName:        "no-hydrate",
		ExternalLocation: external,
	})
	if err != nil {
		t.Fatalf("RawCTASBucketsManifest: %v", err)
	}
	if len(manifest) == 0 {
		t.Fatal("manifest is empty")
	}
	// Never call hydrate. The Client should still be usable.
}

// TestUnloadAndReadBucketsManifest_HappyPath — the composed-CTAS-side
// two-phase variant. Same shape as RawCTASBucketsManifest.
func TestUnloadAndReadBucketsManifest_HappyPath(t *testing.T) {
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

	var recordedLoc string
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql, outputLoc string) {
			tableName := extractCTASName(sql)
			recordedLoc = outputLoc
			_, keyPrefix, _ := parseS3URI(outputLoc)
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00001-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name: aws.String(tableName),
				Parameters: map[string]string{
					"table_type": "ICEBERG",
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(outputLoc),
				},
			}
		},
	}
	c.athena = wrapper

	manifest, meta, hydrate, err := c.UnloadAndReadBucketsManifest(context.Background(), UnloadSpec{
		SQL:         "SELECT id, v FROM base",
		PartitionBy: []string{"id"},
		BucketCount: 2,
		TableFormat: FormatIceberg,
	})
	if err != nil {
		t.Fatalf("UnloadAndReadBucketsManifest: %v", err)
	}
	if len(manifest) != 2 {
		t.Fatalf("manifest length = %d, want 2", len(manifest))
	}
	for i, r := range manifest {
		if r.Frame != nil {
			t.Errorf("slot %d: Frame should be nil pre-hydrate", i)
		}
		if r.Location != recordedLoc {
			t.Errorf("slot %d: Location = %q, want %q", i, r.Location, recordedLoc)
		}
	}
	if meta.Location != recordedLoc {
		t.Errorf("meta.Location = %q, want %q", meta.Location, recordedLoc)
	}
	if meta.QueryID == "" {
		t.Error("meta.QueryID is empty")
	}
	if meta.ScannedBytes != 2048 {
		t.Errorf("meta.ScannedBytes = %d, want 2048 (mockCTASAthena)", meta.ScannedBytes)
	}

	hydrated, err := hydrate(context.Background())
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	// PartitionMetadata attaches on hydrate (same as
	// UnloadAndReadBuckets), so LazyFrames carry the Iceberg hash tag.
	for i, r := range hydrated {
		if r.Frame == nil {
			t.Errorf("hydrated slot %d: Frame is nil", i)
			continue
		}
		if pm := r.Frame.PartitionMetadata(); pm == nil {
			t.Errorf("hydrated slot %d: PartitionMetadata missing", i)
		} else if pm.HashFn != "athenaio/iceberg/murmur3-32/v1" {
			t.Errorf("hydrated slot %d: HashFn = %q, want iceberg tag", i, pm.HashFn)
		}
	}
}

// TestUnloadAndReadBucketsManifest_RejectsEmptyPartitionBy — the
// manifest path enforces the same input contract as
// UnloadAndReadBuckets.
func TestUnloadAndReadBucketsManifest_RejectsEmptyPartitionBy(t *testing.T) {
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/p/",
		Database: "db",
		Athena:   &mockCTASAthena{}, S3: &mockS3{}, Glue: &mockGlue{},
	})
	_, _, _, err := c.UnloadAndReadBucketsManifest(context.Background(), UnloadSpec{
		SQL:         "SELECT * FROM t",
		BucketCount: 2,
		// PartitionBy intentionally empty
	})
	if err == nil {
		t.Fatal("expected error for empty PartitionBy, got nil")
	}
	if !strings.Contains(err.Error(), "PartitionBy") {
		t.Errorf("error missing PartitionBy mention: %v", err)
	}
}

// TestManifestRoundTrip_ThroughBucketResultsFromS3URIs — the end-to-
// end hand-off: (1) RawCTASBucketsManifest returns a manifest with
// S3URIs; (2) a downstream call (simulating a different process)
// takes those URIs and reconstructs the LazyFrames via
// BucketResultsFromS3URIs. The Frames should read the same data as
// hydrate() would have.
func TestManifestRoundTrip_ThroughBucketResultsFromS3URIs(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://test-bucket/roundtrip/user-table/"

	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{
		objects: map[string][]byte{
			"roundtrip/user-table/000000_0.parquet": payload,
			"roundtrip/user-table/000001_0.parquet": payload,
		},
	}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "rt"}: {
			Name: aws.String("rt"),
			StorageDescriptor: &gluetypes.StorageDescriptor{
				NumberOfBuckets: 2,
				Location:        aws.String(external),
			},
		},
	}}
	c, _ := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})

	manifest, _, _, err := c.RawCTASBucketsManifest(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE ... WITH (bucket_count = 2) AS SELECT ...",
		TableName:        "rt",
		ExternalLocation: external,
	})
	if err != nil {
		t.Fatalf("RawCTASBucketsManifest: %v", err)
	}

	// Simulate serialization + hand-off: extract just the URIs.
	uris := make([]string, 0, len(manifest))
	for _, r := range manifest {
		if r.S3URI != "" {
			uris = append(uris, r.S3URI)
		}
	}

	// Simulate downstream process: a fresh Client + BucketResultsFromS3URIs.
	// Reuse the same mockS3 to stand in for "same S3 files are still there
	// after upstream Client exited."
	downstream, _ := NewClient(ClientConfig{
		Workgroup: "wg-downstream", ResultLocation: "s3://test-bucket/results/",
		Database: "downstream_db",
		Athena:   &mockCTASAthena{}, S3: mockS, Glue: &mockGlue{},
	})

	reconstructed, err := downstream.BucketResultsFromS3URIs(context.Background(), uris, nil)
	if err != nil {
		t.Fatalf("BucketResultsFromS3URIs: %v", err)
	}
	if len(reconstructed) != len(uris) {
		t.Fatalf("reconstructed length = %d, want %d", len(reconstructed), len(uris))
	}
	for i, r := range reconstructed {
		if r.Frame == nil {
			t.Errorf("reconstructed slot %d: Frame is nil", i)
		}
		if r.S3URI != uris[i] {
			t.Errorf("reconstructed slot %d: S3URI = %q, want %q", i, r.S3URI, uris[i])
		}
		if r.Location != external {
			t.Errorf("reconstructed slot %d: Location = %q, want %q", i, r.Location, external)
		}
	}
	// Downstream Client didn't touch Glue — no cleanup registration.
	downstream.mu.Lock()
	dsTracked := len(downstream.createdTables)
	downstream.mu.Unlock()
	if dsTracked != 0 {
		t.Errorf("downstream Client tracked %d tables, want 0 (borrowed files)", dsTracked)
	}
}

package athenaio

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

// assertNoResultFiles checks both the sentinel (errors.Is) and the
// struct (errors.As) contracts, then returns the struct for
// field-level assertions.
func assertNoResultFiles(t *testing.T, err error) *NoResultFilesError {
	t.Helper()
	if err == nil {
		t.Fatal("expected ErrNoResultFiles, got nil")
	}
	if !errors.Is(err, ErrNoResultFiles) {
		t.Fatalf("errors.Is(err, ErrNoResultFiles) = false; err = %v", err)
	}
	var nrf *NoResultFilesError
	if !errors.As(err, &nrf) {
		t.Fatalf("errors.As(err, *NoResultFilesError) = false; err = %v", err)
	}
	return nrf
}

// TestUnloadAndRead_NoResultFilesIsTyped — CTAS succeeds, Glue
// records a location, the listing comes back empty.
func TestUnloadAndRead_NoResultFilesIsTyped(t *testing.T) {
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results",
		Database: "test_db",
		Athena:   mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var recordedLoc string
	c.athena = &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql, outputLoc string) {
			recordedLoc = outputLoc
			name := extractCTASName(sql)
			mockG.tables[glueTableKey{Database: "test_db", Name: name}] = &gluetypes.Table{
				Name:              aws.String(name),
				Parameters:        map[string]string{"table_type": "ICEBERG"},
				StorageDescriptor: &gluetypes.StorageDescriptor{Location: aws.String(outputLoc)},
			}
		},
	}

	_, err = c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:         "SELECT id FROM t",
		PartitionBy: []string{"id"},
		BucketCount: 4,
	})
	nrf := assertNoResultFiles(t, err)
	if nrf.Op != "UnloadAndRead" {
		t.Errorf("Op = %q, want UnloadAndRead", nrf.Op)
	}
	if nrf.QueryID == "" {
		t.Error("QueryID is empty")
	}
	if nrf.Location != recordedLoc {
		t.Errorf("Location = %q, want %q", nrf.Location, recordedLoc)
	}
	if nrf.Table != "" {
		t.Errorf("Table = %q, want empty for a CTAS path", nrf.Table)
	}
}

// TestRawCTAS_NoResultFilesIsTyped — both RawCTAS and
// RawCTASWithMetadata surface the typed error.
func TestRawCTAS_NoResultFilesIsTyped(t *testing.T) {
	external := "s3://test-bucket/raw-empty/"
	newClient := func() *Client {
		c, err := NewClient(ClientConfig{
			Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
			Database: "test_db",
			Athena:   &mockCTASAthena{pollsBeforeDone: 0},
			S3:       &mockS3{objects: map[string][]byte{}},
			Glue: &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
				{Database: "test_db", Name: "empty"}: {
					Name:              aws.String("empty"),
					StorageDescriptor: &gluetypes.StorageDescriptor{Location: aws.String(external)},
				},
			}},
			PollInterval: 1 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	spec := RawCTASSpec{
		SQL:              "CREATE TABLE ... AS SELECT ...",
		TableName:        "empty",
		ExternalLocation: external,
	}

	_, err := newClient().RawCTAS(context.Background(), spec)
	nrf := assertNoResultFiles(t, err)
	if nrf.Op != "RawCTAS" || nrf.Location != external || nrf.QueryID == "" {
		t.Errorf("RawCTAS: got %+v", nrf)
	}

	_, _, err = newClient().RawCTASWithMetadata(context.Background(), spec)
	assertNoResultFiles(t, err)
}

// TestOpenPartitionedTable_NoResultFilesIsTyped — existing Hive
// table whose location holds no data files.
func TestOpenPartitionedTable_NoResultFilesIsTyped(t *testing.T) {
	external := "s3://user-bucket/empty-table/"
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://x/results/",
		Database: "user_db",
		Athena:   &mockCTASAthena{},
		S3:       &mockS3{objects: map[string][]byte{}},
		Glue: &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
			{Database: "user_db", Name: "dim_empty"}: {
				Name:       aws.String("dim_empty"),
				Parameters: map[string]string{},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location:        aws.String(external),
					NumberOfBuckets: 4,
					BucketColumns:   []string{"user_id"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.OpenPartitionedTable(context.Background(), "user_db", "dim_empty", nil)
	nrf := assertNoResultFiles(t, err)
	if nrf.Op != "OpenPartitionedTable" || nrf.Table != "user_db.dim_empty" || nrf.Location != external {
		t.Errorf("got %+v", nrf)
	}
	if nrf.QueryID != "" {
		t.Errorf("QueryID = %q, want empty (no query runs)", nrf.QueryID)
	}
}

// TestNoResultFilesError_MessageUnchanged — the typed error must
// render exactly the text the pre-v0.1.16 fmt.Errorf calls produced,
// so existing log searches keep matching.
func TestNoResultFilesError_MessageUnchanged(t *testing.T) {
	cases := []struct {
		err  *NoResultFilesError
		want string
	}{
		{
			&NoResultFilesError{Op: "UnloadAndRead", QueryID: "q-1", Location: "s3://b/p/"},
			"athenaio: UnloadAndRead q-1: no result files under s3://b/p/",
		},
		{
			&NoResultFilesError{Op: "RawCTAS", QueryID: "q-2", Location: "s3://b/p/"},
			"athenaio: RawCTAS q-2: no result files under s3://b/p/",
		},
		{
			&NoResultFilesError{Op: "OpenPartitionedTable", Table: "db.t", Location: "s3://b/p/"},
			"athenaio: db.t: no result files under s3://b/p/",
		},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
	// Still matches through a caller's own wrapping.
	wrapped := fmt.Errorf("handler: %w", cases[0].err)
	if !errors.Is(wrapped, ErrNoResultFiles) {
		t.Error("errors.Is lost through fmt.Errorf %w wrapping")
	}
}

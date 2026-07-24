package athenaio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// -----------------------------------------------------------------------------
// mockAthena: minimal in-memory Athena for unit tests.
//
// Records the last-submitted SQL for assertion; drives the query
// through states so the poll loop exercises its exponential-backoff
// branch at least once (Running → Succeeded). Fixed OutputLocation
// stashed into the terminal QueryExecution so RawQuery walks into the
// S3 read path.
// -----------------------------------------------------------------------------

type mockAthena struct {
	lastSQL         string
	outputURI       string
	pollsBeforeDone int32 // counter driving Running → Succeeded transition

	// Overrides for failure-path testing. If forceFailState is set,
	// the query returns that terminal state instead of Succeeded.
	forceFailState  athenatypes.QueryExecutionState
	forceFailReason string
}

func (m *mockAthena) StartQueryExecution(ctx context.Context, in *athena.StartQueryExecutionInput, opts ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	if in.QueryString == nil {
		return nil, errors.New("mock: nil QueryString")
	}
	m.lastSQL = *in.QueryString
	return &athena.StartQueryExecutionOutput{
		QueryExecutionId: aws.String("test-query-id-abc123"),
	}, nil
}

func (m *mockAthena) GetQueryExecution(ctx context.Context, in *athena.GetQueryExecutionInput, opts ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	// Progress the mock through Running → terminal to exercise the
	// poll loop's non-terminal branch.
	remaining := atomic.AddInt32(&m.pollsBeforeDone, -1)

	if remaining >= 0 {
		return &athena.GetQueryExecutionOutput{
			QueryExecution: &athenatypes.QueryExecution{
				QueryExecutionId: in.QueryExecutionId,
				Status: &athenatypes.QueryExecutionStatus{
					State: athenatypes.QueryExecutionStateRunning,
				},
			},
		}, nil
	}

	terminal := athenatypes.QueryExecutionStateSucceeded
	reason := ""
	if m.forceFailState != "" {
		terminal = m.forceFailState
		reason = m.forceFailReason
	}
	scanned := int64(1024)
	engine := int64(42)
	return &athena.GetQueryExecutionOutput{
		QueryExecution: &athenatypes.QueryExecution{
			QueryExecutionId: in.QueryExecutionId,
			Status: &athenatypes.QueryExecutionStatus{
				State:             terminal,
				StateChangeReason: aws.String(reason),
			},
			ResultConfiguration: &athenatypes.ResultConfiguration{
				OutputLocation: aws.String(m.outputURI),
			},
			Statistics: &athenatypes.QueryExecutionStatistics{
				DataScannedInBytes:          &scanned,
				EngineExecutionTimeInMillis: &engine,
			},
		},
	}, nil
}

// GetQueryResults is a stub for step-6a compatibility — the prepass
// tests define their own GetQueryResults-aware mock. Default returns
// an empty result set with no columns; tests that exercise
// ValidatePartitionCols must use a mock that populates
// ResultSetMetadata.
func (m *mockAthena) GetQueryResults(ctx context.Context, in *athena.GetQueryResultsInput, opts ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	return &athena.GetQueryResultsOutput{
		ResultSet: &athenatypes.ResultSet{
			ResultSetMetadata: &athenatypes.ResultSetMetadata{},
		},
	}, nil
}

// -----------------------------------------------------------------------------
// mockS3: serves a single in-memory Parquet payload by key. HeadObject
// returns the payload length; GetObject with a Range header serves the
// requested slice. Both are called by athenaio's s3ReaderAt.
// -----------------------------------------------------------------------------

type mockS3 struct {
	// key → payload map.
	objects map[string][]byte
	// Records keys passed to DeleteObjects — populated by
	// CleanupAll-path tests to assert on which files got reaped.
	deletedKeys []string
}

func (m *mockS3) HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	key := aws.ToString(in.Key)
	buf, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("mock: key not found: %s", key)
	}
	length := int64(len(buf))
	return &s3.HeadObjectOutput{ContentLength: &length}, nil
}

func (m *mockS3) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(in.Key)
	buf, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("mock: key not found: %s", key)
	}
	if in.Range != nil {
		start, end, err := parseRangeHeader(*in.Range, int64(len(buf)))
		if err != nil {
			return nil, err
		}
		buf = buf[start : end+1]
	}
	return &s3.GetObjectOutput{
		Body: io.NopCloser(bytes.NewReader(buf)),
	}, nil
}

// DeleteObjects removes the requested keys from the in-memory
// object store. Used by the CleanupAll path in Client.dropTable.
// Records the deleted keys on the mockS3 for cleanup-test assertions.
func (m *mockS3) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	if in.Delete != nil {
		for _, obj := range in.Delete.Objects {
			if obj.Key == nil {
				continue
			}
			delete(m.objects, *obj.Key)
			m.deletedKeys = append(m.deletedKeys, *obj.Key)
		}
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func (m *mockS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	var contents []s3types.Object
	// Deterministic order helps tests assert on file ordering.
	keys := sortedKeys(m.objects)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		k := key
		size := int64(len(m.objects[key]))
		contents = append(contents, s3types.Object{Key: &k, Size: &size})
	}
	falseTruncated := false
	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: &falseTruncated,
	}, nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mockGlue is a minimal in-memory Glue Data Catalog for tests. Only
// implements GetTable + DeleteTable — the two methods athenaio's
// GlueAPI needs.
type mockGlue struct {
	// (database, name) → Table entry.
	tables map[glueTableKey]*gluetypes.Table
	// Records last DeleteTable call for cleanup-test assertions.
	deleted []glueTableKey
}

type glueTableKey struct {
	Database string
	Name     string
}

func (m *mockGlue) GetTable(ctx context.Context, in *glue.GetTableInput, opts ...func(*glue.Options)) (*glue.GetTableOutput, error) {
	key := glueTableKey{Database: aws.ToString(in.DatabaseName), Name: aws.ToString(in.Name)}
	t, ok := m.tables[key]
	if !ok {
		return nil, &smithy.GenericAPIError{
			Code:    "EntityNotFoundException",
			Message: fmt.Sprintf("mock glue: no table %s.%s", key.Database, key.Name),
		}
	}
	return &glue.GetTableOutput{Table: t}, nil
}

func (m *mockGlue) DeleteTable(ctx context.Context, in *glue.DeleteTableInput, opts ...func(*glue.Options)) (*glue.DeleteTableOutput, error) {
	key := glueTableKey{Database: aws.ToString(in.DatabaseName), Name: aws.ToString(in.Name)}
	if _, ok := m.tables[key]; !ok {
		return nil, &smithy.GenericAPIError{
			Code:    "EntityNotFoundException",
			Message: fmt.Sprintf("mock glue: no table %s.%s", key.Database, key.Name),
		}
	}
	delete(m.tables, key)
	m.deleted = append(m.deleted, key)
	return &glue.DeleteTableOutput{}, nil
}

// parseRangeHeader implements the minimum "bytes=A-B" shape that
// s3ReaderAt emits. Returns [start, end] inclusive bounds.
func parseRangeHeader(header string, total int64) (int64, int64, error) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, fmt.Errorf("mock: bad Range header: %q", header)
	}
	parts := strings.SplitN(header[len(prefix):], "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("mock: bad Range header: %q", header)
	}
	var start, end int64
	_, err := fmt.Sscanf(parts[0], "%d", &start)
	if err != nil {
		return 0, 0, fmt.Errorf("mock: bad Range start: %w", err)
	}
	_, err = fmt.Sscanf(parts[1], "%d", &end)
	if err != nil {
		return 0, 0, fmt.Errorf("mock: bad Range end: %w", err)
	}
	if end >= total {
		end = total - 1
	}
	return start, end, nil
}

// buildMockParquet returns an in-memory Parquet payload with a small
// (id, v) Frame. Fed to mockS3 as the result of a simulated Athena
// query.
func buildMockParquet(t *testing.T) []byte {
	t.Helper()
	pool := memory.DefaultAllocator
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	vB := array.NewFloat64Builder(pool)
	defer vB.Release()
	idB.AppendValues([]int64{1, 2, 3}, nil)
	vB.AppendValues([]float64{10.5, 20.5, 30.5}, nil)
	idArr := idB.NewArray()
	defer idArr.Release()
	vArr := vB.NewArray()
	defer vArr.Release()

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(vArr.DataType(), []arrow.Array{vArr})),
	}
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	// parquetio only writes to disk today; use a temp file, then read
	// bytes back. Not a hot path — mock fixture setup.
	tmp := t.TempDir() + "/mock.parquet"
	if err := parquetio.WriteFile(f, tmp, nil); err != nil {
		t.Fatalf("mock parquet write: %v", err)
	}
	buf, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

func TestNewClient_RequiresWorkgroupAndResultLocation(t *testing.T) {
	if _, err := NewClient(ClientConfig{ResultLocation: "s3://b/p/"}); err == nil {
		t.Error("missing Workgroup should reject")
	}
	if _, err := NewClient(ClientConfig{Workgroup: "wg"}); err == nil {
		t.Error("missing ResultLocation should reject")
	}
}

func TestNewClient_AppliesDefaults(t *testing.T) {
	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://b/p/",
		Athena:         &mockAthena{},
		S3:             &mockS3{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.PollInterval != defaultPollInterval {
		t.Errorf("PollInterval = %s, want default %s", c.cfg.PollInterval, defaultPollInterval)
	}
}

func TestRawQuery_HappyPath(t *testing.T) {
	payload := buildMockParquet(t)
	mockA := &mockAthena{
		outputURI:       "s3://test-bucket/results/test-query-id-abc123.parquet",
		pollsBeforeDone: 2, // force 2 Running polls before Succeeded
	}
	mockS := &mockS3{
		objects: map[string][]byte{
			"results/test-query-id-abc123.parquet": payload,
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results/",
		Athena:         mockA,
		S3:             mockS,
		// Speed the poll loop up so the 2 Running polls don't add
		// noticeable latency.
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	lf, err := c.RawQuery(context.Background(), "SELECT id, v FROM t")
	if err != nil {
		t.Fatalf("RawQuery: %v", err)
	}
	if mockA.lastSQL != "SELECT id, v FROM t" {
		t.Errorf("lastSQL = %q, want the user SQL untouched", mockA.lastSQL)
	}

	// Collect the LazyFrame and confirm we round-tripped the mock data.
	f, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rows, cols := f.Shape(); rows != 3 || cols != 2 {
		t.Fatalf("shape = (%d, %d), want (3, 2)", rows, cols)
	}

	// QueryStats registered on the LazyFrame.
	stats, ok := StatsFor(lf)
	if !ok {
		t.Fatal("StatsFor returned !ok on the freshly-created LazyFrame")
	}
	if stats.QueryExecutionID != "test-query-id-abc123" {
		t.Errorf("QueryExecutionID = %q", stats.QueryExecutionID)
	}
	if stats.ScannedBytes != 1024 {
		t.Errorf("ScannedBytes = %d, want 1024 (from mock)", stats.ScannedBytes)
	}
	if stats.EngineTime != 42*time.Millisecond {
		t.Errorf("EngineTime = %s, want 42ms", stats.EngineTime)
	}
	if !strings.HasPrefix(stats.ResultPrefix, "s3://test-bucket/") {
		t.Errorf("ResultPrefix = %q", stats.ResultPrefix)
	}
	ClearStats(lf)
	if _, ok := StatsFor(lf); ok {
		t.Error("ClearStats should have removed the entry")
	}
}

func TestRawQuery_TerminalFailure(t *testing.T) {
	mockA := &mockAthena{
		outputURI:       "s3://test-bucket/results/test-query-id-abc123.parquet",
		pollsBeforeDone: 0,
		forceFailState:  athenatypes.QueryExecutionStateFailed,
		forceFailReason: "COLUMN_NOT_FOUND: 'zork'",
	}
	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results/",
		Athena:         mockA,
		S3:             &mockS3{},
		PollInterval:   1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RawQuery(context.Background(), "SELECT zork FROM t")
	if err == nil {
		t.Fatal("expected error on FAILED terminal state")
	}
	if !errors.Is(err, ErrQueryFailed) {
		t.Fatalf("expected ErrQueryFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "COLUMN_NOT_FOUND") {
		t.Errorf("error should surface state-change reason: %v", err)
	}
}

// mockCTASAthena drives a CTAS through Running → Succeeded and
// records the last SQL for assertion. Unlike mockAthena (which
// sets a fixed OutputLocation for T1's single-file result path),
// this mock leaves OutputLocation empty — CTAS's data files live
// under the external_location baked into the SQL, and T3 discovers
// them via ListObjectsV2 rather than parsing OutputLocation.
type mockCTASAthena struct {
	lastSQL         string
	pollsBeforeDone int32
	// If forceFailState is set, GetQueryExecution returns that
	// terminal state (with forceFailReason as StateChangeReason)
	// instead of Succeeded. Tests toggle these fields between
	// attempts via the wrapper's onStart hook to simulate the
	// Iceberg-fallback path.
	forceFailState  athenatypes.QueryExecutionState
	forceFailReason string
}

func (m *mockCTASAthena) StartQueryExecution(ctx context.Context, in *athena.StartQueryExecutionInput, opts ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	m.lastSQL = aws.ToString(in.QueryString)
	return &athena.StartQueryExecutionOutput{
		QueryExecutionId: aws.String("ctas-abc123"),
	}, nil
}

// GetQueryResults default stub. Tests exercising the prepass override
// with a wrapper (see mockCTASAthenaWithSideEffect).
func (m *mockCTASAthena) GetQueryResults(ctx context.Context, in *athena.GetQueryResultsInput, opts ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	return &athena.GetQueryResultsOutput{
		ResultSet: &athenatypes.ResultSet{
			ResultSetMetadata: &athenatypes.ResultSetMetadata{},
		},
	}, nil
}

func (m *mockCTASAthena) GetQueryExecution(ctx context.Context, in *athena.GetQueryExecutionInput, opts ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	remaining := atomic.AddInt32(&m.pollsBeforeDone, -1)
	if remaining >= 0 {
		return &athena.GetQueryExecutionOutput{
			QueryExecution: &athenatypes.QueryExecution{
				QueryExecutionId: in.QueryExecutionId,
				Status: &athenatypes.QueryExecutionStatus{
					State: athenatypes.QueryExecutionStateRunning,
				},
			},
		}, nil
	}
	terminal := athenatypes.QueryExecutionStateSucceeded
	reason := ""
	if m.forceFailState != "" {
		terminal = m.forceFailState
		reason = m.forceFailReason
	}
	scanned := int64(2048)
	engine := int64(100)
	return &athena.GetQueryExecutionOutput{
		QueryExecution: &athenatypes.QueryExecution{
			QueryExecutionId: in.QueryExecutionId,
			Status: &athenatypes.QueryExecutionStatus{
				State:             terminal,
				StateChangeReason: aws.String(reason),
			},
			ResultConfiguration: &athenatypes.ResultConfiguration{
				// Empty — CTAS doesn't write results here; the
				// external_location in the SQL owns the data files.
				OutputLocation: aws.String(""),
			},
			Statistics: &athenatypes.QueryExecutionStatistics{
				DataScannedInBytes:          &scanned,
				EngineExecutionTimeInMillis: &engine,
			},
		},
	}, nil
}

// TestUnloadAndRead_HappyPath drives an Iceberg CTAS through the
// full T3 lifecycle: compose SQL, submit via mockCTASAthena,
// verify via mockGlue (which pre-populates a matching table entry),
// list two "bucket" parquet files via mockS3, read + concat them
// via parquetio, and confirm the returned LazyFrame carries a
// PartitionMetadata claim matching the UnloadSpec.
func TestUnloadAndRead_HappyPath(t *testing.T) {
	// Two bucket files, each containing a slice of the (id, v) fixture.
	// listBucketFiles will find them via ListObjectsV2 with the
	// external_location prefix.
	payload := buildMockParquet(t)

	// External location is derived inside UnloadAndRead — we don't
	// know the exact table name up front (contains a random suffix).
	// Pre-populate S3 with a wildcard-friendly setup and have Glue's
	// GetTable succeed for whatever name gets requested.
	mockA := &mockCTASAthena{pollsBeforeDone: 1}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}

	// Client with a mockGlue that echoes back a valid Iceberg table
	// entry for any GetTable request under the test database.
	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results",
		Database:       "test_db",
		ClientID:       "abcd1234",
		Athena:         mockA,
		S3:             mockS,
		Glue:           mockG,
		PollInterval:   1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Intercept the composed SQL to discover the auto-generated
	// table name + external_location. We patch StartQueryExecution
	// to also populate mockGlue + mockS3 for the read-back phase.
	origStart := mockA.StartQueryExecution
	_ = origStart
	// Wrapper approach: install a hook on the mockA that populates
	// mockS + mockG based on the composed SQL. Easier: parse the
	// composed SQL after submission to extract table name + location,
	// then set up the mocks between submit and read-back. But polling
	// happens between those — we need mocks in place before pollUntilDone.
	//
	// Simpler: intercept in the mockAthena's StartQueryExecution.
	// Since mockCTASAthena's method is a value receiver on a pointer
	// type, we can't override it. Use a wrapper mock.
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql string) {
			tableName, location := extractCTASNameAndLocation(sql)
			// Populate S3 with bucket files under external_location.
			bucket, keyPrefix, _ := parseS3URI(location)
			_ = bucket
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00001-0.parquet"] = payload
			// Iceberg metadata file that should be skipped by
			// listBucketFiles (only *.parquet is picked up).
			mockS.objects[keyPrefix+"metadata/v1.metadata.json"] = []byte("{}")
			// Register the Glue table entry.
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
		BucketCount: 4,
		OrderBy:     []gobi.SortKey{{Column: "id"}},
		TableFormat: FormatIceberg,
	}

	lf, err := c.UnloadAndRead(context.Background(), spec)
	if err != nil {
		t.Fatalf("UnloadAndRead: %v", err)
	}

	// The composed SQL should be a CTAS wrapping the user's SELECT.
	if !strings.Contains(wrapper.inner.lastSQL, "CREATE TABLE") {
		t.Errorf("composed SQL missing CREATE TABLE:\n%s", wrapper.inner.lastSQL)
	}
	if !strings.Contains(wrapper.inner.lastSQL, "table_type = 'ICEBERG'") {
		t.Errorf("composed SQL missing Iceberg table_type:\n%s", wrapper.inner.lastSQL)
	}
	if !strings.Contains(wrapper.inner.lastSQL, "bucket(4, ") {
		t.Errorf("composed SQL missing bucket(4, ...):\n%s", wrapper.inner.lastSQL)
	}
	if !strings.Contains(wrapper.inner.lastSQL, "sorted_by = ARRAY['id']") {
		t.Errorf("composed SQL missing sorted_by:\n%s", wrapper.inner.lastSQL)
	}
	if !strings.Contains(wrapper.inner.lastSQL, "SELECT id, v FROM base") {
		t.Errorf("user SELECT body missing from composed SQL:\n%s", wrapper.inner.lastSQL)
	}

	// PartitionMetadata attached to the LazyFrame.
	meta := lf.PartitionMetadata()
	if meta == nil {
		t.Fatal("PartitionMetadata missing on returned LazyFrame")
	}
	if len(meta.Columns) != 1 || meta.Columns[0] != "id" {
		t.Errorf("Columns = %v, want [id]", meta.Columns)
	}
	if meta.HashFn != "athenaio/iceberg/murmur3-32/v1" {
		t.Errorf("HashFn = %q, want iceberg tag", meta.HashFn)
	}
	if len(meta.SortedBy) != 1 || meta.SortedBy[0].Column != "id" {
		t.Errorf("SortedBy = %v, want [{id false}]", meta.SortedBy)
	}
	if !meta.SortEnforced {
		t.Error("SortEnforced should be true for Iceberg with sorted_by")
	}

	// Data round-trip: LazyFrame → Collect → 2 concatenated payloads
	// = 6 rows total (buildMockParquet emits 3 rows per file).
	f, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rows, _ := f.Shape(); rows != 6 {
		t.Fatalf("row count = %d, want 6 (2 bucket files × 3 rows)", rows)
	}

	// Stats registered.
	stats, ok := StatsFor(lf)
	if !ok {
		t.Fatal("StatsFor missing on UnloadAndRead output")
	}
	if stats.QueryExecutionID != "ctas-abc123" {
		t.Errorf("QueryExecutionID = %q", stats.QueryExecutionID)
	}
	if stats.ScannedBytes != 2048 {
		t.Errorf("ScannedBytes = %d, want 2048", stats.ScannedBytes)
	}

	// Table registered for cleanup.
	c.mu.Lock()
	trackedCount := len(c.createdTables)
	c.mu.Unlock()
	if trackedCount != 1 {
		t.Errorf("createdTables = %d entries, want 1", trackedCount)
	}

	// Close drops it via Glue.
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(mockG.deleted) != 1 {
		t.Errorf("expected 1 DeleteTable call, got %d", len(mockG.deleted))
	}
	// Second Close is idempotent — table already gone from Glue.
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("second Close should be idempotent: %v", err)
	}
}

// mockCTASAthenaWithSideEffect wraps mockCTASAthena to run a hook on
// StartQueryExecution — used to populate the mockS3 + mockGlue with
// the CTAS's expected side effects after the SQL is composed but
// before pollUntilDone runs. prepassCols, when non-empty, is served
// by GetQueryResults as the ResultSetMetadata columns list — that's
// how prepass tests inject a fixed projection schema.
type mockCTASAthenaWithSideEffect struct {
	inner       *mockCTASAthena
	onStart     func(sql string)
	prepassCols []string
}

func (w *mockCTASAthenaWithSideEffect) StartQueryExecution(ctx context.Context, in *athena.StartQueryExecutionInput, opts ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	if w.onStart != nil {
		w.onStart(aws.ToString(in.QueryString))
	}
	return w.inner.StartQueryExecution(ctx, in, opts...)
}

func (w *mockCTASAthenaWithSideEffect) GetQueryExecution(ctx context.Context, in *athena.GetQueryExecutionInput, opts ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	return w.inner.GetQueryExecution(ctx, in, opts...)
}

// GetQueryResults delegates to a per-instance columns list when set
// (populated by the prepass tests); otherwise falls through to the
// inner mock's stub. Enables one wrapper to serve both the CTAS
// happy path and the prepass path in the same test.
func (w *mockCTASAthenaWithSideEffect) GetQueryResults(ctx context.Context, in *athena.GetQueryResultsInput, opts ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	if len(w.prepassCols) == 0 {
		return w.inner.GetQueryResults(ctx, in, opts...)
	}
	cols := make([]athenatypes.ColumnInfo, len(w.prepassCols))
	for i, name := range w.prepassCols {
		n := name
		cols[i] = athenatypes.ColumnInfo{Name: &n}
	}
	return &athena.GetQueryResultsOutput{
		ResultSet: &athenatypes.ResultSet{
			ResultSetMetadata: &athenatypes.ResultSetMetadata{
				ColumnInfo: cols,
			},
		},
	}, nil
}

// extractCTASNameAndLocation pulls the auto-generated table name +
// external_location out of a composed CTAS statement. Fragile shape-
// match — good enough for the test since athenaio's composeCTAS
// produces a stable format. Database-agnostic: matches `CREATE TABLE
// "<db>"."<table>"` for any db identifier by finding the last dot
// between double-quoted identifiers.
func extractCTASNameAndLocation(sql string) (name, location string) {
	// Table name: after `CREATE TABLE "<db>"."` and before the next
	// closing `"`. Walk past "CREATE TABLE ", the db-quoted ident,
	// the dot, and the opening quote of the table ident.
	const create = `CREATE TABLE "`
	if _, after, ok := strings.Cut(sql, create); ok {
		rest := after
		// Skip past db-name closing quote.
		if j := strings.Index(rest, `".`); j >= 0 {
			rest = rest[j+2:]
			if strings.HasPrefix(rest, `"`) {
				rest = rest[1:]
				if before, _, ok0 := strings.Cut(rest, `"`); ok0 {
					name = before
				}
			}
		}
	}
	// Location: after `location = '` up to the next `'`.
	locMarker := `location = '`
	if _, after, ok := strings.Cut(sql, locMarker); ok {
		rest := after
		if before, _, ok0 := strings.Cut(rest, `'`); ok0 {
			location = before
		}
	}
	return name, location
}

// TestUnloadAndRead_HiveHappyPath is the Hive-format counterpart to
// TestUnloadAndRead_HappyPath. Same driver machinery — user submits
// UnloadSpec with FormatHive; athenaio composes the Hive CTAS shape,
// Glue verifies it (no table_type=ICEBERG), and the returned
// LazyFrame carries a "athenaio/hive/bucket/v1" HashFn with
// SortEnforced=false (Hive's sorted_by is a hint only).
func TestUnloadAndRead_HiveHappyPath(t *testing.T) {
	payload := buildMockParquet(t)
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql string) {
			tableName, location := extractCTASNameAndLocation(sql)
			_, keyPrefix, _ := parseS3URI(location)
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			// Hive table: no `table_type=ICEBERG` parameter.
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name:       aws.String(tableName),
				Parameters: map[string]string{},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(location),
				},
			}
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results",
		Database: "test_db",
		Athena:   wrapper, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	spec := UnloadSpec{
		SQL:         "SELECT id, v FROM base",
		PartitionBy: []string{"id"},
		BucketCount: 4,
		OrderBy:     []gobi.SortKey{{Column: "id"}},
		TableFormat: FormatHive,
	}
	lf, err := c.UnloadAndRead(context.Background(), spec)
	if err != nil {
		t.Fatalf("UnloadAndRead: %v", err)
	}

	// Composed SQL is Hive shape — bucketed_by + bucket_count as
	// separate props, no table_type property.
	if !strings.Contains(wrapper.inner.lastSQL, "bucketed_by = ARRAY['id']") {
		t.Errorf("composed SQL missing Hive bucketed_by:\n%s", wrapper.inner.lastSQL)
	}
	if !strings.Contains(wrapper.inner.lastSQL, "bucket_count = 4") {
		t.Errorf("composed SQL missing bucket_count:\n%s", wrapper.inner.lastSQL)
	}
	if strings.Contains(wrapper.inner.lastSQL, "table_type = 'ICEBERG'") {
		t.Errorf("Hive path should not emit ICEBERG table_type:\n%s", wrapper.inner.lastSQL)
	}
	// ORDER BY appended so at least the physical write is sorted
	// even though sorted_by is a hint.
	if !strings.Contains(wrapper.inner.lastSQL, "ORDER BY id") {
		t.Errorf("Hive path should append ORDER BY on OrderBy spec:\n%s", wrapper.inner.lastSQL)
	}

	// PartitionMetadata carries the Hive tag + SortEnforced=false.
	meta := lf.PartitionMetadata()
	if meta == nil {
		t.Fatal("PartitionMetadata missing")
	}
	if meta.HashFn != "athenaio/hive/bucket/v1" {
		t.Errorf("HashFn = %q, want hive/bucket/v1", meta.HashFn)
	}
	if meta.SortEnforced {
		t.Error("Hive's sorted_by is a hint — SortEnforced must be false")
	}
}

// TestUnloadAndRead_IcebergToHiveFallback proves the adaptive
// fallback: the mock Athena refuses the Iceberg CTAS with an
// "Iceberg not supported" state-change reason; athenaio detects
// it, retries with Hive, and returns a Hive-metadata LazyFrame.
// The warnLog hook fires exactly once, and c.hiveFallbackOnly
// latches so a subsequent UnloadAndRead skips the Iceberg attempt.
func TestUnloadAndRead_IcebergToHiveFallback(t *testing.T) {
	payload := buildMockParquet(t)
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	var attemptCount int32
	var warnCount int
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql string) {
			atomic.AddInt32(&attemptCount, 1)
			if strings.Contains(sql, "table_type = 'ICEBERG'") {
				// Iceberg attempt: force the mock to return FAILED
				// with the engine-v2 "Iceberg not supported" reason
				// on this query's poll. The retry path in
				// UnloadAndRead will match this reason and re-submit
				// as Hive.
				mockA.forceFailState = athenatypes.QueryExecutionStateFailed
				mockA.forceFailReason = "line 3:10: Iceberg tables are not supported by engine version 2"
				return
			}
			// Hive attempt: clear the failure toggle + populate the
			// S3/Glue mocks so verifyCTASOutput finds the table.
			mockA.forceFailState = ""
			mockA.forceFailReason = ""
			tableName, location := extractCTASNameAndLocation(sql)
			_, keyPrefix, _ := parseS3URI(location)
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name:       aws.String(tableName),
				Parameters: map[string]string{},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(location),
				},
			}
		},
	}

	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results",
		Database: "test_db",
		Athena:   wrapper, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
		WarnLog: func(format string, args ...any) {
			warnCount++
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	spec := UnloadSpec{
		SQL:         "SELECT id FROM t",
		PartitionBy: []string{"id"},
		BucketCount: 4,
		// TableFormat unset = FormatUnknown → try Iceberg first,
		// fall back to Hive on the specific error.
	}
	lf, err := c.UnloadAndRead(context.Background(), spec)
	if err != nil {
		t.Fatalf("UnloadAndRead fallback: %v", err)
	}
	if atomic.LoadInt32(&attemptCount) != 2 {
		t.Errorf("expected 2 StartQueryExecution attempts (Iceberg + Hive), got %d",
			atomic.LoadInt32(&attemptCount))
	}
	if warnCount != 1 {
		t.Errorf("warnLog should fire exactly once on fallback, got %d", warnCount)
	}
	if meta := lf.PartitionMetadata(); meta == nil || meta.HashFn != "athenaio/hive/bucket/v1" {
		t.Errorf("fallback should produce Hive metadata, got %+v", meta)
	}
	if !c.getHiveFallbackOnly() {
		t.Error("hiveFallbackOnly should be latched after successful fallback")
	}

	// Second call: hiveFallbackOnly latch skips the Iceberg attempt.
	attemptBefore := atomic.LoadInt32(&attemptCount)
	_, err = c.UnloadAndRead(context.Background(), spec)
	if err != nil {
		t.Fatalf("second UnloadAndRead: %v", err)
	}
	if got := atomic.LoadInt32(&attemptCount) - attemptBefore; got != 1 {
		t.Errorf("post-latch call should do 1 attempt (Hive only), got %d", got)
	}
	if warnCount != 1 {
		t.Errorf("warnLog should fire only on the first fallback, still expected 1, got %d", warnCount)
	}
}

// TestUnloadAndRead_ForcedIcebergRejects_NoFallback confirms that
// spec.TableFormat=FormatIceberg forces Iceberg — no fallback on
// failure. Users who explicitly want Iceberg get the original error
// rather than silent conversion.
func TestUnloadAndRead_ForcedIcebergRejects_NoFallback(t *testing.T) {
	mockA := &mockCTASAthena{
		pollsBeforeDone: 0,
		forceFailState:  athenatypes.QueryExecutionStateFailed,
		forceFailReason: "Iceberg tables are not supported by engine version 2",
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/results", Database: "db",
		Athena: mockA, S3: &mockS3{}, Glue: &mockGlue{},
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:         "SELECT id FROM t",
		PartitionBy: []string{"id"},
		BucketCount: 4,
		TableFormat: FormatIceberg, // explicit — no fallback
	})
	if err == nil {
		t.Fatal("forced Iceberg should surface the workgroup error")
	}
	if c.getHiveFallbackOnly() {
		t.Error("hiveFallbackOnly must NOT latch when spec forces Iceberg")
	}
}

func TestIsIcebergNotSupportedErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Iceberg tables are not supported by engine version 2", true},
		{"line 3: table_type is not a valid property", true},
		{"'table_type' does not exist", true},
		{"NOT_SUPPORTED: Iceberg is not supported", true},
		{"SYNTAX_ERROR: unexpected token near FROM", false},
		{"AccessDeniedException: no s3:GetObject", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isIcebergNotSupportedErr(fmt.Errorf("%s", tc.msg))
		if got != tc.want {
			t.Errorf("isIcebergNotSupportedErr(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestUnloadAndRead_RejectsEmptyPartitionBy(t *testing.T) {
	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results/",
		Database:       "test_db",
		Athena:         &mockCTASAthena{},
		S3:             &mockS3{},
		Glue:           &mockGlue{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:         "SELECT id FROM t",
		BucketCount: 4,
	})
	if err == nil {
		t.Fatal("empty PartitionBy should error — use RawQuery instead")
	}
}

func TestUnloadAndRead_ReadBackVerifyFailure(t *testing.T) {
	// Glue returns a table whose Location doesn't match the CTAS
	// external_location — this simulates Athena silently rewriting
	// the location. Verify should hard-error rather than proceed
	// with a possibly-wrong PartitionMetadata claim.
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results",
		Database:       "test_db",
		Athena:         mockA,
		S3:             mockS,
		Glue:           mockG,
		PollInterval:   1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wrapper installs a Glue entry with a Location that DOES NOT
	// match the CTAS's external_location — verifyIcebergTable should
	// reject.
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql string) {
			tableName, _ := extractCTASNameAndLocation(sql)
			mockG.tables[glueTableKey{Database: "test_db", Name: tableName}] = &gluetypes.Table{
				Name: aws.String(tableName),
				Parameters: map[string]string{
					"table_type": "ICEBERG",
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					// Wrong location on purpose.
					Location: aws.String("s3://wrong-bucket/somewhere/"),
				},
			}
		},
	}
	c.athena = wrapper

	_, err = c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:         "SELECT id FROM t",
		PartitionBy: []string{"id"},
		BucketCount: 4,
	})
	if err == nil {
		t.Fatal("mismatched Location should error")
	}
	if !strings.Contains(err.Error(), "Location mismatch") {
		t.Errorf("error should name Location mismatch, got: %v", err)
	}
	// Verify path registers the orphaned table for cleanup — Close
	// must still find it and drop it via Glue.
	c.mu.Lock()
	trackedCount := len(c.createdTables)
	c.mu.Unlock()
	if trackedCount != 1 {
		t.Errorf("failed-verify path should still register table, got %d tracked", trackedCount)
	}
}

// TestRawCTAS_HappyPath drives the escape-hatch path: user provides
// full CTAS SQL + table name + external location + metadata. athenaio
// submits, polls, reads the result files, attaches the user's
// metadata claim (no verification), and returns the LazyFrame.
func TestRawCTAS_HappyPath(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://test-bucket/raw-ctas-outputs/user-table/"

	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	// User's CTAS creates a table + writes files. Pre-populate mockS3
	// under the user-provided external_location.
	mockS := &mockS3{
		objects: map[string][]byte{
			"raw-ctas-outputs/user-table/data/00000-0.parquet": payload,
		},
	}
	// mockGlue starts empty — RawCTAS doesn't verify via GetTable
	// but Close's DeleteTable still fires.
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "user-table"}: {Name: aws.String("user-table")},
	}}

	c, err := NewClient(ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results/",
		Database:       "test_db",
		Athena:         mockA,
		S3:             mockS,
		Glue:           mockG,
		PollInterval:   1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	userMeta := &gobi.PartitionMetadata{
		Columns: []string{"custom_key"},
		HashFn:  "athenaio/custom/v1", // user-invented tag
	}
	lf, err := c.RawCTAS(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE test_db.\"user-table\" WITH (format='PARQUET') AS SELECT * FROM base",
		TableName:        "user-table",
		ExternalLocation: external,
		Metadata:         userMeta,
	})
	if err != nil {
		t.Fatalf("RawCTAS: %v", err)
	}
	if got := lf.PartitionMetadata(); got == nil || got.HashFn != "athenaio/custom/v1" {
		t.Errorf("user-provided metadata not attached: %+v", got)
	}
	// Table registered for cleanup even though RawCTAS doesn't verify.
	c.mu.Lock()
	trackedCount := len(c.createdTables)
	c.mu.Unlock()
	if trackedCount != 1 {
		t.Errorf("expected 1 tracked table, got %d", trackedCount)
	}
}

func TestRawCTAS_NilMetadataStillWorks(t *testing.T) {
	payload := buildMockParquet(t)
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{
		objects: map[string][]byte{
			"raw-ctas/nometa/data.parquet": payload,
		},
	}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{
		{Database: "test_db", Name: "no-meta"}: {Name: aws.String("no-meta")},
	}}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   mockA, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	lf, err := c.RawCTAS(context.Background(), RawCTASSpec{
		SQL:              "CREATE TABLE ... AS SELECT ...",
		TableName:        "no-meta",
		ExternalLocation: "s3://test-bucket/raw-ctas/nometa/",
		Metadata:         nil,
	})
	if err != nil {
		t.Fatalf("RawCTAS with nil metadata: %v", err)
	}
	if got := lf.PartitionMetadata(); got != nil {
		t.Errorf("expected nil metadata, got %+v", got)
	}
}

func TestRawCTAS_RejectsMissingFields(t *testing.T) {
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/p/", Database: "db",
		Athena: &mockCTASAthena{}, S3: &mockS3{}, Glue: &mockGlue{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []RawCTASSpec{
		{TableName: "t", ExternalLocation: "s3://b/p/"},    // empty SQL
		{SQL: "CREATE ...", ExternalLocation: "s3://b/p/"}, // empty TableName
		{SQL: "CREATE ...", TableName: "t"},                // empty ExternalLocation
	}
	for i, spec := range cases {
		if _, err := c.RawCTAS(context.Background(), spec); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestPrepass_MissingPartitionColumn(t *testing.T) {
	// Prepass returns a projection missing the requested partition
	// column — UnloadAndRead should fail-fast with a clear error
	// before attempting the CTAS.
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	wrapper := &mockCTASAthenaWithSideEffect{
		inner:       mockA,
		prepassCols: []string{"id", "v"}, // no "region" — user asked to partition on it
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/p/", Database: "db",
		Athena: wrapper, S3: &mockS3{}, Glue: &mockGlue{},
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:                   "SELECT id, v FROM t",
		PartitionBy:           []string{"region"},
		BucketCount:           4,
		ValidatePartitionCols: true,
	})
	if err == nil {
		t.Fatal("prepass should reject missing partition column")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error should name the missing column: %v", err)
	}
}

func TestPrepass_AllColumnsPresent(t *testing.T) {
	// Prepass returns all requested partition columns — UnloadAndRead
	// proceeds through the normal CTAS path.
	payload := buildMockParquet(t)
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	wrapper := &mockCTASAthenaWithSideEffect{
		inner:       mockA,
		prepassCols: []string{"id", "v"},
		onStart: func(sql string) {
			// Only fires on CTAS submit (SQL starts with CREATE);
			// prepass SQL starts with SELECT and shouldn't populate
			// mocks.
			if !strings.HasPrefix(strings.TrimSpace(sql), "CREATE") {
				return
			}
			tableName, location := extractCTASNameAndLocation(sql)
			bucket, keyPrefix, _ := parseS3URI(location)
			_ = bucket
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "db", Name: tableName}] = &gluetypes.Table{
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
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/p", Database: "db",
		Athena: wrapper, S3: mockS, Glue: mockG,
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:                   "SELECT id, v FROM t",
		PartitionBy:           []string{"id"}, // in projection
		BucketCount:           4,
		OrderBy:               []gobi.SortKey{{Column: "id"}},
		ValidatePartitionCols: true,
	})
	if err != nil {
		t.Fatalf("prepass with matching columns should pass: %v", err)
	}
}

func TestVerifyPartitionColsPresent_CaseInsensitive(t *testing.T) {
	// Athena normalizes column names in ResultSetMetadata; the
	// prepass check must match case-insensitively so ID vs id doesn't
	// spuriously reject.
	err := verifyPartitionColsPresent([]string{"ID"}, []string{"id", "v"})
	if err != nil {
		t.Errorf("case-insensitive lookup should succeed: %v", err)
	}
	err = verifyPartitionColsPresent([]string{"missing"}, []string{"id", "v"})
	if err == nil {
		t.Error("missing column should error")
	}
}

func TestCleanupAll_DeletesS3Objects(t *testing.T) {
	// UnloadAndRead with a CleanupAll-configured Client. On Close,
	// dropTable should DROP the Glue entry AND delete the S3
	// objects under external_location.
	payload := buildMockParquet(t)
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql string) {
			tableName, location := extractCTASNameAndLocation(sql)
			_, keyPrefix, _ := parseS3URI(location)
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockS.objects[keyPrefix+"data/00001-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "db", Name: tableName}] = &gluetypes.Table{
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
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/results", Database: "db",
		Athena: wrapper, S3: mockS, Glue: mockG,
		Cleanup:      CleanupAll, // default for all UnloadAndRead calls
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:         "SELECT id FROM t",
		PartitionBy: []string{"id"},
		BucketCount: 4,
	}); err != nil {
		t.Fatalf("UnloadAndRead: %v", err)
	}
	// Confirm the S3 objects exist before Close.
	if len(mockS.objects) < 2 {
		t.Fatalf("expected at least 2 pre-Close objects, got %d", len(mockS.objects))
	}
	preCount := len(mockS.objects)

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Glue side: table dropped.
	if len(mockG.deleted) != 1 {
		t.Errorf("expected 1 DeleteTable call, got %d", len(mockG.deleted))
	}
	// S3 side: deletedKeys populated.
	if len(mockS.deletedKeys) < 2 {
		t.Errorf("expected ≥2 DeleteObjects targets, got %d", len(mockS.deletedKeys))
	}
	// The two data files under external_location should be gone;
	// only pre-existing unrelated objects (none here) remain.
	if len(mockS.objects) >= preCount {
		t.Errorf("CleanupAll should have removed S3 objects: pre=%d post=%d",
			preCount, len(mockS.objects))
	}
}

func TestCleanupCatalogOnly_LeavesS3Alone(t *testing.T) {
	// Same fixture, but Cleanup = CleanupCatalogOnly (default).
	// Close drops the Glue entry but leaves S3 files intact.
	payload := buildMockParquet(t)
	mockA := &mockCTASAthena{pollsBeforeDone: 0}
	mockS := &mockS3{objects: map[string][]byte{}}
	mockG := &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}}
	wrapper := &mockCTASAthenaWithSideEffect{
		inner: mockA,
		onStart: func(sql string) {
			tableName, location := extractCTASNameAndLocation(sql)
			_, keyPrefix, _ := parseS3URI(location)
			mockS.objects[keyPrefix+"data/00000-0.parquet"] = payload
			mockG.tables[glueTableKey{Database: "db", Name: tableName}] = &gluetypes.Table{
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
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://b/results", Database: "db",
		Athena: wrapper, S3: mockS, Glue: mockG,
		// Client default = CleanupInherit → normalized to
		// CleanupCatalogOnly in NewClient.
		PollInterval: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UnloadAndRead(context.Background(), UnloadSpec{
		SQL:         "SELECT id FROM t",
		PartitionBy: []string{"id"},
		BucketCount: 4,
	}); err != nil {
		t.Fatal(err)
	}
	preCount := len(mockS.objects)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mockS.deletedKeys) != 0 {
		t.Errorf("CleanupCatalogOnly must not delete S3 objects; got %d",
			len(mockS.deletedKeys))
	}
	if len(mockS.objects) != preCount {
		t.Errorf("S3 objects mutated under CleanupCatalogOnly: pre=%d post=%d",
			preCount, len(mockS.objects))
	}
}

// TestOpenPartitionedTable_HiveAutoDetect covers the primary case:
// a user's Airflow / dbt / whatever populated a Hive-bucketed table
// in Glue. athenaio.OpenPartitionedTable reads the Glue entry,
// derives PartitionMetadata from StorageDescriptor.BucketColumns +
// SortColumns, streams the parquet files, returns the LazyFrame.
func TestOpenPartitionedTable_HiveAutoDetect(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://user-bucket/user-etl/dim_users/"
	_, keyPrefix, _ := parseS3URI(external)

	mockS := &mockS3{
		objects: map[string][]byte{
			keyPrefix + "part-00000.parquet": payload,
			keyPrefix + "part-00001.parquet": payload,
		},
	}
	mockG := &mockGlue{
		tables: map[glueTableKey]*gluetypes.Table{
			{Database: "user_db", Name: "dim_users"}: {
				Name:       aws.String("dim_users"),
				Parameters: map[string]string{}, // not Iceberg
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location:        aws.String(external),
					NumberOfBuckets: 16,
					BucketColumns:   []string{"user_id"},
					SortColumns: []gluetypes.Order{
						{Column: aws.String("user_id"), SortOrder: 1}, // ASC
					},
				},
			},
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://x/results/",
		Database: "user_db",
		Athena:   &mockCTASAthena{}, S3: mockS, Glue: mockG,
	})
	if err != nil {
		t.Fatal(err)
	}

	lf, err := c.OpenPartitionedTable(context.Background(), "user_db", "dim_users", nil)
	if err != nil {
		t.Fatalf("OpenPartitionedTable: %v", err)
	}
	meta := lf.PartitionMetadata()
	if meta == nil {
		t.Fatal("expected inferred PartitionMetadata")
	}
	if len(meta.Columns) != 1 || meta.Columns[0] != "user_id" {
		t.Errorf("Columns = %v, want [user_id]", meta.Columns)
	}
	if meta.HashFn != "athenaio/hive/bucket/v1" {
		t.Errorf("HashFn = %q, want hive/bucket/v1", meta.HashFn)
	}
	if len(meta.SortedBy) != 1 || meta.SortedBy[0].Column != "user_id" || meta.SortedBy[0].Descending {
		t.Errorf("SortedBy wrong: %+v", meta.SortedBy)
	}
	if meta.SortEnforced {
		t.Error("Hive auto-detect must never claim SortEnforced=true — sorted_by is a hint")
	}

	// Table should NOT be tracked for cleanup — user owns it.
	c.mu.Lock()
	trackedCount := len(c.createdTables)
	c.mu.Unlock()
	if trackedCount != 0 {
		t.Errorf("OpenPartitionedTable must not track the table (user owns it), got %d tracked", trackedCount)
	}

	// Data round-trip: 2 files × 3 rows = 6.
	f, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rows, _ := f.Shape(); rows != 6 {
		t.Errorf("row count = %d, want 6", rows)
	}
}

// TestOpenPartitionedTable_UserOverride confirms opts.Metadata
// bypasses the Glue-based inference — the primary path for Iceberg
// tables until auto-detect lands.
func TestOpenPartitionedTable_UserOverride(t *testing.T) {
	payload := buildMockParquet(t)
	external := "s3://user-bucket/iceberg-tbl/"
	_, keyPrefix, _ := parseS3URI(external)

	mockS := &mockS3{objects: map[string][]byte{
		keyPrefix + "data/00000-0.parquet": payload,
	}}
	mockG := &mockGlue{
		tables: map[glueTableKey]*gluetypes.Table{
			{Database: "user_db", Name: "fact_events"}: {
				Name: aws.String("fact_events"),
				Parameters: map[string]string{
					"table_type": "ICEBERG", // auto-detect would refuse this
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String(external),
				},
			},
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://x/results/",
		Database: "user_db",
		Athena:   &mockCTASAthena{}, S3: mockS, Glue: mockG,
	})
	if err != nil {
		t.Fatal(err)
	}

	userMeta := &gobi.PartitionMetadata{
		Columns:      []string{"event_id"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []gobi.SortKey{{Column: "ts"}},
		SortEnforced: true,
	}
	lf, err := c.OpenPartitionedTable(context.Background(), "user_db", "fact_events", &OpenOptions{
		Metadata: userMeta,
	})
	if err != nil {
		t.Fatalf("OpenPartitionedTable with override: %v", err)
	}
	got := lf.PartitionMetadata()
	if got == nil || got.HashFn != "athenaio/iceberg/murmur3-32/v1" || !got.SortEnforced {
		t.Errorf("user metadata not attached faithfully: %+v", got)
	}
}

func TestOpenPartitionedTable_IcebergWithoutOverrideRejects(t *testing.T) {
	mockG := &mockGlue{
		tables: map[glueTableKey]*gluetypes.Table{
			{Database: "db", Name: "iceberg-tbl"}: {
				Name: aws.String("iceberg-tbl"),
				Parameters: map[string]string{
					"table_type": "ICEBERG",
				},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String("s3://b/p/"),
				},
			},
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://x/results/",
		Database: "db",
		Athena:   &mockCTASAthena{}, S3: &mockS3{}, Glue: mockG,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.OpenPartitionedTable(context.Background(), "db", "iceberg-tbl", nil)
	if err == nil {
		t.Fatal("Iceberg auto-detect should require an explicit Metadata override")
	}
	if !strings.Contains(err.Error(), "Iceberg") || !strings.Contains(err.Error(), "OpenOptions.Metadata") {
		t.Errorf("error should hint at the OpenOptions.Metadata workaround, got: %v", err)
	}
}

func TestOpenPartitionedTable_UnbucketedRejects(t *testing.T) {
	mockG := &mockGlue{
		tables: map[glueTableKey]*gluetypes.Table{
			{Database: "db", Name: "plain-table"}: {
				Name:       aws.String("plain-table"),
				Parameters: map[string]string{},
				StorageDescriptor: &gluetypes.StorageDescriptor{
					Location: aws.String("s3://b/p/"),
					// No BucketColumns / NumberOfBuckets — plain external
					// parquet dataset without hash bucketing.
				},
			},
		},
	}
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://x/results/",
		Database: "db",
		Athena:   &mockCTASAthena{}, S3: &mockS3{}, Glue: mockG,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.OpenPartitionedTable(context.Background(), "db", "plain-table", nil)
	if err == nil {
		t.Fatal("unbucketed table should reject (this API is for bucketed reads)")
	}
	if !strings.Contains(err.Error(), "BucketColumns") {
		t.Errorf("error should name the missing BucketColumns, got: %v", err)
	}
}

func TestOpenPartitionedTable_MissingArgs(t *testing.T) {
	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://x/", Database: "db",
		Athena: &mockCTASAthena{}, S3: &mockS3{}, Glue: &mockGlue{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Empty database.
	if _, err := c.OpenPartitionedTable(context.Background(), "", "t", nil); err == nil {
		t.Error("empty database should reject")
	}
	// Empty tableName.
	if _, err := c.OpenPartitionedTable(context.Background(), "db", "", nil); err == nil {
		t.Error("empty tableName should reject")
	}
}

func TestParseS3URI(t *testing.T) {
	cases := []struct {
		uri        string
		wantBucket string
		wantKey    string
		wantErr    bool
	}{
		{"s3://my-bucket/path/to/file.parquet", "my-bucket", "path/to/file.parquet", false},
		{"s3://bucket-only", "bucket-only", "", false},
		{"s3://b/", "b", "", false},
		{"https://s3.aws/b/k", "", "", true},
		{"", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			b, k, err := parseS3URI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b != tc.wantBucket || k != tc.wantKey {
				t.Errorf("got (%q, %q), want (%q, %q)", b, k, tc.wantBucket, tc.wantKey)
			}
		})
	}
}

package athenaio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/aws/aws-sdk-go-v2/aws"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// icebergHashTag is the HashFn string emitted on the PartitionMetadata
// claim of an Iceberg CTAS result. See the tag registry in
// contrib/athenaio/PARTITION-METADATA.md.
const icebergHashTag = "athenaio/iceberg/murmur3-32/v1"

// hiveHashTag is the HashFn string for a Hive-format CTAS. Distinct
// from icebergHashTag so AlignedWith refuses cross-format claims —
// the hash functions are genuinely different (Java hashCode-based
// vs. Murmur3-32) and shouldn't be assumed interchangeable.
const hiveHashTag = "athenaio/hive/bucket/v1"

// UnloadAndRead is the T3 entry point: submit a CTAS wrapping
// spec.SQL with partitioning + bucketing, poll to completion, verify
// the resulting Glue table matches the spec, read the parquet result
// files, and return a LazyFrame carrying a PartitionMetadata claim
// that lets gobi's alignment predicate fire for shuffle-free .Over(K)
// / partition-wise Join / repartition-skip GroupBy on the join key.
//
// Step 6a scope: Iceberg format only, catalog-only cleanup. Hive
// fallback and CleanupAll's S3 side land in step 6b.
//
// The composed table stays in the Glue catalog until Close is called
// (or the user explicitly drops it) — track the LazyFrame's
// QueryStats.QueryExecutionID + TableName for out-of-band cleanup
// after crashes.
func (c *Client) UnloadAndRead(ctx context.Context, spec UnloadSpec) (*gobi.LazyFrame, error) {
	start := time.Now()

	// Opt-in LIMIT-0 prepass: confirm partition columns exist in
	// the SELECT projection before submitting the CTAS. Adds one
	// Athena round-trip but produces a clean fail-fast error
	// rather than a nested CTAS failure that surfaces the same
	// mistake with a much less readable stack.
	if spec.ValidatePartitionCols {
		cols, err := c.runPrepass(ctx, spec.SQL)
		if err != nil {
			return nil, fmt.Errorf("athenaio: UnloadAndRead prepass: %w", err)
		}
		if err := verifyPartitionColsPresent(spec.PartitionBy, cols); err != nil {
			return nil, fmt.Errorf("athenaio: UnloadAndRead prepass: %w", err)
		}
	}

	// Format resolution + fallback loop:
	//   - FormatHive forces Hive directly (no Iceberg attempt).
	//   - FormatIceberg forces Iceberg (no Hive fallback on failure).
	//   - FormatUnknown / FormatIceberg-default: try Iceberg first
	//     unless we've already learned this workgroup rejects it
	//     (hiveFallbackOnly sticks across calls per Client).
	useHive := spec.TableFormat == FormatHive
	if !useHive && c.getHiveFallbackOnly() {
		useHive = true
	}

	composed, queryID, exec, err := c.tryCTAS(ctx, spec, useHive)
	if err != nil {
		// If Iceberg-not-supported and the caller isn't forcing
		// Iceberg, retry with Hive. Log the fallback via warnLog.
		if !useHive && spec.TableFormat != FormatIceberg && isIcebergNotSupportedErr(err) {
			if c.warnLog != nil {
				c.warnLog("athenaio: workgroup %s rejected Iceberg CTAS; falling back to Hive format", c.cfg.Workgroup)
			}
			c.setHiveFallbackOnly()
			composed, queryID, exec, err = c.tryCTAS(ctx, spec, true)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// Read-back verify via Glue. Confirms the actual table matches
	// what we asked for; hard error on mismatch (never silently
	// narrow PartitionMetadata).
	if err := c.verifyCTASOutput(ctx, composed, spec); err != nil {
		// Register the table for cleanup even though verify failed —
		// otherwise a mismatch leaves the orphan in Glue forever.
		c.registerTable(trackedTable{
			Database:         c.cfg.Database,
			Name:             composed.TableName,
			Cleanup:          c.effectiveCleanup(spec),
			Format:           composed.Format,
			ExternalLocation: composed.ExternalLocation,
		})
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s read-back verify: %w", queryID, err)
	}

	// Register the table so Client.Close drops it.
	c.registerTable(trackedTable{
		Database:         c.cfg.Database,
		Name:             composed.TableName,
		Cleanup:          c.effectiveCleanup(spec),
		Format:           composed.Format,
		ExternalLocation: composed.ExternalLocation,
	})

	// List the parquet bucket files under external_location + read
	// them via parquetio.ReadReader. Concatenate in listing order —
	// Iceberg bucketing guarantees same-K rows only appear in one
	// bucket, so the concat is contiguous by K globally when
	// sorted_by is set.
	files, err := listBucketFiles(ctx, c.s3, composed.ExternalLocation)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s: no result files under %s",
			queryID, composed.ExternalLocation)
	}
	frame, err := c.readBucketFiles(ctx, files)
	if err != nil {
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s: %w", queryID, err)
	}

	// Attach the PartitionMetadata claim. HashFn + SortEnforced
	// differ per format so the alignment predicate correctly
	// refuses cross-format claims (Iceberg's Murmur3-32 hash isn't
	// interchangeable with Hive's Java hashCode-based hash) and
	// downstream operators refuse to trust Hive's hint-only sort.
	meta := &gobi.PartitionMetadata{
		Columns:      append([]string(nil), spec.PartitionBy...),
		HashFn:       hashTagFor(composed.Format),
		SortedBy:     append([]gobi.SortKey(nil), spec.OrderBy...),
		SortEnforced: composed.Format == FormatIceberg && len(spec.OrderBy) > 0,
	}
	frame.WithPartitionMeta(meta)
	lf := frame.Lazy()

	// Wrap in a WithPartitionAssertion so the claim is carried
	// through the plan tree, not just on the root Frame. The
	// assertion narrowing rule accepts nil→any so this is always
	// valid on a fresh scanFrameNode.
	asserted, err := lf.WithPartitionAssertion(meta)
	if err != nil {
		return nil, fmt.Errorf("athenaio: attach partition assertion: %w", err)
	}

	registerStats(asserted, QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     composed.ExternalLocation,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        time.Since(start),
	})
	return asserted, nil
}

// RawCTAS submits a caller-composed CTAS statement. Escape hatch
// for advanced use cases — custom hash functions, Iceberg-specific
// properties athenaio's UnloadAndRead doesn't expose, composite
// partition transforms. Symmetric with LazyFrame.WithPartitionAssertion
// on the read side: gobi trusts the caller's Metadata claim without
// verification, and correctness is the caller's responsibility.
//
// Contract:
//
//   - spec.SQL is submitted verbatim. No wrapping, no LIMIT-0 prepass,
//     no composed outer clause. Include the full CREATE TABLE ... AS.
//   - spec.TableName + spec.ExternalLocation must match what SQL
//     actually creates — athenaio doesn't parse SQL to derive them.
//   - spec.Metadata (if non-nil) is attached to the returned
//     LazyFrame as-is. A wrong claim produces silently-wrong results
//     when downstream operators consume the alignment.
//   - The table is registered for cleanup on Client.Close, honoring
//     spec.Cleanup.
//
// Errors on submit / poll / read-back mirror UnloadAndRead's shape.
// The composed SQL is included in error messages for debuggability.
func (c *Client) RawCTAS(ctx context.Context, spec RawCTASSpec) (*gobi.LazyFrame, error) {
	if spec.SQL == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.SQL is empty")
	}
	if spec.TableName == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.TableName is required (athenaio doesn't parse SQL)")
	}
	if spec.ExternalLocation == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.ExternalLocation is required (athenaio doesn't parse SQL)")
	}
	database := spec.Database
	if database == "" {
		database = c.cfg.Database
	}
	if database == "" {
		return nil, fmt.Errorf("athenaio: RawCTAS requires Database (in spec or Client config)")
	}
	cleanup := spec.Cleanup
	if cleanup == CleanupInherit {
		cleanup = c.cfg.Cleanup
	}
	start := time.Now()

	queryID, err := c.submit(ctx, spec.SQL)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTAS submit:\n---\n%s\n---\n%w", spec.SQL, err)
	}
	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTAS %s failed:\n---\n%s\n---\n%w",
			queryID, spec.SQL, err)
	}

	// Register the table for cleanup on Close. Do this before the
	// read step so a read-side failure still lets Close reap the
	// orphan. Cleanup semantics: catalog-only by default; CleanupAll
	// also deletes S3 objects.
	c.registerTable(trackedTable{
		Database:         database,
		Name:             spec.TableName,
		Cleanup:          cleanup,
		Format:           FormatUnknown, // athenaio doesn't know for RawCTAS
		ExternalLocation: spec.ExternalLocation,
	})

	// Read the result files. No read-back verification — the user
	// asserted the metadata claim; we don't second-guess.
	files, err := listBucketFiles(ctx, c.s3, spec.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("athenaio: RawCTAS %s: no result files under %s",
			queryID, spec.ExternalLocation)
	}
	frame, err := c.readBucketFiles(ctx, files)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}

	lf := frame.Lazy()
	if spec.Metadata != nil {
		frame.WithPartitionMeta(spec.Metadata)
		asserted, err := lf.WithPartitionAssertion(spec.Metadata)
		if err != nil {
			return nil, fmt.Errorf("athenaio: attach partition assertion: %w", err)
		}
		lf = asserted
	}

	registerStats(lf, QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     spec.ExternalLocation,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        time.Since(start),
	})
	return lf, nil
}

// tryCTAS composes + submits + polls a CTAS in the given format
// (Iceberg if useHive=false, Hive if true). Returns the composed
// spec + query ID + completed execution on success, or an error
// containing the composed SQL for debuggability. Isolated as a
// helper so UnloadAndRead can call it twice (once per format) on
// the fallback path.
func (c *Client) tryCTAS(ctx context.Context, spec UnloadSpec, useHive bool) (*composedCTAS, string, *athenatypes.QueryExecution, error) {
	composed, err := c.composeCTAS(spec, useHive)
	if err != nil {
		return nil, "", nil, err
	}
	queryID, err := c.submit(ctx, composed.SQL)
	if err != nil {
		return composed, "", nil, fmt.Errorf("athenaio: UnloadAndRead submit: composed SQL=%s: %w", composed.SQL, err)
	}
	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return composed, queryID, nil, fmt.Errorf("athenaio: UnloadAndRead query %s failed:\n---\n%s\n---\n%w",
			queryID, composed.SQL, err)
	}
	return composed, queryID, exec, nil
}

// getHiveFallbackOnly returns the sticky "this workgroup rejects
// Iceberg" flag. Read under the Client mutex.
func (c *Client) getHiveFallbackOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hiveFallbackOnly
}

// setHiveFallbackOnly latches the fallback flag. Called after the
// first Iceberg-not-supported error so subsequent UnloadAndRead
// calls skip the failed Iceberg attempt.
func (c *Client) setHiveFallbackOnly() {
	c.mu.Lock()
	c.hiveFallbackOnly = true
	c.mu.Unlock()
}

// isIcebergNotSupportedErr reports whether err's message contains
// markers that suggest the workgroup rejected the Iceberg-specific
// properties in the CTAS. Fragile substring match — Athena error
// text can drift between versions — but pragmatic: users can force
// Hive with spec.TableFormat=FormatHive to bypass the detection
// entirely, and callers get the original error surfaced when the
// pattern doesn't match.
//
// Common shapes observed on engine-v2 workgroups:
//   - "Iceberg tables are not supported by engine version 2"
//   - "table_type is not a valid property"
//   - "'table_type' does not exist"
//   - "NOT_SUPPORTED: ... iceberg ..."
func isIcebergNotSupportedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// "iceberg" appears in Athena's error for the engine-v2 case
	// AND in athenaio's own composed-SQL echo (which includes
	// `table_type = 'ICEBERG'`). Distinguish by checking for
	// error-state markers alongside — "not supported" or the
	// property-doesn't-exist / not-valid shapes.
	if strings.Contains(msg, "iceberg") && strings.Contains(msg, "not supported") {
		return true
	}
	if strings.Contains(msg, "table_type") &&
		(strings.Contains(msg, "not a valid") || strings.Contains(msg, "does not exist")) {
		return true
	}
	return false
}

// hashTagFor returns the PartitionMetadata HashFn tag for a given
// resolved table format. Distinct tags per format so AlignedWith
// refuses cross-format alignment claims — Iceberg's Murmur3-32 and
// Hive's Java hashCode-based hash are genuinely different functions.
func hashTagFor(f TableFormat) string {
	if f == FormatHive {
		return hiveHashTag
	}
	return icebergHashTag
}

// effectiveCleanup returns the Cleanup setting for a specific
// UnloadAndRead call — spec.Cleanup if explicitly set (non-zero),
// otherwise the Client default. NewClient normalizes the Client-
// level CleanupInherit → CleanupCatalogOnly, so cfg.Cleanup is
// always a concrete value here.
func (c *Client) effectiveCleanup(spec UnloadSpec) Cleanup {
	if spec.Cleanup == CleanupInherit {
		return c.cfg.Cleanup
	}
	return spec.Cleanup
}

// verifyCTASOutput calls Glue GetTable to confirm the table Athena
// wrote matches the composed CTAS. Hard-errors on mismatch —
// silently narrowing PartitionMetadata would cause correctness bugs
// months later when the alignment claim doesn't reflect reality.
//
// Dispatches on composed.Format because Iceberg + Hive tables have
// different Glue-parameter shapes:
//   - Iceberg: `table_type=ICEBERG` parameter present;
//     StorageDescriptor.Location matches external_location.
//   - Hive: `table_type` absent or non-ICEBERG (matching-ICEBERG
//     would indicate the Hive fallback got fooled); Location check
//     is the same.
//
// Deeper partition-spec verification (matching bucket count / sort
// keys) is still deferred — the current checks prove Athena honored
// the location + high-level format, which is enough to prevent
// silent PartitionMetadata narrowing under the Hive fallback.
func (c *Client) verifyCTASOutput(ctx context.Context, composed *composedCTAS, _ UnloadSpec) error {
	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(c.cfg.Database),
		Name:         aws.String(composed.TableName),
	})
	if err != nil {
		return fmt.Errorf("GetTable %s.%s: %w", c.cfg.Database, composed.TableName, err)
	}
	if out.Table == nil {
		return fmt.Errorf("GetTable %s.%s: nil Table", c.cfg.Database, composed.TableName)
	}
	switch composed.Format {
	case FormatIceberg:
		return verifyIcebergTable(out.Table, composed)
	case FormatHive:
		return verifyHiveTable(out.Table, composed)
	default:
		return fmt.Errorf("athenaio: verifyCTASOutput: unknown format %v", composed.Format)
	}
}

// verifyIcebergTable checks a Glue Table entry against the CTAS spec
// athenaio submitted. Isolated for testability.
func verifyIcebergTable(t *gluetypes.Table, composed *composedCTAS) error {
	// Iceberg tables in Glue carry a `table_type=ICEBERG` parameter.
	// Confirm it — a Hive fallback would surface as an absent or
	// different value.
	if t.Parameters["table_type"] != "ICEBERG" {
		return fmt.Errorf("expected ICEBERG table_type, got %q", t.Parameters["table_type"])
	}
	if err := verifyLocation(t, composed); err != nil {
		return err
	}
	return nil
}

// verifyHiveTable checks a Glue Table entry produced by a Hive-shape
// CTAS. Different from verifyIcebergTable: Hive tables don't carry
// `table_type=ICEBERG` (an unset or different value is expected).
// The same StorageDescriptor.Location check applies. Also asserts
// `table_type != ICEBERG` — an Iceberg table masquerading as Hive
// would cause the Hive-fallback path to emit hive/bucket/v1
// metadata for what's actually Iceberg data (a wrong claim, not a
// narrowed one).
func verifyHiveTable(t *gluetypes.Table, composed *composedCTAS) error {
	if tt := t.Parameters["table_type"]; tt == "ICEBERG" {
		return fmt.Errorf("expected Hive table but Glue reports table_type=%q", tt)
	}
	return verifyLocation(t, composed)
}

// verifyLocation checks StorageDescriptor.Location matches the
// composed external_location. Shared between the Iceberg + Hive
// verification paths.
func verifyLocation(t *gluetypes.Table, composed *composedCTAS) error {
	if t.StorageDescriptor == nil || t.StorageDescriptor.Location == nil {
		return fmt.Errorf("athenaio: Glue table missing StorageDescriptor.Location")
	}
	loc := *t.StorageDescriptor.Location
	if !locationMatches(loc, composed.ExternalLocation) {
		return fmt.Errorf("athenaio: Location mismatch: got %q, expected %q", loc, composed.ExternalLocation)
	}
	return nil
}

// locationMatches compares two s3:// URIs modulo trailing slash.
func locationMatches(a, b string) bool {
	trim := func(s string) string {
		for len(s) > 0 && s[len(s)-1] == '/' {
			s = s[:len(s)-1]
		}
		return s
	}
	return trim(a) == trim(b)
}

// readBucketFiles opens each s3:// URI in files, reads it via
// parquetio.ReadReader, and concatenates the resulting Frames into
// a single-chunk output Frame. First file's schema is authoritative.
// Order preserved from files argument (typically lexicographic bucket
// order from ListObjectsV2).
//
// Uses array.Concatenate for a single-chunk output rather than
// gobi.Concat (which produces multi-chunk columns) because the
// streaming executor's frameToBatch reads only chunks[0] — a multi-
// chunk Frame reaching Collect silently drops rows past the first
// chunk.
func (c *Client) readBucketFiles(ctx context.Context, files []string) (*gobi.Frame, error) {
	frames := make([]*gobi.Frame, 0, len(files))
	for _, uri := range files {
		bucket, key, err := parseS3URI(uri)
		if err != nil {
			return nil, err
		}
		ra, size, err := newS3ReaderAt(ctx, c.s3, bucket, key)
		if err != nil {
			return nil, err
		}
		f, err := parquetio.ReadReader(ra, size, nil)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", uri, err)
		}
		frames = append(frames, f)
	}
	if len(frames) == 1 {
		return frames[0], nil
	}

	// Merge per-column single-chunk arrays across frames via
	// array.Concatenate. Schema is shared — take it from the first
	// frame.
	schema := frames[0].Schema()
	numCols := len(schema.Fields())
	pool := memory.DefaultAllocator
	outCols := make([]arrow.Column, numCols)
	for ci := range numCols {
		chunks := make([]arrow.Array, 0, len(frames))
		for _, f := range frames {
			s, err := f.ColumnAt(ci)
			if err != nil {
				return nil, fmt.Errorf("col %d: %w", ci, err)
			}
			chunks = append(chunks, s.Column().Data().Chunks()...)
		}
		combined, err := array.Concatenate(chunks, pool)
		if err != nil {
			return nil, fmt.Errorf("concat col %d: %w", ci, err)
		}
		field := schema.Field(ci)
		chunked := arrow.NewChunked(combined.DataType(), []arrow.Array{combined})
		outCols[ci] = *arrow.NewColumn(field, chunked)
		combined.Release()
	}
	return gobi.NewFrame(schema, outCols)
}

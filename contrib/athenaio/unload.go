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

	// List the parquet bucket files under the *actual* Glue-recorded
	// location — not the composed external_location, which the
	// workgroup may have silently overridden when
	// EnforceWorkGroupConfiguration=true. Concatenate in listing
	// order — Iceberg bucketing guarantees same-K rows only appear
	// in one bucket, so the concat is contiguous by K globally when
	// sorted_by is set.
	actualLoc, err := c.resolveActualLocation(ctx, c.cfg.Database, composed.TableName, composed.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndRead %s: no result files under %s",
			queryID, actualLoc)
	}
	frame, err := c.readBucketFiles(ctx, files, readOptsFromSpec(spec.Columns, spec.Predicate))
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
		RowCount:         int64(frame.NumRows()),
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

	// Steer the CTAS output via ResultConfiguration.OutputLocation
	// (the caller-provided ExternalLocation). Any external_location
	// clause the user embedded in spec.SQL still passes through
	// verbatim — athenaio doesn't parse it — but OutputLocation is
	// what a workgroup-enforced setup will honor.
	queryID, err := c.submitTo(ctx, spec.SQL, spec.ExternalLocation)
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

	// Read the result files under the *actual* Glue-recorded location.
	// See resolveActualLocation for why the composed value is unsafe
	// (workgroup override with EnforceWorkGroupConfiguration=true).
	// No read-back verification of metadata — the user asserted the
	// claim; we don't second-guess.
	actualLoc, err := c.resolveActualLocation(ctx, database, spec.TableName, spec.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTAS %s: %w", queryID, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("athenaio: RawCTAS %s: no result files under %s",
			queryID, actualLoc)
	}
	frame, err := c.readBucketFiles(ctx, files, readOptsFromSpec(spec.Columns, spec.Predicate))
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
		RowCount:         int64(frame.NumRows()),
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
	// Submit with the composed data location as OutputLocation —
	// that's the sole knob for CTAS data placement now that the
	// WITH-clause `external_location` / `location` properties are
	// gone. Workgroups with EnforceWorkGroupConfiguration=true may
	// still override this; resolveActualLocation surfaces the
	// override to callers.
	queryID, err := c.submitTo(ctx, composed.SQL, composed.ExternalLocation)
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

// verifyLocation checks StorageDescriptor.Location is present. The
// exact-match check against composed.ExternalLocation is no longer
// enforced: workgroups with EnforceWorkGroupConfiguration=true
// silently override the external_location hint and write to the
// workgroup's ResultConfiguration.OutputLocation instead — a Glue
// mismatch is expected in that mode, not a bug. Callers must
// subsequently read the ground-truth location from
// resolveActualLocation and pass it to listBucketFiles.
func verifyLocation(t *gluetypes.Table, _ *composedCTAS) error {
	if t.StorageDescriptor == nil || t.StorageDescriptor.Location == nil {
		return fmt.Errorf("athenaio: Glue table missing StorageDescriptor.Location")
	}
	return nil
}

// readGlueTableLocation returns *StorageDescriptor.Location — the
// ground truth of where Athena actually wrote CTAS output. Distinct
// from the caller-composed external_location because Athena
// workgroups with EnforceWorkGroupConfiguration=true silently
// override the external_location property and route writes to the
// workgroup's ResultConfiguration.OutputLocation. Every caller that
// needs to list result files must use this value — the composed
// value points at an empty prefix in that mode.
func (c *Client) readGlueTableLocation(ctx context.Context, database, tableName string) (string, error) {
	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(database),
		Name:         aws.String(tableName),
	})
	if err != nil {
		return "", fmt.Errorf("GetTable %s.%s: %w", database, tableName, err)
	}
	if out.Table == nil {
		return "", fmt.Errorf("GetTable %s.%s: nil Table", database, tableName)
	}
	sd := out.Table.StorageDescriptor
	if sd == nil || sd.Location == nil {
		return "", fmt.Errorf("GetTable %s.%s: missing StorageDescriptor.Location", database, tableName)
	}
	return *sd.Location, nil
}

// resolveActualLocation reads the Glue-recorded location for the
// newly-created table and warns via c.warnLog when it differs from
// the caller-composed value (the workgroup-override symptom). The
// return value is the location callers should pass to
// listBucketFiles — never the composed one.
func (c *Client) resolveActualLocation(ctx context.Context, database, tableName, requested string) (string, error) {
	actual, err := c.readGlueTableLocation(ctx, database, tableName)
	if err != nil {
		return "", err
	}
	if requested != "" && !locationMatches(actual, requested) && c.warnLog != nil {
		c.warnLog(
			"athenaio: workgroup silently overrode external_location:\n"+
				"  requested: %s\n"+
				"  actual:    %s\n"+
				"  (workgroup likely has EnforceWorkGroupConfiguration=true)",
			requested, actual)
	}
	return actual, nil
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

// openBucketFrame opens one s3:// URI, reads its parquet payload via
// parquetio.ReadReader, and returns the resulting Frame. Extracted
// so the per-bucket variants (UnloadAndReadBuckets, RawCTASBuckets)
// can call it once per bucket without duplicating the S3-read plumbing.
//
// opts is passed to parquetio.ReadReader — Columns projects, Predicate
// prunes row groups. nil means "read every column, no row-group
// pruning" (the pre-v0.3.7 behavior).
func (c *Client) openBucketFrame(ctx context.Context, uri string, opts *parquetio.ReadOptions) (*gobi.Frame, error) {
	bucket, key, err := parseS3URI(uri)
	if err != nil {
		return nil, err
	}
	ra, size, err := newS3ReaderAt(ctx, c.s3, bucket, key)
	if err != nil {
		return nil, err
	}
	f, err := parquetio.ReadReader(ra, size, opts)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", uri, err)
	}
	return f, nil
}

// readOptsFromSpec builds a *parquetio.ReadOptions carrying columns
// and predicate, or returns nil when neither is set — preserves the
// pre-existing "opts == nil ⇒ default read" contract exactly when
// callers leave both spec fields unset.
func readOptsFromSpec(columns []string, predicate gobi.Expr) *parquetio.ReadOptions {
	if len(columns) == 0 && predicate.Node() == nil {
		return nil
	}
	return &parquetio.ReadOptions{
		Columns:   columns,
		Predicate: predicate,
	}
}

// readBucketFiles opens each s3:// URI in files, reads it via
// parquetio.ReadReader, and concatenates the resulting Frames into
// a single-chunk output Frame. First file's schema is authoritative.
// Order preserved from files argument (typically lexicographic bucket
// order from ListObjectsV2).
//
// opts applies to every per-file ReadReader — spec-derived Columns
// projection and Predicate row-group pruning propagate identically
// across the bucket set.
//
// Uses array.Concatenate for a single-chunk output rather than
// gobi.Concat (which produces multi-chunk columns) because the
// streaming executor's frameToBatch reads only chunks[0] — a multi-
// chunk Frame reaching Collect silently drops rows past the first
// chunk.
func (c *Client) readBucketFiles(ctx context.Context, files []bucketFileInfo, opts *parquetio.ReadOptions) (*gobi.Frame, error) {
	frames := make([]*gobi.Frame, 0, len(files))
	for _, fi := range files {
		f, err := c.openBucketFrame(ctx, fi.URI, opts)
		if err != nil {
			for _, prev := range frames {
				prev.Release()
			}
			return nil, err
		}
		frames = append(frames, f)
	}
	if len(frames) == 1 {
		return frames[0], nil
	}
	return concatFramesSingleChunk(frames, memory.DefaultAllocator)
}

// concatFramesSingleChunk consumes frames — Releases every input Frame
// as part of building the output — and returns a single Frame whose
// per-column data is one array.Concatenate of the inputs. All input
// frames must share the same schema. First frame's schema is
// authoritative.
//
// Ownership: on return (both success AND error) every input Frame has
// been Released exactly once. Callers must not use frames after the
// call. array.Concatenate copies the data into new buffers, so the
// output Frame's arrow arrays are independent of the inputs — dropping
// the sources immediately is safe and prevents the multi-GB reader
// leak that surfaced on multi-bucket UnloadAndRead workloads.
//
// Uses single-chunk output rather than gobi.Concat (which produces
// multi-chunk columns) because the streaming executor's frameToBatch
// reads only chunks[0]; a multi-chunk Frame reaching Collect silently
// drops rows past the first chunk.
func concatFramesSingleChunk(frames []*gobi.Frame, pool memory.Allocator) (*gobi.Frame, error) {
	defer func() {
		for _, f := range frames {
			f.Release()
		}
	}()

	schema := frames[0].Schema()
	numCols := len(schema.Fields())
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
		chunked.Release()
	}
	return gobi.NewFrame(schema, outCols)
}

// -----------------------------------------------------------------------------
// Per-bucket variants: UnloadAndReadBuckets + RawCTASBuckets
//
// The mainline UnloadAndRead / RawCTAS concatenate every bucket file
// into one LazyFrame, forcing downstream callers back into a single-
// plan execution — even when CTAS bucketing was set up expressly to
// enable per-bucket parallelism. The per-bucket variants return one
// LazyFrame per bucket file so callers can dispatch parallel per-
// partition work (`errgroup.WithContext` + per-frame goroutine)
// without reimplementing the S3-list-and-read plumbing.
//
// Each returned LazyFrame carries the same PartitionMetadata claim
// as the mainline variant. Alignment holds within-bucket (same-K
// rows are contained in a single bucket, per the bucketing invariant)
// but NOT across bucket indices — bucket i on left and bucket i on
// right of two separate UnloadAndReadBuckets calls are alignment-
// compatible; bucket i on left and bucket j on right are not.
// -----------------------------------------------------------------------------

// BucketResult pairs a per-bucket LazyFrame with the S3 URI it was
// read from. Returned by the *WithMetadata variants when the caller
// wants per-bucket telemetry, logging, or correlation with Athena's
// query stats. Frame may be nil for skipped/missing bucket indices
// under strict PartitionBy+BucketCount contracts.
type BucketResult struct {
	// S3URI is the fully-qualified `s3://bucket/key` the frame was
	// read from. Empty when Frame is nil.
	S3URI string
	// Frame is the per-bucket LazyFrame. Independently readable via
	// Collect(); errors surface at Collect time on the specific
	// frame that failed (sibling frames unaffected).
	Frame *gobi.LazyFrame
	// Size is the S3 object size in bytes as reported by
	// ListObjectsV2 at read time. Zero for nil-Frame slots (no file
	// existed for that bucket). Useful for skew diagnostics and
	// downstream cost estimation without a per-file HEAD call.
	//
	// Callers computing average file size across the returned slice
	// should divide by the count of non-nil BucketResults, not by
	// len(results) — skew-empty buckets pull the average down
	// spuriously otherwise.
	Size int64
}

// UnloadAndReadBuckets is the bucket-aware variant of UnloadAndRead.
// Submits the same CTAS + verify + list flow, then returns one
// *gobi.LazyFrame per bucket file — each independently readable, each
// carrying the same PartitionMetadata claim. Callers can Collect them
// in parallel goroutines to exploit CTAS bucket parallelism directly.
//
// Contract:
//
//   - spec.PartitionBy must be non-empty and spec.BucketCount > 0.
//     Unbucketed CTAS output doesn't guarantee stable per-file
//     semantics; callers who want that should use UnloadAndRead.
//   - Returned slice length == spec.BucketCount. Empty bucket indices
//     (skew — a bucket ends up with no rows and Athena writes no
//     file) are represented by a nil slot at that index. Callers
//     iterating with `for i, lf := range results` should nil-check.
//   - Each LazyFrame's Collect() error is independent of siblings.
//     A bad Parquet file on bucket 3 doesn't invalidate bucket 4.
//   - Table cleanup lifecycle is unchanged from UnloadAndRead: one
//     Glue-catalog entry per CTAS, tracked on the Client for Close.
//
// Ordering of the returned slice matches S3's ListObjectsV2 output
// (lexicographic on key), which for Athena's bucket file naming
// puts bucket_00000 first. Callers should not assume the ordering
// carries semantic meaning beyond "bucket i in slice matches bucket
// i in a peer call with the same spec".
func (c *Client) UnloadAndReadBuckets(ctx context.Context, spec UnloadSpec) ([]*gobi.LazyFrame, error) {
	if len(spec.PartitionBy) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets requires non-empty spec.PartitionBy")
	}
	if spec.BucketCount <= 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets requires spec.BucketCount > 0")
	}
	results, err := c.unloadAndReadBucketsWithMeta(ctx, spec)
	if err != nil {
		return nil, err
	}
	out := make([]*gobi.LazyFrame, len(results))
	for i, r := range results {
		out[i] = r.Frame
	}
	return out, nil
}

// UnloadAndReadBucketsWithMetadata is the observability-friendly form
// of UnloadAndReadBuckets — returns per-bucket S3 URIs alongside the
// LazyFrames. Same contract otherwise.
func (c *Client) UnloadAndReadBucketsWithMetadata(ctx context.Context, spec UnloadSpec) ([]BucketResult, error) {
	if len(spec.PartitionBy) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBucketsWithMetadata requires non-empty spec.PartitionBy")
	}
	if spec.BucketCount <= 0 {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBucketsWithMetadata requires spec.BucketCount > 0")
	}
	return c.unloadAndReadBucketsWithMeta(ctx, spec)
}

// unloadAndReadBucketsWithMeta is the shared implementation. Runs
// the full UnloadAndRead composition + submit + verify flow, then
// per-file constructs a LazyFrame with the same PartitionMetadata
// claim as the mainline variant. Missing bucket indices become nil
// slots.
func (c *Client) unloadAndReadBucketsWithMeta(ctx context.Context, spec UnloadSpec) ([]BucketResult, error) {
	start := time.Now()
	if spec.ValidatePartitionCols {
		cols, err := c.runPrepass(ctx, spec.SQL)
		if err != nil {
			return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets prepass: %w", err)
		}
		if err := verifyPartitionColsPresent(spec.PartitionBy, cols); err != nil {
			return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets prepass: %w", err)
		}
	}

	useHive := spec.TableFormat == FormatHive
	if !useHive && c.getHiveFallbackOnly() {
		useHive = true
	}
	composed, queryID, exec, err := c.tryCTAS(ctx, spec, useHive)
	if err != nil {
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

	if err := c.verifyCTASOutput(ctx, composed, spec); err != nil {
		c.registerTable(trackedTable{
			Database:         c.cfg.Database,
			Name:             composed.TableName,
			Cleanup:          c.effectiveCleanup(spec),
			Format:           composed.Format,
			ExternalLocation: composed.ExternalLocation,
		})
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets %s read-back verify: %w", queryID, err)
	}
	c.registerTable(trackedTable{
		Database:         c.cfg.Database,
		Name:             composed.TableName,
		Cleanup:          c.effectiveCleanup(spec),
		Format:           composed.Format,
		ExternalLocation: composed.ExternalLocation,
	})

	// List files under the actual Glue-recorded location — see
	// resolveActualLocation for the workgroup-override rationale.
	actualLoc, err := c.resolveActualLocation(ctx, c.cfg.Database, composed.TableName, composed.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, err
	}
	// Empty file set is a legitimate outcome — the CTAS succeeded
	// (verifyCTASOutput above passed) and the SELECT produced zero
	// rows. Callers on small AOIs / narrow time windows hit this
	// path when their input has genuinely no matching data. Return
	// a BucketCount-length slice of nil-Frame results (same shape as
	// per-bucket-empty at line 836 below) so caller code that
	// iterates buckets stays uniform between "some buckets empty"
	// and "all buckets empty."

	meta := &gobi.PartitionMetadata{
		Columns:      append([]string(nil), spec.PartitionBy...),
		HashFn:       hashTagFor(composed.Format),
		SortedBy:     append([]gobi.SortKey(nil), spec.OrderBy...),
		SortEnforced: composed.Format == FormatIceberg && len(spec.OrderBy) > 0,
	}

	// Build per-file LazyFrames. Missing bucket indices — where a
	// bucket produced zero rows and Athena wrote no file — become
	// nil slots so len(result) == BucketCount and index i maps to
	// bucket i consistently across peer calls.
	results := make([]BucketResult, spec.BucketCount)
	totalRows, err := c.populateBucketResults(ctx, files, results, meta, readOptsFromSpec(spec.Columns, spec.Predicate))
	if err != nil {
		return nil, fmt.Errorf("athenaio: UnloadAndReadBuckets %s: %w", queryID, err)
	}

	// Register stats on every non-nil frame so per-bucket callers
	// can look them up individually. RowCount is the CTAS-wide total
	// (same value on every bucket's stats blob) — per-bucket sizes
	// are recoverable via Frame.NumRows() after Collect.
	stats := QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     composed.ExternalLocation,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        time.Since(start),
		RowCount:         totalRows,
	}
	for _, r := range results {
		if r.Frame != nil {
			registerStats(r.Frame, stats)
		}
	}
	return results, nil
}

// RawCTASBuckets is the bucket-aware variant of RawCTAS. Submits the
// caller-composed CTAS SQL (which must already encode bucketing in
// the DDL), verifies via Glue that the resulting table is bucketed
// with `bucket_count > 0`, then returns one LazyFrame per bucket file.
//
// Contract:
//
//   - spec.SQL must contain bucketing DDL. athenaio verifies the
//     Glue table's post-create bucket_count > 0; failure surfaces
//     BEFORE any LazyFrame is constructed. Callers who want to
//     bypass the check should use RawCTAS.
//   - spec.Metadata (if set) is attached to each returned LazyFrame
//     via WithPartitionAssertion — same claim per-bucket, matching
//     the mainline RawCTAS shape.
//   - Otherwise identical semantics to UnloadAndReadBuckets: per-file
//     LazyFrames, nil slots for empty buckets when possible,
//     independent Collect() errors.
func (c *Client) RawCTASBuckets(ctx context.Context, spec RawCTASSpec) ([]BucketResult, error) {
	if spec.SQL == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.SQL is empty")
	}
	if spec.TableName == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.TableName is required")
	}
	if spec.ExternalLocation == "" {
		return nil, fmt.Errorf("athenaio: RawCTASSpec.ExternalLocation is required")
	}
	database := spec.Database
	if database == "" {
		database = c.cfg.Database
	}
	if database == "" {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets requires Database (in spec or Client config)")
	}
	cleanup := spec.Cleanup
	if cleanup == CleanupInherit {
		cleanup = c.cfg.Cleanup
	}
	start := time.Now()

	queryID, err := c.submitTo(ctx, spec.SQL, spec.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets submit:\n---\n%s\n---\n%w", spec.SQL, err)
	}
	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s failed:\n---\n%s\n---\n%w",
			queryID, spec.SQL, err)
	}

	// Register the table for cleanup before the bucketing verification —
	// otherwise an unbucketed table would be orphaned in Glue when we
	// error out below.
	c.registerTable(trackedTable{
		Database:         database,
		Name:             spec.TableName,
		Cleanup:          cleanup,
		Format:           FormatUnknown,
		ExternalLocation: spec.ExternalLocation,
	})

	// Verify the table is actually bucketed. RawCTAS-side we don't
	// compose the SQL so we can't know the bucket count without
	// asking Glue. This check catches "caller forgot the bucketed_by
	// clause" before any LazyFrame is handed back.
	bucketCount, err := c.readGlueBucketCount(ctx, database, spec.TableName)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: verify bucketing: %w", queryID, err)
	}
	if bucketCount <= 0 {
		return nil, fmt.Errorf(
			"athenaio: RawCTASBuckets %s: table %s.%s is not bucketed (bucket_count=%d) — use RawCTAS for non-bucketed output",
			queryID, database, spec.TableName, bucketCount)
	}

	// Resolve to the actual Glue-recorded location before listing —
	// see resolveActualLocation for the workgroup-override rationale.
	actualLoc, err := c.resolveActualLocation(ctx, database, spec.TableName, spec.ExternalLocation)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: %w", queryID, err)
	}
	files, err := listBucketFiles(ctx, c.s3, actualLoc)
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: %w", queryID, err)
	}
	// Empty file set is a legitimate outcome — the CTAS succeeded
	// (readGlueBucketCount above confirmed bucket_count > 0) and
	// the caller-provided SELECT produced zero rows. Return a
	// bucketCount-length slice of nil-Frame results so caller code
	// iterating buckets stays uniform between partially-empty and
	// fully-empty results. Matches the UnloadAndReadBuckets shape.

	results := make([]BucketResult, bucketCount)
	totalRows, err := c.populateBucketResults(ctx, files, results, spec.Metadata, readOptsFromSpec(spec.Columns, spec.Predicate))
	if err != nil {
		return nil, fmt.Errorf("athenaio: RawCTASBuckets %s: %w", queryID, err)
	}
	stats := QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     spec.ExternalLocation,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        time.Since(start),
		RowCount:         totalRows,
	}
	for _, r := range results {
		if r.Frame != nil {
			registerStats(r.Frame, stats)
		}
	}
	return results, nil
}

// populateBucketResults reads each file into a Frame, wraps it in a
// LazyFrame, optionally attaches PartitionMetadata, and installs it
// at the appropriate slot in results. Returns the total row count
// across all bucket files (sum of frame.NumRows() at open time —
// derived from the parquet footer, no data-page cost beyond the
// full read already happening for the LazyFrame wrap).
//
// opts propagates to every per-bucket parquetio.ReadReader call —
// column projection + predicate row-group pruning derived from the
// caller's spec.
//
// Slotting: file path suffix `bucket_NNNNN` (or Athena's naming
// variant) is parsed to extract the bucket index; if parsing fails
// (RawCTAS output without a matching name), files fill slots in
// listing order. Missing bucket indices stay nil.
func (c *Client) populateBucketResults(ctx context.Context, files []bucketFileInfo, results []BucketResult, meta *gobi.PartitionMetadata, opts *parquetio.ReadOptions) (int64, error) {
	nSlots := len(results)
	var totalRows int64
	for i, fi := range files {
		frame, err := c.openBucketFrame(ctx, fi.URI, opts)
		if err != nil {
			return 0, err
		}
		totalRows += int64(frame.NumRows())
		if meta != nil {
			frame.WithPartitionMeta(meta)
		}
		lf := frame.Lazy()
		if meta != nil {
			asserted, err := lf.WithPartitionAssertion(meta)
			if err != nil {
				return 0, fmt.Errorf("attach partition assertion for %s: %w", fi.URI, err)
			}
			lf = asserted
		}

		// Prefer parsed bucket index; fall back to listing order for
		// non-standard names. Out-of-range indices fall back too.
		slot := bucketIndexFromURI(fi.URI)
		if slot < 0 || slot >= nSlots {
			slot = i
			if slot >= nSlots {
				// More files than expected buckets — should not happen
				// with a valid bucket_count check, but stay defensive.
				return 0, fmt.Errorf("athenaio: file %s exceeds expected bucket range [0,%d)", fi.URI, nSlots)
			}
		}
		if results[slot].Frame != nil {
			// Two files claim the same slot — surface rather than
			// silently overwrite. Only fires on Athena writer bugs
			// or naming collisions.
			return 0, fmt.Errorf("athenaio: duplicate bucket slot %d: %s and %s",
				slot, results[slot].S3URI, fi.URI)
		}
		results[slot] = BucketResult{S3URI: fi.URI, Frame: lf, Size: fi.Size}
	}
	return totalRows, nil
}

// bucketIndexFromURI extracts the bucket index from an Athena-shaped
// output filename. Athena writes bucketed CTAS output with the bucket
// index as a leading zero-padded numeric segment in the basename:
//
//   - Iceberg: `<external_location>/data/NNNNN-<part>-<uuid>.parquet`
//     (leading `NNNNN` = bucket index).
//   - Hive:    `<external_location>/NNNNNN_M.parquet`
//     (leading `NNNNNN` = bucket index, `M` = write-attempt suffix).
//
// Returns -1 when the basename doesn't start with digits.
func bucketIndexFromURI(uri string) int {
	// Take the basename (portion after the last '/').
	base := uri
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// Walk forward, collect leading digits.
	i := 0
	for i < len(base) && base[i] >= '0' && base[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1
	}
	n := 0
	for _, d := range base[:i] {
		n = n*10 + int(d-'0')
	}
	return n
}

// readGlueBucketCount asks Glue for the bucket count on a table
// produced by a Hive-style CTAS. StorageDescriptor.NumberOfBuckets
// carries the count; 0 or absent means the table is not bucketed.
// Used by RawCTASBuckets to catch "caller forgot bucketed_by" before
// handing back LazyFrames.
func (c *Client) readGlueBucketCount(ctx context.Context, database, tableName string) (int, error) {
	out, err := c.glue.GetTable(ctx, &glue.GetTableInput{
		DatabaseName: aws.String(database),
		Name:         aws.String(tableName),
	})
	if err != nil {
		return 0, fmt.Errorf("GetTable %s.%s: %w", database, tableName, err)
	}
	if out.Table == nil {
		return 0, fmt.Errorf("GetTable %s.%s: nil Table", database, tableName)
	}
	sd := out.Table.StorageDescriptor
	if sd == nil {
		return 0, nil
	}
	return int(sd.NumberOfBuckets), nil
}

package athenaio

import (
	"fmt"
	"strings"
	"time"

	"github.com/zoobst/gobi"
)

// composedCTAS is the output of composeCTAS: the full SQL to submit
// to Athena plus the metadata athenaio needs to track the table
// created by that SQL.
type composedCTAS struct {
	// SQL is the composed CREATE TABLE ... WITH (...) AS <body>
	// statement, ready to hand to StartQueryExecution.
	SQL string

	// TableName is the auto-generated table name that CTAS creates.
	// Namespaced gobi_athenaio_<hex>_<epoch> so an out-of-band sweep
	// can identify + reap orphans.
	TableName string

	// ExternalLocation is the s3://... URI where CTAS writes the
	// parquet data files. Derived from the Client's ResultLocation
	// plus a query-scoped subdirectory.
	ExternalLocation string

	// Format is the resolved table format (never FormatUnknown here —
	// composeCTAS normalizes before returning).
	Format TableFormat
}

// composeCTAS validates spec + dispatches to the format-specific
// composer. Iceberg is preferred where the workgroup supports it;
// Hive is the fallback for engine-v2 workgroups. The resolved
// format is stamped on the returned composedCTAS so downstream code
// (PartitionMetadata emission, verifyCTASOutput) knows which shape
// it's dealing with.
//
// Format resolution:
//
//   - spec.TableFormat == FormatIceberg — force Iceberg. Fail if the
//     workgroup doesn't support it.
//   - spec.TableFormat == FormatHive — force Hive. Skip the
//     Iceberg-first probe.
//   - spec.TableFormat == FormatUnknown — try Iceberg first via
//     UnloadAndRead's outer retry loop, fall back to Hive on
//     Iceberg-not-supported errors. composeCTAS just handles the
//     force paths; the retry logic sits in UnloadAndRead.
//
// Callers who want the retry semantics pass an explicit `useHive`
// flag rather than depending on spec.TableFormat alone — this
// keeps composeCTAS a pure function and the fallback state on the
// Client.
func (c *Client) composeCTAS(spec UnloadSpec, useHive bool) (*composedCTAS, error) {
	if strings.TrimSpace(spec.SQL) == "" {
		return nil, fmt.Errorf("athenaio: UnloadSpec.SQL is empty")
	}
	if len(spec.PartitionBy) == 0 {
		return nil, fmt.Errorf("athenaio: UnloadSpec.PartitionBy is required for T3 (partition-aware) writes; use RawQuery for unpartitioned")
	}
	if spec.BucketCount <= 0 {
		return nil, fmt.Errorf("athenaio: UnloadSpec.BucketCount must be > 0 when PartitionBy is set")
	}
	if c.cfg.Database == "" {
		return nil, fmt.Errorf("athenaio: UnloadAndRead requires ClientConfig.Database (Glue table lookups need it)")
	}

	format := FormatIceberg
	if useHive {
		format = FormatHive
	}

	tableName, externalLoc, err := c.newTableIdentity()
	if err != nil {
		return nil, err
	}

	var sql string
	switch format {
	case FormatIceberg:
		sql = composeIcebergSQL(c.cfg.Database, tableName, externalLoc, spec)
	case FormatHive:
		sql = composeHiveSQL(c.cfg.Database, tableName, externalLoc, spec)
	default:
		return nil, fmt.Errorf("athenaio: unknown table format %v", format)
	}

	return &composedCTAS{
		SQL:              sql,
		TableName:        tableName,
		ExternalLocation: externalLoc,
		Format:           format,
	}, nil
}

// newTableIdentity generates the namespaced table name +
// external_location for one CTAS. Namespaced
// gobi_athenaio_<clientID>_<unix>_<hex4> so an out-of-band Glue
// sweep can identify orphans + concurrent Clients don't collide on
// same-second timestamps.
func (c *Client) newTableIdentity() (tableName, externalLoc string, err error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", "", fmt.Errorf("athenaio: composeCTAS: %w", err)
	}
	tableName = fmt.Sprintf("gobi_athenaio_%s_%d_%s",
		c.cfg.ClientID, time.Now().Unix(), suffix)
	base := strings.TrimSuffix(c.cfg.ResultLocation, "/")
	externalLoc = fmt.Sprintf("%s/%s/", base, tableName)
	return tableName, externalLoc, nil
}

// composeIcebergSQL renders the Iceberg CTAS. Uses Iceberg-specific
// properties (`table_type = 'ICEBERG'`, `partitioning = ARRAY['bucket(N, K)']`,
// `sorted_by = ARRAY['col ASC/DESC']`). Requires Athena engine v3.
func composeIcebergSQL(database, tableName, externalLoc string, spec UnloadSpec) string {
	withParts := []string{
		"table_type = 'ICEBERG'",
		"format = 'PARQUET'",
		fmt.Sprintf("location = '%s'", externalLoc),
		fmt.Sprintf("partitioning = ARRAY[%s]", icebergBucketExpr(spec.PartitionBy, spec.BucketCount)),
	}
	if len(spec.OrderBy) > 0 {
		withParts = append(withParts,
			fmt.Sprintf("sorted_by = ARRAY[%s]", icebergSortedBy(spec.OrderBy)))
	}
	return fmt.Sprintf("CREATE TABLE %s.%s\nWITH (\n  %s\n) AS\n%s",
		quoteIdent(database),
		quoteIdent(tableName),
		strings.Join(withParts, ",\n  "),
		spec.SQL,
	)
}

// composeHiveSQL renders the Hive CTAS. Uses Hive-specific
// properties (`external_location`, `bucketed_by` + `bucket_count`
// as separate props). Works on both engine v2 + v3 workgroups.
//
// Hive's `sorted_by` is a table property hint only — the writer
// doesn't enforce order. To actually get physically-sorted output,
// athenaio appends `ORDER BY` to the user's SELECT (wrapped in a
// subquery so it doesn't clash with any user-level top-level ORDER).
// Even so, the returned PartitionMetadata marks SortEnforced=false —
// downstream operators (Over/Join/GroupBy fast paths) refuse to
// trust hint-only sort claims. Users who need enforcement rely on
// the ORDER BY wrapping directly or migrate to Iceberg.
func composeHiveSQL(database, tableName, externalLoc string, spec UnloadSpec) string {
	withParts := []string{
		"format = 'PARQUET'",
		fmt.Sprintf("external_location = '%s'", externalLoc),
		fmt.Sprintf("bucketed_by = ARRAY[%s]", quotedIdentArray(spec.PartitionBy)),
		fmt.Sprintf("bucket_count = %d", spec.BucketCount),
	}
	if len(spec.OrderBy) > 0 {
		withParts = append(withParts,
			fmt.Sprintf("sorted_by = ARRAY[%s]", hiveSortedBy(spec.OrderBy)))
	}

	// Physically enforce the sort via ORDER BY on the SELECT body,
	// wrapping to preserve arbitrary user SQL. Iceberg would enforce
	// this natively; Hive needs our help.
	body := spec.SQL
	if len(spec.OrderBy) > 0 {
		body = fmt.Sprintf("SELECT * FROM (\n%s\n) ORDER BY %s",
			spec.SQL, hiveOrderByClause(spec.OrderBy))
	}

	return fmt.Sprintf("CREATE TABLE %s.%s\nWITH (\n  %s\n) AS\n%s",
		quoteIdent(database),
		quoteIdent(tableName),
		strings.Join(withParts, ",\n  "),
		body,
	)
}

// icebergBucketExpr renders the Iceberg `partitioning` array element
// for hash bucketing. Iceberg's syntax is `bucket(N, col)` for a
// single-column bucket transform; multi-column composes the columns
// inside a single bucket(...) call — bucket(N, col1, col2) hashes
// the tuple.
func icebergBucketExpr(cols []string, n int) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = quoteIdent(c)
	}
	return fmt.Sprintf("'bucket(%d, %s)'", n, strings.Join(parts, ", "))
}

// icebergSortedBy renders the Iceberg `sorted_by` array — one
// single-quoted element per SortKey. Format: 'col1', 'col2 DESC'.
// Ascending is Iceberg's default so we omit the ASC keyword to keep
// the composed SQL tidy.
func icebergSortedBy(keys []gobi.SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		if k.Descending {
			parts[i] = fmt.Sprintf("'%s DESC'", k.Column)
		} else {
			parts[i] = fmt.Sprintf("'%s'", k.Column)
		}
	}
	return strings.Join(parts, ", ")
}

// hiveSortedBy renders Hive's `sorted_by` array. Same syntactic
// shape as Iceberg's, but semantics differ: Hive treats this as a
// hint rather than a physical guarantee. Kept separate from
// icebergSortedBy so the two formats' quirks stay isolated even
// though the current output happens to match.
func hiveSortedBy(keys []gobi.SortKey) string {
	return icebergSortedBy(keys)
}

// hiveOrderByClause renders the user-SELECT-wrap ORDER BY that
// athenaio appends when the spec asks for sorted output under Hive.
// Format: `col1, col2 DESC`. Unquoted column names (per SQL ORDER
// BY convention).
func hiveOrderByClause(keys []gobi.SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		if k.Descending {
			parts[i] = fmt.Sprintf("%s DESC", k.Column)
		} else {
			parts[i] = k.Column
		}
	}
	return strings.Join(parts, ", ")
}

// quotedIdentArray renders `'col1', 'col2'` — Hive's `bucketed_by`
// takes single-quoted string literals (unlike Iceberg's
// `partitioning` which puts the whole `bucket(N, col)` expr inside
// a single-quoted string). Kept as a separate helper to make the
// two shapes obvious side-by-side.
func quotedIdentArray(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("'%s'", c)
	}
	return strings.Join(parts, ", ")
}

// quoteIdent double-quotes an identifier for safe inclusion in
// SQL. Handles the standard case (no embedded quotes) — user input
// shouldn't reach here since Database/Table names are athenaio-
// controlled, but the quoting keeps composed SQL uniformly styled.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

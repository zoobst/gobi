package athenaio

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
)

// runPrepass submits a LIMIT-0 wrapping of userSQL to discover the
// column projection Athena will produce, without materializing any
// rows. Returns the ordered list of column names.
//
// Athena's GetQueryResults includes a ResultSetMetadata.ColumnInfo
// list even when the ResultSet has zero rows — that's the only cheap
// way to introspect a query's projection today (Athena doesn't
// expose a "describe query" API). One-round-trip cost:
// StartQueryExecution + poll to done + GetQueryResults with
// MaxResults=1 (SDK requires >0).
//
// The prepass query bills for zero data scanned (LIMIT 0 short-
// circuits Athena's scan planner in every engine version I've
// tested) so the cost delta is one Athena API dispatch per
// UnloadAndRead call.
func (c *Client) runPrepass(ctx context.Context, userSQL string) ([]string, error) {
	sql := fmt.Sprintf("SELECT * FROM (%s) LIMIT 0", strings.TrimSpace(userSQL))
	queryID, err := c.submit(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("prepass submit: %w", err)
	}
	if _, err := c.pollUntilDone(ctx, queryID); err != nil {
		return nil, fmt.Errorf("prepass %s: %w", queryID, err)
	}

	// GetQueryResults with the smallest allowed page — we only care
	// about the schema, not rows.
	one := int32(1)
	out, err := c.athena.GetQueryResults(ctx, &athena.GetQueryResultsInput{
		QueryExecutionId: aws.String(queryID),
		MaxResults:       &one,
	})
	if err != nil {
		return nil, fmt.Errorf("prepass %s GetQueryResults: %w", queryID, err)
	}
	if out.ResultSet == nil || out.ResultSet.ResultSetMetadata == nil {
		return nil, fmt.Errorf("prepass %s: nil ResultSetMetadata", queryID)
	}
	cols := make([]string, 0, len(out.ResultSet.ResultSetMetadata.ColumnInfo))
	for _, ci := range out.ResultSet.ResultSetMetadata.ColumnInfo {
		if ci.Name == nil {
			return nil, fmt.Errorf("prepass %s: ColumnInfo with nil Name", queryID)
		}
		cols = append(cols, *ci.Name)
	}
	return cols, nil
}

// verifyPartitionColsPresent confirms every column in partitionBy
// exists in cols (case-insensitive — Athena uppercases column names
// in some contexts). Returns nil if all are present, otherwise an
// error naming the first missing column.
func verifyPartitionColsPresent(partitionBy, cols []string) error {
	// Fold cols to a lower-case set for O(1) lookup. Case-insensitive
	// because Athena normalizes column names in its result metadata
	// even when the source SELECT preserves case in identifiers.
	set := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		set[strings.ToLower(c)] = struct{}{}
	}
	for _, want := range partitionBy {
		if _, ok := set[strings.ToLower(want)]; !ok {
			return fmt.Errorf("partition column %q not present in SELECT projection (found: %v)",
				want, cols)
		}
	}
	return nil
}

// Package pgio reads and writes gobi Frames against PostgreSQL /
// PostGIS databases.
//
// Depends on github.com/jackc/pgx/v5 in native mode (not the
// database/sql wrapper). Native mode is chosen deliberately:
//
//   - pgx.CopyFrom gives bulk INSERT throughput 10-100× the naive
//     "INSERT ... VALUES" loop that database/sql would force.
//     PostGIS workflows tend to be bulk-load heavy, and losing this
//     for architectural consistency with gpkgio (which uses
//     database/sql via modernc.org/sqlite) is a bad trade.
//   - pgx's pgtype layer knows how to decode PostGIS geometry OIDs
//     into structured values. WKB / EWKB round-trips are cleaner
//     without an intermediate sql.RawBytes conversion.
//
// Everything is pure Go — pgx has no cgo dependency, matching the
// gobi-wide constraint.
//
// The package offers three entry points, mirroring the shape of
// parquetio / gpkgio:
//
//   - ReadQuery / ReadTable materialize the whole result set as a
//     single Frame. Peak memory scales with the query's row count.
//
//   - ReadTableChunksFunc / ReadQueryChunksFunc stream results as
//     record-batch-sized Frames. Peak memory bounded to one batch.
//
//   - ScanTable / ScanQuery return gobi.LazyFrames. Participates in
//     the optimizer's projection + predicate pushdown — SELECT
//     column lists narrow to what the plan actually uses, and
//     Filter above the scan translates to SQL WHERE via
//     gobi.ExprToSQL.
//
// Writes go through WriteTable, which uses pgx.CopyFrom under the
// hood for throughput. The target table must already exist —
// PostgreSQL's DDL is too tied to the caller's schema conventions
// for us to auto-create.
//
// Geometry columns are returned as arrow Binary tagged with
// gobi.GeometryField metadata carrying the source SRID. The
// underlying bytes are WKB (Well-Known Binary) — pgio strips the
// EWKB SRID prefix on read and reattaches it on write via the
// gpkg-style GPB header. Callers who need EWKB directly can use
// pgx.Query themselves.
package pgio

import (
	"context"
	"errors"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Conn is the small subset of *pgx.Conn / *pgxpool.Pool that pgio
// needs. Accepting an interface (rather than a concrete type) lets
// callers pass whichever they use — direct connections for
// short-lived tools, pooled connections for long-running services.
// The pgx types satisfy this shape automatically.
type Conn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	// CopyFrom is the fast-path bulk-loader. WriteTable requires it.
	// Callers passing a Conn that doesn't support CopyFrom (rare —
	// pgx.Conn and pgxpool.Pool both do) will hit an error at
	// WriteTable time, not construction time.
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// ReadOptions controls ReadQuery / ReadTable / ScanTable behavior.
type ReadOptions struct {
	// Schema is the PostgreSQL schema qualifying Table. Defaults to
	// "public".
	Schema string

	// Columns restricts the SELECT to the named columns. nil / empty
	// means "select every column" (SELECT *). When set, geometry
	// columns are included automatically only if they appear in the
	// list — unlike gpkgio, pgio doesn't sneak geometry columns
	// back in on projection, because a PostgreSQL table can have
	// multiple geometry columns (or none) and there's no canonical
	// choice.
	Columns []string

	// Where is an optional SQL fragment appended after WHERE. Use
	// `$1` / `$2` placeholders and supply values via WhereArgs.
	// ScanTable's predicate-pushdown callback fills these in
	// automatically from Frame.FilterExpr; direct ReadTable callers
	// supply their own.
	Where string

	// WhereArgs is the positional args for Where's placeholders.
	WhereArgs []any

	// Limit caps the number of rows returned. 0 = unlimited.
	Limit int64

	// GeomColumns names columns that should be treated as PostGIS
	// geometries. When empty, pgio queries the geometry_columns view
	// to detect them — a small extra query but zero-config. Setting
	// this explicitly skips that lookup, which matters on tables
	// where the geometry_columns registry might be out of date
	// (e.g., unregistered geometry columns created via ALTER TABLE
	// without the AddGeometryColumn helper).
	GeomColumns []string

	// Allocator overrides the arrow allocator used for the produced
	// Frame's columns. nil = memory.DefaultAllocator. Provided for
	// callers who pool arrow buffers across pipelines.
	Allocator memory.Allocator

	// pushdownDone is an internal flag ScanTable's predicate-pushdown
	// callback sets after the first successful push, mirroring
	// gpkgio's Options.pushdownDone — see there for the rationale.
	pushdownDone bool
}

// WriteOptions controls WriteTable behavior.
type WriteOptions struct {
	// Schema qualifies the target table. Defaults to "public".
	Schema string

	// Truncate, when true, issues `TRUNCATE TABLE ...` before the
	// bulk insert. Convenient for full-refresh ETL patterns; leave
	// off for append workflows.
	Truncate bool

	// GeomCol names a column that should be encoded as PostGIS
	// EWKB. When empty and the Frame has a geometry-tagged column,
	// pgio uses that. Explicit GeomCol wins over inference — needed
	// when a Frame has multiple geometry columns.
	GeomCol string

	// SRID overrides the geometry column's SRID when writing. When
	// zero, pgio uses the SRID carried by the geometry column's
	// arrow-field metadata (MetaGeometryCRS). If the metadata is
	// also empty, defaults to 4326 (WGS 84).
	SRID int32
}

// ErrCopyFromRequired is returned when WriteTable is called with a
// Conn that doesn't support CopyFrom. Standard pgx.Conn and
// pgxpool.Pool both do; the error surfaces if a custom Conn
// implementation left the method un-implemented.
var ErrCopyFromRequired = errors.New("pgio: Conn does not support CopyFrom; use *pgx.Conn or *pgxpool.Pool")

// defaultSchema returns "public" when s is empty — matches
// PostgreSQL's default search_path.
func defaultSchema(s string) string {
	if s == "" {
		return "public"
	}
	return s
}

// resolveAllocator returns opts.Allocator or memory.DefaultAllocator
// when unset. Kept small so every read path uses the same resolution.
func resolveAllocator(opts *ReadOptions) memory.Allocator {
	if opts != nil && opts.Allocator != nil {
		return opts.Allocator
	}
	return memory.DefaultAllocator
}

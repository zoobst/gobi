//go:build integration
// +build integration

// Integration tests for pgio. Only compiled when the "integration"
// build tag is set; the tests read the connection DSN from the
// PGIO_TEST_DSN environment variable and skip if it's unset.
//
// Run against a local Docker PostGIS:
//
//	docker run --rm -d --name pgio-test \
//	  -e POSTGRES_PASSWORD=test -p 5433:5432 postgis/postgis:16-3.4
//	PGIO_TEST_DSN=postgres://postgres:test@localhost:5433/postgres?sslmode=disable \
//	  go test -tags integration -v ./pgio/...
//	docker rm -f pgio-test
//
// Every test creates its own tables (with random suffixes) inside a
// dedicated schema and cleans up on completion, so tests can run
// against a shared server without stepping on each other.

package pgio_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/jackc/pgx/v5"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/pgio"
)

// mustConn opens a pgx.Conn from PGIO_TEST_DSN or skips the test.
// Every call gets its own connection to avoid schema/table
// collisions when tests run in parallel.
func mustConn(t *testing.T) (*pgx.Conn, func()) {
	t.Helper()
	dsn := os.Getenv("PGIO_TEST_DSN")
	if dsn == "" {
		t.Skip("PGIO_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	return conn, func() { conn.Close(ctx) }
}

// uniqueTable returns a table name unlikely to collide with parallel
// runs against the same server. Short so we don't blow the 63-byte
// PostgreSQL identifier limit.
func uniqueTable(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, rand.Uint32N(1_000_000))
}

// TestIntegration_WriteThenRead is the smoke test — build a Frame
// with attributes + geometry, WriteTable, then ReadTable, then
// verify every column round-tripped byte-identically.
func TestIntegration_WriteThenRead(t *testing.T) {
	conn, done := mustConn(t)
	defer done()
	ctx := context.Background()

	table := uniqueTable("pgio_rt")
	defer func() {
		conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, table))
	}()

	// CREATE the table up front — WriteTable doesn't do DDL.
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %q (
			id BIGINT,
			name TEXT,
			value DOUBLE PRECISION,
			geom GEOMETRY(POINT, 4326)
		)`, table)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	df := buildTestFrame(t)
	if err := pgio.WriteTable(ctx, conn, table, df, &pgio.WriteOptions{
		GeomCol: "geom",
		SRID:    4326,
	}); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	out, err := pgio.ReadTable(ctx, conn, table, nil)
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if out.NumRows() != 3 {
		t.Fatalf("rows = %d, want 3", out.NumRows())
	}
	names := out.ColumnNames()
	if len(names) != 4 {
		t.Fatalf("cols = %v, want 4", names)
	}

	// Geometry sanity check: parse the first row's WKB back to a Point.
	geomS, err := out.Column("geom")
	if err != nil {
		t.Fatal(err)
	}
	if !geomS.IsGeometry() {
		t.Errorf("read geometry column lost its geometry tag")
	}
	arr := geomS.Column().Data().Chunks()[0].(*array.Binary)
	g, err := geometry.ParseWKB(arr.Value(0))
	if err != nil {
		t.Fatalf("parse row 0: %v", err)
	}
	pt, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("row 0 not Point: %T", g)
	}
	if pt.X != 0 || pt.Y != 0 {
		t.Errorf("row 0 point = (%v, %v), want (0, 0)", pt.X, pt.Y)
	}
}

// TestIntegration_ScanTable_PredicatePushdown verifies that a
// FilterExpr above ScanTable translates to a SQL WHERE clause and
// is executed by PostgreSQL. The row count comes back matching the
// predicate.
func TestIntegration_ScanTable_PredicatePushdown(t *testing.T) {
	conn, done := mustConn(t)
	defer done()
	ctx := context.Background()

	table := uniqueTable("pgio_pp")
	defer conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, table))

	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %q (
			id BIGINT,
			name TEXT,
			value DOUBLE PRECISION,
			geom GEOMETRY(POINT, 4326)
		)`, table)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	df := buildTestFrame(t)
	if err := pgio.WriteTable(ctx, conn, table, df, &pgio.WriteOptions{
		GeomCol: "geom", SRID: 4326,
	}); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	// FilterExpr(id > 1) should push id=1 out and return 2 rows.
	lf := pgio.ScanTable(ctx, conn, table, nil).
		Filter(gobi.Col("id").Gt(gobi.Lit(int64(1))))
	explain := lf.ExplainPhysical()
	if !containsAll(explain, []string{"Scan[postgres]", "where="}) {
		t.Fatalf("expected pushdown in explain:\n%s", explain)
	}
	out, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2", out.NumRows())
	}
}

// buildTestFrame — 3-row Frame with id/name/value/geom, matching
// the CREATE TABLE above.
func buildTestFrame(t *testing.T) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3}, nil)
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"a", "b", "c"}, nil)
	valB := array.NewFloat64Builder(pool)
	defer valB.Release()
	valB.AppendValues([]float64{1.5, 2.5, 3.5}, nil)
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, pt := range []geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 2}} {
		geomB.Append(geometry.WKB(pt))
	}

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		gobi.GeometryField("geom", 4326),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), nameB.NewArray(), valB.NewArray(), geomB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := gobi.NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

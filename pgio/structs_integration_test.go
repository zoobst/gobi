//go:build integration
// +build integration

package pgio_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zoobst/gobi/pgio"
)

type pgStructRow struct {
	ID    int64   `pgio:"id"`
	Name  string  `pgio:"name" gobi:"name_generic"` // pgio: wins
	Value float64 `gobi:"value"`                    // no pgio tag → gobi fallback
	Skip  string  `pgio:"-"`
}

func TestPGIOReadWriteStructsTable_Roundtrip(t *testing.T) {
	conn, done := mustConn(t)
	defer done()
	ctx := context.Background()

	table := uniqueTable("pgio_structs")
	defer func() {
		_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, table))
	}()

	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %q (
			id BIGINT,
			name TEXT,
			value DOUBLE PRECISION
		)`, table)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	rows := []pgStructRow{
		{ID: 1, Name: "alice", Value: 3.14, Skip: "gone"},
		{ID: 2, Name: "bob", Value: 2.71, Skip: "gone"},
	}
	if err := pgio.WriteStructsTable(ctx, conn, table, rows, nil); err != nil {
		t.Fatalf("WriteStructsTable: %v", err)
	}
	got, err := pgio.ReadStructsTable[pgStructRow](ctx, conn, table, nil)
	if err != nil {
		t.Fatalf("ReadStructsTable: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Rows may come back out-of-order; index by ID.
	byID := map[int64]pgStructRow{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if r := byID[1]; r.Name != "alice" || r.Value != 3.14 || r.Skip != "" {
		t.Errorf("row id=1 = %+v", r)
	}
	if r := byID[2]; r.Name != "bob" || r.Value != 2.71 || r.Skip != "" {
		t.Errorf("row id=2 = %+v", r)
	}
}

func TestPGIOReadStructsQuery_TagPriority(t *testing.T) {
	conn, done := mustConn(t)
	defer done()
	ctx := context.Background()

	// SELECT casts columns to specific names so we can prove the
	// `pgio:` tag maps the row back to the right Go fields.
	sql := `SELECT 42::bigint AS id, 'hello'::text AS name, 1.5::double precision AS value`
	rows, err := pgio.ReadStructsQuery[pgStructRow](ctx, conn, sql)
	if err != nil {
		t.Fatalf("ReadStructsQuery: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 42 || rows[0].Name != "hello" || rows[0].Value != 1.5 {
		t.Errorf("row = %+v", rows[0])
	}
}

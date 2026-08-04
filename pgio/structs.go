package pgio

import (
	"context"

	"github.com/zoobst/gobi"
)

// ReadStructsQuery runs sql against conn and decodes each row into a
// T. Column names are matched against T's fields via gobi's tag
// resolution under the "pgio" namespace: `pgio:"col"` tags take
// priority, with fallback to `gobi:"col"` → field name. `pgio:"-"`
// skips a field. Fields tagged `geom:"true"` map to geometry columns
// (already unwrapped from EWKB by pgio's read path).
//
//	type Row struct {
//	    ID       int64  `pgio:"id"`
//	    Name     string `pgio:"name"`
//	    Geometry []byte `geom:"true"`
//	}
//	rows, err := pgio.ReadStructsQuery[Row](ctx, conn, "SELECT * FROM parks")
//
// Wraps ReadQuery + gobi.ToStructs.
func ReadStructsQuery[T any](ctx context.Context, conn Conn, sql string, args ...any) ([]T, error) {
	f, err := ReadQuery(ctx, conn, sql, args...)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("pgio"))
}

// ReadStructsTable reads a whole table (with optional projection +
// WHERE via opts) and decodes each row into a T. See ReadStructsQuery
// for the tag conventions. Wraps ReadTable + gobi.ToStructs.
func ReadStructsTable[T any](ctx context.Context, conn Conn, table string, opts *ReadOptions) ([]T, error) {
	f, err := ReadTable(ctx, conn, table, opts)
	if err != nil {
		return nil, err
	}
	return gobi.ToStructs[T](f, gobi.StructTagFormat("pgio"))
}

// WriteStructsTable encodes rows into a table. Column names come from
// the "pgio" tag namespace (see ReadStructsQuery). Wraps FromStructs
// + WriteTable.
func WriteStructsTable[T any](ctx context.Context, conn Conn, table string, rows []T, opts *WriteOptions) error {
	f, err := gobi.FromStructs(rows, gobi.StructTagFormat("pgio"))
	if err != nil {
		return err
	}
	return WriteTable(ctx, conn, table, f, opts)
}

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
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. (The variadic slot holds query args, so
// there is no structOpts here; for a shared interner call ReadQuery
// + gobi.ToStructs with gobi.StructInterner directly.)
func ReadStructsQuery[T any](ctx context.Context, conn Conn, sql string, args ...any) ([]T, error) {
	f, err := ReadQuery(ctx, conn, sql, args...)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, gobi.StructTagFormat("pgio"), gobi.StructCopyValues())
}

// ReadStructsTable reads a whole table (with optional projection +
// WHERE via opts) and decodes each row into a T. See ReadStructsQuery
// for the tag conventions. Wraps ReadTable + gobi.ToStructs.
//
// Strings and []byte fields are copied out of the intermediate Frame,
// which is released before returning, so the rows own their memory
// and don't pin read buffers. Pass gobi.StructInterner in structOpts
// to share one copy of each value of `intern`-tagged fields across
// calls.
func ReadStructsTable[T any](ctx context.Context, conn Conn, table string, opts *ReadOptions, structOpts ...gobi.StructOption) ([]T, error) {
	f, err := ReadTable(ctx, conn, table, opts)
	if err != nil {
		return nil, err
	}
	defer f.Release()
	return gobi.ToStructs[T](f, append([]gobi.StructOption{gobi.StructTagFormat("pgio"), gobi.StructCopyValues()}, structOpts...)...)
}

// WriteStructsTable encodes rows into a table. Column names come from
// the "pgio" tag namespace (see ReadStructsQuery). Wraps FromStructs
// + WriteTable.
func WriteStructsTable[T any](ctx context.Context, conn Conn, table string, rows []T, opts *WriteOptions, structOpts ...gobi.StructOption) error {
	f, err := gobi.FromStructs(rows, append([]gobi.StructOption{gobi.StructTagFormat("pgio")}, structOpts...)...)
	if err != nil {
		return err
	}
	defer f.Release()
	return WriteTable(ctx, conn, table, f, opts)
}

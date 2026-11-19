package sqlitedb

import (
	"context"
	"database/sql"
	"strings"
)

// Object is one entry of a database's sqlite_schema, its SQL with runs of
// whitespace collapsed.
type Object struct {
	Type, Name, Table, SQL string
}

// Objects lists d's schema objects by type and name.
func (d *DB) Objects(ctx context.Context) ([]Object, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema ORDER BY type, name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var objects []Object
	for rows.Next() {
		var o Object
		if err := rows.Scan(&o.Type, &o.Name, &o.Table, &o.SQL); err != nil {
			return nil, err
		}
		o.SQL = strings.Join(strings.Fields(o.SQL), " ")
		objects = append(objects, o)
	}
	return objects, rows.Err()
}

// Objects lists the schema objects s's migrations create, applied to a
// private in-memory database.
func (s Schema) Objects(ctx context.Context) ([]Object, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection holds the one in-memory database
	d := &DB{db: db, schema: s, detached: context.WithoutCancel(ctx)}
	defer func() { _ = d.Close() }()
	if _, _, err := d.migrate(ctx, nil, true); err != nil {
		return nil, err
	}
	return d.Objects(ctx)
}

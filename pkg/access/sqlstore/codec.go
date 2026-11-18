package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
)

//go:embed *.sql SUMS
var migrations embed.FS

// schema owns access.sqlite; the application ID spells "zzac".
var schema = sqlitedb.Schema{AppID: 0x7a7a6163, FS: migrations}

// col pairs a column with a pointer to the field it stores. One list both
// writes a row and scans it, so the two cannot drift.
type col struct {
	name string
	ptr  any
}

func ptrs(cols []col) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		out[i] = c.ptr
	}
	return out
}

// table holds the statements for one table; its first column is the primary key.
type table struct {
	insert, upsert, selectAll, remove string
}

func newTable(name string, cols []col) table {
	names := make([]string, len(cols))
	var updates []string
	for i, c := range cols {
		names[i] = c.name
		if i > 0 {
			updates = append(updates, c.name+" = excluded."+c.name)
		}
	}
	list := strings.Join(names, ", ")
	insert := "INSERT INTO " + name + " (" + list + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	return table{
		insert:    insert,
		upsert:    insert + " ON CONFLICT (" + names[0] + ") DO UPDATE SET " + strings.Join(updates, ", "),
		selectAll: "SELECT " + list + " FROM " + name,
		remove:    "DELETE FROM " + name + " WHERE " + names[0] + " = ?",
	}
}

var (
	keyTable          = newTable("keys", keyCols(&keys.VirtualKey{}))
	teamTable         = newTable("teams", teamCols(&teams.Team{}))
	personTable       = newTable("people", personCols(&people.PersonRecord{}))
	grantTable        = newTable("grants", grantCols(&grantRow{}))
	installationTable = newTable("installation", installationCols(&people.Records{}))
	auditTable        = newTable("audit_events", auditCols(&auditRow{}))
)

// micros stores a time as INTEGER unix microseconds in UTC, truncated to the
// microsecond (nanoseconds would overflow after year 2262). encode applies the
// same conversion to every record before it is published.
type micros struct{ t *time.Time }

func (m micros) Value() (driver.Value, error) { return m.t.UnixMicro(), nil }

func (m micros) Scan(src any) error {
	v, ok := src.(int64)
	if !ok {
		return fmt.Errorf("time column holds %T", src)
	}
	*m.t = time.UnixMicro(v).UTC()
	return nil
}

// optMicros stores a nil time as NULL.
type optMicros struct{ t **time.Time }

func (m optMicros) Value() (driver.Value, error) {
	if *m.t == nil {
		return nil, nil
	}
	return micros{*m.t}.Value()
}

func (m optMicros) Scan(src any) error {
	if src == nil {
		*m.t = nil
		return nil
	}
	*m.t = new(time.Time)
	return micros{*m.t}.Scan(src)
}

// jsonText stores a value as JSON, and a nil or zero value as NULL: a zero
// policy would otherwise marshal as its explicit default.
type jsonText struct{ v any }

func (j jsonText) Value() (driver.Value, error) {
	if zero, ok := j.v.(interface{ IsZero() bool }); ok && zero.IsZero() {
		return nil, nil
	}
	data, err := json.Marshal(j.v)
	if err != nil || string(data) == "null" {
		return nil, err
	}
	return string(data), nil
}

// Scan replaces the target rather than decoding into it: the target may be a
// map someone else holds.
func (j jsonText) Scan(src any) error {
	reflect.ValueOf(j.v).Elem().SetZero()
	switch src := src.(type) {
	case nil:
		return nil
	case string:
		return json.Unmarshal([]byte(src), j.v)
	}
	return fmt.Errorf("JSON column holds %T", src)
}

// optText stores an empty string as NULL, for a reference that may be absent.
type optText struct{ s *string }

func (o optText) Value() (driver.Value, error) {
	if *o.s == "" {
		return nil, nil
	}
	return *o.s, nil
}

func (o optText) Scan(src any) error {
	switch src := src.(type) {
	case nil:
		*o.s = ""
	case string:
		*o.s = src
	default:
		return fmt.Errorf("text column holds %T", src)
	}
	return nil
}

func quotaCols(q *quota.QuotaConfig) []col {
	return []col{
		{"rpm_limit", &q.RPMLimit}, {"tpm_limit", &q.TPMLimit}, {"max_parallel_requests", &q.MaxParallelRequests},
		{"spend_limit", &q.SpendLimit}, {"reset_period", &q.ResetPeriod}, {"default_max_tokens", &q.DefaultMaxTokens},
	}
}

// keyCols has no KeyEnv: checkKey refuses a key that sets it.
func keyCols(k *keys.VirtualKey) []col {
	cols := []col{
		{"id", &k.ID}, {"uuid", &k.UUID}, {"name", &k.Name}, {"description", &k.Description},
		{"hashed_key", &k.HashedKey}, {"team_id", &k.TeamID}, {"team_role", &k.TeamRole}, {"role", &k.Role},
		{"suspended", &k.Suspended}, {"suspended_at", optMicros{&k.SuspendedAt}}, {"suspended_by", &k.SuspendedBy},
		{"expires_at", optMicros{&k.ExpiresAt}}, {"created_at", micros{&k.CreatedAt}}, {"updated_at", micros{&k.UpdatedAt}},
		{"last_seen_at", micros{&k.LastSeenAt}}, {"created_by", &k.CreatedBy}, {"updated_by", &k.UpdatedBy},
		{"metadata", jsonText{&k.Metadata}},
	}
	return append(cols, quotaCols(&k.QuotaConfig)...)
}

func teamCols(t *teams.Team) []col {
	cols := []col{
		{"id", &t.ID}, {"uuid", &t.UUID}, {"name", &t.Name}, {"kind", &t.Kind},
		{"allowed_models", jsonText{&t.AllowedModels}}, {"route_access", jsonText{&t.RouteAccess}},
		{"provider_access", jsonText{&t.ProviderAccess}},
		{"suspended", &t.Suspended}, {"suspended_at", optMicros{&t.SuspendedAt}}, {"suspended_by", &t.SuspendedBy},
		{"created_at", micros{&t.CreatedAt}}, {"updated_at", micros{&t.UpdatedAt}},
		{"created_by", &t.CreatedBy}, {"updated_by", &t.UpdatedBy}, {"metadata", jsonText{&t.Metadata}},
	}
	return append(cols, quotaCols(&t.QuotaConfig)...)
}

func personCols(p *people.PersonRecord) []col {
	return []col{
		{"id", &p.ID}, {"username", &p.Username}, {"name", &p.Name}, {"role", &p.Role}, {"team_id", &p.TeamID},
		{"owner", &p.Owner}, {"suspended", &p.Suspended}, {"revision", &p.Revision}, {"password_hash", &p.PasswordHash},
		{"created_at", micros{&p.CreatedAt}}, {"updated_at", micros{&p.UpdatedAt}},
	}
}

// grantRow is a grant with the token digest people key it by. Status is
// derived on read, so it has no column.
type grantRow struct {
	hash string
	people.GrantRecord
}

func grantCols(g *grantRow) []col {
	return []col{
		{"token_hash", &g.hash}, {"id", &g.ID}, {"purpose", &g.Purpose}, {"team_id", &g.TeamID}, {"role", &g.Role},
		{"person_id", &g.PersonID}, {"created_at", micros{&g.CreatedAt}}, {"expires_at", micros{&g.ExpiresAt}},
		{"used_at", optMicros{&g.UsedAt}}, {"revoked_at", optMicros{&g.RevokedAt}},
		{"issuer_person_id", &g.Issuer.PersonID}, {"issuer_revision", &g.Issuer.Revision},
		{"issuer_reference", &g.Issuer.Reference}, {"browser_hash", &g.BrowserHash},
	}
}

// installationCols stores the owner and progress; Initialized is OwnerID != "",
// which people's validation already requires.
func installationCols(r *people.Records) []col {
	id := 1
	return []col{
		{"id", &id}, {"owner_id", optText{&r.OwnerID}}, {"step", &r.Progress.Step}, {"dismissed", &r.Progress.Dismissed},
		{"response_verified", &r.Progress.ResponseVerified}, {"app_connected", &r.Progress.AppConnected},
		{"progress_updated_at", micros{&r.Progress.UpdatedAt}},
	}
}

// auditRow is an event with the sequence of the commit that wrote it.
type auditRow struct {
	seq int64
	audit.Event
}

func auditCols(r *auditRow) []col {
	return []col{
		{"seq", &r.seq}, {"time", micros{&r.Time}}, {"action", &r.Action}, {"actor", &r.Actor},
		{"subject", &r.Subject}, {"request_id", &r.RequestID}, {"metadata", jsonText{&r.Metadata}},
	}
}

// encode converts each column to the value SQLite stores and reads that value
// back through the codec, so the record holds exactly what a reopen reads and
// no value is encoded inside a transaction.
func encode(cols []col) ([]any, error) {
	args := make([]any, len(cols))
	for i, c := range cols {
		args[i] = c.ptr
		codec, ok := c.ptr.(interface {
			driver.Valuer
			sql.Scanner
		})
		if !ok {
			continue // a plain field stores itself unchanged
		}
		value, err := codec.Value()
		if err == nil {
			err = codec.Scan(value)
		}
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", c.name, err)
		}
		args[i] = value
	}
	return args, nil
}

// querier is a database or a transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scanAll scans every row query returns into a fresh R and hands it to keep.
// All rows are read before it returns, so the connection is free again.
func scanAll[R any](ctx context.Context, db querier, cols func(*R) []col, keep func(*R), query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := new(R)
		if err := rows.Scan(ptrs(cols(r))...); err != nil {
			return fmt.Errorf("%w: %w", errBroken, err)
		}
		keep(r)
	}
	return rows.Err()
}

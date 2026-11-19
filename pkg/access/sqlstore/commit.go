package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stperic/zzrouter/pkg/utils"
)

const updateMeta = "UPDATE meta SET value = ? WHERE key = ?"

var (
	// ErrFailed refuses every write after the store lost track of what it holds:
	// a COMMIT failed and reading the database back failed too.
	ErrFailed = errors.New("access store cannot vouch for its state")
	// ErrBusy refuses an open, a write or an audit read because another process
	// held the database past the busy timeout; nothing changed, and a retry may
	// succeed.
	ErrBusy = errors.New("access store is busy: another program is holding the access database")
)

// busy is ErrBusy for an operation another process's lock refused, err otherwise.
func busy(err error) error {
	if sqlitedb.IsBusy(err) {
		return ErrBusy
	}
	return err
}

// writes is the SQL of one commit, its values encoded before the transaction opens.
type writes struct {
	stmts                     []stmt
	revises                   bool // keys, teams, people or invitations changed
	keysChanged               bool
	removedKeys, removedTeams []string
}

type stmt struct {
	query string
	args  []any
}

func (w *writes) exec(query string, args ...any) { w.stmts = append(w.stmts, stmt{query, args}) }

// tx is one commit being built from the published meta: the records it
// changes, the events it records, in order, a new generation when it replaces
// the content, and where to report the meta it commits, for an operation that does.
type tx struct {
	records
	meta       Meta
	events     []audit.Event
	generation string
	committed  *Meta
}

// record adds ev to the commit; a zero event records nothing.
func (t *tx) record(ev audit.Event) {
	if ev.Action != "" {
		t.events = append(t.events, ev)
	}
}

// stamped is ev with the facts the store decided added to a copy of its metadata.
func stamped(ev audit.Event, facts map[string]any) audit.Event {
	metadata := maps.Clone(ev.Metadata)
	if metadata == nil {
		metadata = make(map[string]any, len(facts))
	}
	maps.Copy(metadata, facts)
	ev.Metadata = metadata
	return ev
}

// alongside is the event for a record the store changes on ev's behalf: ev's
// actor and request, and nothing when ev records nothing.
func alongside(ev audit.Event, action audit.Action, subject string, metadata map[string]any) audit.Event {
	if ev.Action == "" {
		return audit.Event{}
	}
	return audit.Event{Action: action, Actor: ev.Actor, Subject: subject, RequestID: ev.RequestID, Metadata: metadata}
}

// mutate runs apply on a copy of p's records and commits the change with its
// events. Subscribers, or Options.Fatal after a failure, run outside the gate.
func (s *Store) mutate(ctx context.Context, p part, apply func(*tx) error) error {
	after, err := s.commit(ctx, p, apply)
	if after != nil {
		after()
	}
	return err
}

func (s *Store) commit(ctx context.Context, p part, apply func(*tx) error) (func(), error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.refusal != nil {
		return nil, s.refusal
	}
	prev := s.pub.Load()
	t := &tx{records: prev.records, meta: prev.meta}
	if p&keyRecords != 0 {
		t.keys = maps.Clone(t.keys)
	}
	if p&teamRecords != 0 {
		t.teams = maps.Clone(t.teams)
	}
	if p&peopleRecords != 0 {
		t.people = t.people.Clone()
	}
	if err := apply(t); err != nil {
		return nil, err
	}
	w, err := diff(&prev.records, &t.records, p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m := prev.meta
	m.Sequence++
	if w.revises {
		m.Revision++
	}
	if t.generation != "" {
		m.Generation = t.generation
		w.exec(updateMeta, m.Generation, "generation")
	}
	if t.committed != nil {
		*t.committed = m
	}
	changed := len(w.stmts) > 0
	if !changed && len(t.events) == 0 {
		return nil, nil
	}
	snap := prev.snap
	if changed {
		keysVersion := snap.KeysVersion()
		if w.keysChanged {
			keysVersion++
		}
		snap = access.NewSnapshot(m.Generation, keysVersion, t.keys, t.teams, t.people)
		if err := snap.Validate(); err != nil {
			return nil, err
		}
	}
	for _, ev := range t.events {
		row := auditRow{Event: audit.Attribute(ctx, ev)}
		row.Time = utils.NowUTC() // the store owns event time; a caller's is ignored
		if err := row.Validate(); err != nil {
			return nil, err
		}
		args, err := encode(auditCols(&row)[1:])
		if err != nil {
			return nil, err
		}
		w.exec(auditAppend, args...)
	}
	w.exec(updateMeta, strconv.FormatUint(m.Revision, 10), "revision")
	w.exec(updateMeta, strconv.FormatUint(m.Sequence, 10), "sequence")
	published := &state{records: t.records, snap: snap, meta: m}
	free, uncertain, err := s.write(ctx, w)
	if err != nil {
		if !uncertain {
			return nil, busy(err)
		}
		if published, err = s.reload(ctx, err, m.Sequence); err != nil {
			if errors.Is(err, ErrFailed) && s.fatal != nil {
				return func() { s.fatal(err) }, err
			}
			return nil, err
		}
	}
	published.freeBytes = free
	s.pub.Store(published)
	if !changed {
		return nil, nil
	}
	c, subs := access.Commit{RemovedKeys: w.removedKeys, RemovedTeams: w.removedTeams}, s.subs
	return func() {
		for _, fn := range subs {
			fn(c)
		}
	}, nil
}

// write runs w in one immediate transaction that the caller's cancellation
// cannot cut short, and returns the free-page bytes it leaves, read under the
// transaction's lock so nothing waits between the COMMIT and the publication.
// uncertain marks a failed COMMIT, which may have committed.
func (s *Store) write(ctx context.Context, w *writes) (free sql.NullInt64, uncertain bool, err error) {
	ctx = context.WithoutCancel(ctx)
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return free, false, fmt.Errorf("begin access commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, st := range w.stmts {
		if _, err := tx.ExecContext(ctx, st.query, st.args...); err != nil {
			return free, false, fmt.Errorf("write access records: %w", err)
		}
	}
	free = freeBytes(ctx, tx)
	if s.beforeCommit != nil {
		if err := s.beforeCommit(tx); err != nil {
			return free, false, err
		}
	}
	return free, true, tx.Commit()
}

// reload settles a failed COMMIT by the stored sequence. If the database cannot
// be read back, the store fails closed: it publishes a snapshot that admits no
// one and refuses every later write with ErrFailed.
func (s *Store) reload(ctx context.Context, commitErr error, sequence uint64) (*state, error) {
	stored, err := s.readBack(context.WithoutCancel(ctx), s.pub.Load().snap.KeysVersion()+1)
	if err != nil {
		s.refusal = fmt.Errorf("%w: %w", ErrFailed, errors.Join(commitErr, err))
		s.pub.Store(&state{snap: access.FailedSnapshot(), meta: s.pub.Load().meta})
		return nil, s.refusal
	}
	if stored.meta.Sequence != sequence {
		return nil, busy(commitErr)
	}
	return stored, nil
}

// readBack loads in a read transaction: if the driver left the failed
// transaction open, BEGIN fails instead of reading that transaction's writes.
func (s *Store) readBack(ctx context.Context, keysVersion uint64) (*state, error) {
	tx, err := s.db.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return load(ctx, tx, keysVersion)
}

// diff validates what the change to p's parts made of prev and writes it in
// stored form. Foreign keys are checked at COMMIT, so the order is free.
func diff(prev, next *records, p part) (*writes, error) {
	w := &writes{}
	if p&keyRecords != 0 {
		written, removed, err := changedRecords(w, keyTable, "key", prev.keys, next.keys, (*keys.VirtualKey).Validate, keyCols)
		if err != nil {
			return nil, err
		}
		w.keysChanged, w.removedKeys = len(written)+len(removed) > 0, removed
		w.revises = w.keysChanged
	}
	if p&teamRecords != 0 {
		written, removed, err := changedRecords(w, teamTable, "team", prev.teams, next.teams, (*teams.Team).Validate, teamCols)
		if err != nil {
			return nil, err
		}
		w.removedTeams = removed
		w.revises = w.revises || len(written)+len(removed) > 0
	}
	if p&peopleRecords != 0 {
		revises, err := diffPeople(w, prev, next)
		if err != nil {
			return nil, err
		}
		w.revises = w.revises || revises
	}
	return w, nil
}

// changedRecords writes each changed key or team that passes its rule, leaving
// the record in the stored form it is published in, and deletes those next lacks.
func changedRecords[R any](w *writes, t table, kind string, prev, next map[string]*R, check func(*R) error, cols func(*R) []col) (written, removed []string, err error) {
	return rows(w, t, prev, next, same, func(id string, r *R) ([]any, error) {
		if err := check(r); err != nil {
			return nil, fmt.Errorf("%s %q: %w", kind, id, err)
		}
		return encode(cols(r))
	})
}

// diffPeople publishes a validated copy of exactly what it writes and reports
// whether the change advances the revision.
func diffPeople(w *writes, prev, next *records) (bool, error) {
	before, after := prev.people.Records(), next.people.Records()
	if err := storedPeople(&after); err != nil {
		return false, err
	}
	written, removed, err := rows(w, personTable, before.People, after.People, same, func(_ string, p people.PersonRecord) ([]any, error) {
		return encode(personCols(&p))
	})
	if err != nil {
		return false, err
	}
	grants, _, err := rows(w, grantTable, before.Grants, after.Grants, sameGrant, func(hash string, g people.GrantRecord) ([]any, error) {
		return encode(grantCols(&grantRow{hash, g}))
	})
	if err != nil {
		return false, err
	}
	if before.OwnerID != after.OwnerID || before.Progress != after.Progress {
		args, err := encode(installationCols(&after))
		if err != nil {
			return false, err
		}
		w.exec(installationTable.upsert, args...)
	}
	// Pruned grants, console-entry codes and progress leave the revision alone.
	revises := len(written)+len(removed) > 0
	for _, hash := range grants {
		purpose := after.Grants[hash].Purpose
		revises = revises || purpose == people.PurposeInvitation || purpose == people.PurposeRecovery
	}
	state, err := people.NewState(after)
	if err != nil {
		return false, fmt.Errorf("people state: %w", err)
	}
	next.people = state
	return revises, nil
}

// storedPeople puts every person, grant and the progress in stored form.
func storedPeople(r *people.Records) error {
	for id, p := range r.People {
		if _, err := encode(personCols(&p)); err != nil {
			return err
		}
		r.People[id] = p
	}
	for hash, g := range r.Grants {
		row := grantRow{hash, g}
		if _, err := encode(grantCols(&row)); err != nil {
			return err
		}
		r.Grants[hash] = row.GrantRecord
	}
	_, err := encode(installationCols(r))
	return err
}

// rows deletes each record next lacks and writes each one that differs from
// prev with the arguments args returns; it returns the keys it wrote and removed.
func rows[R any](w *writes, t table, prev, next map[string]R, same func(a, b R) bool, args func(string, R) ([]any, error)) (written, removed []string, err error) {
	for id := range prev {
		if _, ok := next[id]; !ok {
			w.exec(t.remove, id)
			removed = append(removed, id)
		}
	}
	for id, r := range next {
		if old, ok := prev[id]; ok && same(old, r) {
			continue
		}
		values, err := args(id, r)
		if err != nil {
			return nil, nil, err
		}
		w.exec(t.upsert, values...)
		written = append(written, id)
	}
	return written, removed, nil
}

func same[T comparable](a, b T) bool { return a == b }

// sameGrant compares by value: Records gives every copy its own time pointers.
func sameGrant(a, b people.GrantRecord) bool {
	sameTime := func(x, y *time.Time) bool { return x == y || x != nil && y != nil && x.Equal(*y) }
	times := sameTime(a.UsedAt, b.UsedAt) && sameTime(a.RevokedAt, b.RevokedAt)
	a.UsedAt, a.RevokedAt, b.UsedAt, b.RevokedAt = nil, nil, nil, nil
	return times && a == b
}

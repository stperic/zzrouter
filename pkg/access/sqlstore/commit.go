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
	"github.com/stperic/zzrouter/pkg/utils"
)

const updateMeta = "UPDATE meta SET value = ? WHERE key = ?"

// ErrFailed refuses every write after the store lost track of what it holds:
// a COMMIT failed and reading the database back failed too.
var ErrFailed = errors.New("access store cannot vouch for its state")

// writes is the SQL of one commit, its values encoded before the transaction opens.
type writes struct {
	stmts                     []stmt
	revises                   bool // keys, teams, people or invitations changed
	removedKeys, removedTeams []string
}

type stmt struct {
	query string
	args  []any
}

func (w *writes) exec(query string, args ...any) { w.stmts = append(w.stmts, stmt{query, args}) }

// mutate runs apply on a copy of p's records and commits the change with its
// event. Subscribers, or Options.Fatal after a failure, run outside the gate.
func (s *Store) mutate(ctx context.Context, p part, apply func(*records) (audit.Event, error)) error {
	after, err := s.commit(ctx, p, apply)
	if after != nil {
		after()
	}
	return err
}

func (s *Store) commit(ctx context.Context, p part, apply func(*records) (audit.Event, error)) (func(), error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.refusal != nil {
		return nil, s.refusal
	}
	prev := s.pub.Load()
	next := prev.records
	switch p {
	case keyRecords:
		next.keys = maps.Clone(next.keys)
	case teamRecords:
		next.teams = maps.Clone(next.teams)
	case peopleRecords:
		next.people = next.people.Clone()
	}
	ev, err := apply(&next)
	if err != nil {
		return nil, err
	}
	w, err := diff(&prev.records, &next, p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	changed := len(w.stmts) > 0
	if !changed && ev.Action == "" {
		return nil, nil
	}
	m := prev.meta
	m.sequence++
	if w.revises {
		m.revision++
	}
	if ev.Action != "" {
		row := auditRow{seq: int64(m.sequence), Event: audit.Attribute(ctx, ev)}
		if row.Time.IsZero() {
			row.Time = utils.NowUTC()
		}
		if err := row.Validate(); err != nil {
			return nil, err
		}
		args, err := encode(auditCols(&row))
		if err != nil {
			return nil, err
		}
		w.exec(auditTable.insert, args...)
	}
	w.exec(updateMeta, strconv.FormatUint(m.revision, 10), "revision")
	w.exec(updateMeta, strconv.FormatUint(m.sequence, 10), "sequence")
	published := &state{records: next, snap: prev.snap, meta: m}
	if changed {
		published.snap = access.NewSnapshot(next.keys, next.teams, next.people)
	}
	if uncertain, err := s.write(ctx, w); err != nil {
		if !uncertain {
			return nil, err
		}
		if published, err = s.reload(ctx, err, m.sequence); err != nil {
			if errors.Is(err, ErrFailed) && s.fatal != nil {
				return func() { s.fatal(err) }, err
			}
			return nil, err
		}
	}
	s.pub.Store(published)
	if !changed {
		return nil, nil
	}
	c, subs := access.Commit{KeysChanged: p == keyRecords, RemovedKeys: w.removedKeys, RemovedTeams: w.removedTeams}, s.subs
	return func() {
		for _, fn := range subs {
			fn(c)
		}
	}, nil
}

// write runs w in one immediate transaction that the caller's cancellation
// cannot cut short. uncertain marks a failed COMMIT, which may have committed.
func (s *Store) write(ctx context.Context, w *writes) (uncertain bool, err error) {
	ctx = context.WithoutCancel(ctx)
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin access commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, st := range w.stmts {
		if _, err := tx.ExecContext(ctx, st.query, st.args...); err != nil {
			return false, fmt.Errorf("write access records: %w", err)
		}
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(tx); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// reload settles a failed COMMIT by the stored sequence. If the database cannot
// be read back, the store fails closed: it publishes a snapshot that admits no
// one and refuses every later write with ErrFailed.
func (s *Store) reload(ctx context.Context, commitErr error, sequence uint64) (*state, error) {
	stored, err := s.readBack(context.WithoutCancel(ctx))
	if err != nil {
		s.refusal = fmt.Errorf("%w: %w", ErrFailed, errors.Join(commitErr, err))
		s.pub.Store(&state{snap: access.FailedSnapshot(), meta: s.pub.Load().meta})
		return nil, s.refusal
	}
	if stored.meta.sequence != sequence {
		return nil, commitErr
	}
	return stored, nil
}

// readBack loads in a read transaction: if the driver left the failed
// transaction open, BEGIN fails instead of reading that transaction's writes.
func (s *Store) readBack(ctx context.Context) (*state, error) {
	tx, err := s.db.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return load(ctx, tx)
}

// diff validates what p's change made of prev and writes it in stored form.
func diff(prev, next *records, p part) (*writes, error) {
	w := &writes{}
	var err error
	switch p {
	case keyRecords:
		w.removedKeys, err = changedRecords(w, keyTable, "key", prev.keys, next.keys, checkKey, keyCols)
	case teamRecords:
		w.removedTeams, err = changedRecords(w, teamTable, "team", prev.teams, next.teams, (*teams.Team).Validate, teamCols)
	case peopleRecords:
		err = diffPeople(w, prev, next)
	}
	return w, err
}

// changedRecords writes each changed key or team that passes its rule, leaving
// the record in the stored form it is published in, and deletes those next lacks.
func changedRecords[R any](w *writes, t table, kind string, prev, next map[string]*R, check func(*R) error, cols func(*R) []col) ([]string, error) {
	written, removed, err := rows(w, t, prev, next, same, func(id string, r *R) ([]any, error) {
		if err := check(r); err != nil {
			return nil, fmt.Errorf("%s %q: %w", kind, id, err)
		}
		return encode(cols(r))
	})
	w.revises = len(written)+len(removed) > 0
	return removed, err
}

// diffPeople publishes a validated copy of exactly what it writes.
func diffPeople(w *writes, prev, next *records) error {
	before, after := prev.people.Records(), next.people.Records()
	if err := storedPeople(&after); err != nil {
		return err
	}
	written, removed, err := rows(w, personTable, before.People, after.People, same, func(_ string, p people.PersonRecord) ([]any, error) {
		return encode(personCols(&p))
	})
	if err != nil {
		return err
	}
	grants, _, err := rows(w, grantTable, before.Grants, after.Grants, sameGrant, func(hash string, g people.GrantRecord) ([]any, error) {
		return encode(grantCols(&grantRow{hash, g}))
	})
	if err != nil {
		return err
	}
	if before.OwnerID != after.OwnerID || before.Progress != after.Progress {
		args, err := encode(installationCols(&after))
		if err != nil {
			return err
		}
		w.exec(installationTable.upsert, args...)
	}
	// Pruned grants, console-entry codes and progress leave the revision alone.
	w.revises = len(written)+len(removed) > 0
	for _, hash := range grants {
		purpose := after.Grants[hash].Purpose
		w.revises = w.revises || purpose == people.PurposeInvitation || purpose == people.PurposeRecovery
	}
	state, err := people.NewState(after)
	if err != nil {
		return fmt.Errorf("people state: %w", err)
	}
	next.people = state
	return nil
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

// checkKey is Validate plus this store's rule: an env-backed key has no column
// for its credential, so it is refused rather than stored without one.
func checkKey(k *keys.VirtualKey) error {
	if k.KeyEnv != "" {
		return fmt.Errorf("%w: env-backed keys are not stored", keys.ErrInvalid)
	}
	return k.Validate()
}

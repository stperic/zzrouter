package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	keysFileName   = "keys.yaml"
	teamsFileName  = "teams.yaml"
	peopleFileName = "access/people.json"
	maxPeopleBytes = 8 << 20
)

// part names the records, and so the one file, a commit rewrites.
type part int

const (
	keyRecords part = iota
	teamRecords
	peopleRecords
)

var _ access.People = (*people.Store)(nil)

// Options configures Open. With People set, both validators are required.
type Options struct {
	People                 bool // open access/people.json
	ValidateExternalIssuer func(context.Context, string) error
	ValidateTeam           func(*access.Snapshot, string) error
}

// records are the published state; a commit replaces a map or the people state, never edits it.
type records struct {
	keys   map[string]*keys.VirtualKey
	teams  map[string]*teams.Team
	people *people.State
}

// store adapts keys.yaml, teams.yaml and people.json to access.Store.
type store struct {
	dir        string
	write      func(string, []byte, os.FileMode) error
	people     *people.Store
	peopleLock *os.File
	snap       atomic.Pointer[access.Snapshot]

	gate   sync.Mutex // serializes key, team and people commits, their publish and the fields below
	recs   records
	sink   audit.Sink
	subs   []func(access.Commit)
	closed bool
}

// Open loads dir's keys.yaml and teams.yaml, where a missing file is an empty
// set, and opens people when asked. It writes neither file.
func Open(ctx context.Context, dir string, opts Options) (access.Store, error) {
	s := &store{dir: dir, write: atomicfile.WriteFile, sink: audit.Null{}}
	var err error
	if s.recs.keys, err = load(filepath.Join(dir, keysFileName), DecodeKeys); err != nil {
		return nil, fmt.Errorf("load keys.yaml: %w", err)
	}
	if s.recs.teams, err = load(filepath.Join(dir, teamsFileName), DecodeTeams); err != nil {
		return nil, fmt.Errorf("load teams.yaml: %w", err)
	}
	if opts.People {
		if err := s.openPeople(opts); err != nil {
			return nil, fmt.Errorf("initialize people: %w", err)
		}
	}
	s.publishRecords()
	if n := len(s.recs.keys); n > 0 {
		slog.Info("Virtual keys loaded", "count", n)
	}
	if n := len(s.recs.teams); n > 0 {
		slog.Info("Teams loaded", "count", n)
	}
	return s, nil
}

// openPeople builds the transitions, then takes people.json's lock and loads it.
func (s *store) openPeople(opts Options) error {
	var validateTeam func(context.Context, string) error
	if opts.ValidateTeam != nil {
		validateTeam = func(_ context.Context, id string) error { return opts.ValidateTeam(s.Snapshot(), id) }
	}
	var err error
	s.people, err = people.New(people.Options{
		ValidateExternalIssuer: opts.ValidateExternalIssuer, ValidateTeam: validateTeam,
		Mutate: func(ctx context.Context, change func(*people.State) (audit.Event, error)) (*people.State, error) {
			var committed *records // publish leaves the published people state here
			err := s.commit(ctx, peopleRecords, func(r *records) (audit.Event, error) {
				committed = r
				return change(r.people)
			})
			if err != nil {
				return nil, err
			}
			return committed.people, nil
		},
		View: func() *people.State { return s.Snapshot().PeopleState() },
	})
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, peopleFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := security.RestrictDirectoryPermissions(filepath.Dir(path)); err != nil {
		return err
	}
	if s.peopleLock, err = atomicfile.Lock(path + ".lock"); err != nil {
		return fmt.Errorf("lock people snapshot: %w", err)
	}
	if s.recs.people, err = loadPeople(path); err != nil {
		_ = s.peopleLock.Close()
		return err
	}
	return nil
}

func loadPeople(path string) (*people.State, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPeopleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPeopleBytes {
		return nil, fmt.Errorf("people snapshot exceeds size limit")
	}
	return people.DecodeState(data)
}

// publishRecords makes recs the state every reader sees.
func (s *store) publishRecords() {
	s.snap.Store(access.NewSnapshot(s.recs.keys, s.recs.teams, s.recs.people))
}

func load[T any](path string, decode func([]byte) (map[string]*T, error)) (map[string]*T, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*T{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(data)
}

func (s *store) Snapshot() *access.Snapshot { return s.snap.Load() }

func (s *store) OnCommit(fn func(access.Commit)) {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.subs = append(s.subs, fn)
}

func (s *store) People() access.People {
	if s.people == nil {
		return nil
	}
	return s.people
}

func (s *store) SetAuditSink(sink audit.Sink) {
	if sink == nil {
		sink = audit.Null{}
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	s.sink = sink
}

func (s *store) Record(ctx context.Context, ev audit.Event) {
	s.gate.Lock()
	sink, closed := s.sink, s.closed
	s.gate.Unlock()
	if !closed {
		record(ctx, sink, ev)
	}
}

func (s *store) Close() error {
	s.gate.Lock()
	defer s.gate.Unlock()
	wasClosed := s.closed
	s.closed = true
	if wasClosed || s.peopleLock == nil {
		return nil
	}
	return s.peopleLock.Close()
}

func record(ctx context.Context, sink audit.Sink, ev audit.Event) {
	if ev.Action != "" {
		audit.Log(ctx, sink, ev)
	}
}

// commit applies change to a copy of one file's records, persists that file,
// publishes the result and records the returned event; subscribers run after
// the gate is released.
func (s *store) commit(ctx context.Context, f part, change func(*records) (audit.Event, error)) error {
	c, subs, err := s.publish(ctx, f, change)
	if err != nil {
		return err
	}
	for _, fn := range subs {
		fn(c)
	}
	return nil
}

func (s *store) publish(ctx context.Context, f part, change func(*records) (audit.Event, error)) (access.Commit, []func(access.Commit), error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.closed {
		return access.Commit{}, nil, access.ErrClosed
	}
	next := s.recs
	switch f {
	case keyRecords:
		next.keys = maps.Clone(next.keys)
	case teamRecords:
		next.teams = maps.Clone(next.teams)
	case peopleRecords:
		next.people = next.people.Clone()
	}
	ev, err := change(&next)
	if err != nil {
		return access.Commit{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return access.Commit{}, nil, err
	}
	c := access.Commit{KeysChanged: f == keyRecords}
	var data []byte
	var name string
	switch f {
	case keyRecords:
		name, c.RemovedKeys = keysFileName, removed(s.recs.keys, next.keys)
		data, err = encodeKeys(next.keys)
	case teamRecords:
		name, c.RemovedTeams = teamsFileName, removed(s.recs.teams, next.teams)
		data, err = encodeTeams(next.teams)
	case peopleRecords:
		name = peopleFileName
		// Built through the validator, so a commit never publishes what a reopen refuses.
		if next.people, err = people.NewState(next.people.Records()); err != nil {
			return access.Commit{}, nil, fmt.Errorf("people state: %w", err)
		}
		if data, err = json.Marshal(next.people); err == nil && len(data) > maxPeopleBytes {
			return access.Commit{}, nil, people.ErrCapacity
		}
	}
	if err != nil {
		return access.Commit{}, nil, fmt.Errorf("encode %s: %w", name, err)
	}
	// A durability error after the rename still committed the new file.
	if err := s.write(filepath.Join(s.dir, name), data, 0o600); errors.Is(err, atomicfile.ErrPostRenameDurability) {
		slog.Warn("Access state committed with uncertain durability", "file", name, "error", err)
	} else if err != nil {
		return access.Commit{}, nil, fmt.Errorf("save %s: %w", name, err)
	}
	s.recs = next
	s.publishRecords()
	record(ctx, s.sink, ev)
	return c, s.subs, nil
}

func removed[T any](before, after map[string]*T) []string {
	var ids []string
	for id := range before {
		if _, ok := after[id]; !ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func credential() (string, string, error) {
	raw, err := keys.GenerateRawKey()
	if err != nil {
		return "", "", err
	}
	hash, err := keys.HashKey(raw)
	return raw, hash, err
}

func (s *store) CreateKey(ctx context.Context, id string, key keys.VirtualKey, ev audit.Event) (*keys.VirtualKey, string, error) {
	raw, hash, err := credential()
	if err != nil {
		return nil, "", err
	}
	created := key.Clone()
	err = s.commit(ctx, keyRecords, func(r *records) (audit.Event, error) {
		if _, exists := r.keys[id]; exists {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, keys.ErrKeyExists)
		}
		created.ID, created.UUID, created.HashedKey, created.KeyEnv = id, uuid.NewString(), hash, ""
		created.CreatedAt = utils.NowUTC()
		created.UpdatedAt = created.CreatedAt
		if err := created.Validate(); err != nil {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, err)
		}
		r.keys[id] = created
		return ev, nil
	})
	if err != nil {
		return nil, "", err
	}
	return created.Clone(), raw, nil
}

func (s *store) UpdateKey(ctx context.Context, id string, apply func(*keys.VirtualKey) error, ev audit.Event) (*keys.VirtualKey, error) {
	return s.replaceKey(ctx, id, ev, func(existing *keys.VirtualKey) (*keys.VirtualKey, error) {
		next := existing.Clone()
		if err := apply(next); err != nil {
			return nil, err
		}
		next = next.Clone()
		next.ID, next.UUID, next.HashedKey, next.KeyEnv = id, existing.UUID, existing.HashedKey, existing.KeyEnv
		next.TeamID, next.TeamRole = existing.TeamID, existing.TeamRole
		next.CreatedAt, next.CreatedBy = existing.CreatedAt, existing.CreatedBy
		return next, nil
	})
}

// RotateKey replaces the credential; an env-backed key becomes a hashed key.
func (s *store) RotateKey(ctx context.Context, id string, ev audit.Event) (*keys.VirtualKey, string, error) {
	raw, hash, err := credential()
	if err != nil {
		return nil, "", err
	}
	rotated, err := s.replaceKey(ctx, id, ev, func(existing *keys.VirtualKey) (*keys.VirtualKey, error) {
		next := existing.Clone()
		next.HashedKey, next.KeyEnv = hash, ""
		return next, nil
	})
	if err != nil {
		return nil, "", err
	}
	return rotated, raw, nil
}

func (s *store) replaceKey(ctx context.Context, id string, ev audit.Event, replace func(*keys.VirtualKey) (*keys.VirtualKey, error)) (*keys.VirtualKey, error) {
	var next *keys.VirtualKey
	err := s.commit(ctx, keyRecords, func(r *records) (audit.Event, error) {
		existing, ok := r.keys[id]
		if !ok {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
		}
		var err error
		if next, err = replace(existing); err != nil {
			return audit.Event{}, err
		}
		next.UpdatedAt = utils.NowUTC()
		if err := next.Validate(); err != nil {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, err)
		}
		r.keys[id] = next
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return next.Clone(), nil
}

func (s *store) DeleteKey(ctx context.Context, id string, ev audit.Event) error {
	return s.commit(ctx, keyRecords, func(r *records) (audit.Event, error) {
		if _, ok := r.keys[id]; !ok {
			return audit.Event{}, fmt.Errorf("key %q: %w", id, keys.ErrKeyNotFound)
		}
		delete(r.keys, id)
		return ev, nil
	})
}

func (s *store) CreateTeam(ctx context.Context, id string, team teams.Team, ev audit.Event) (*teams.Team, error) {
	created := team.Clone()
	err := s.commit(ctx, teamRecords, func(r *records) (audit.Event, error) {
		if _, exists := r.teams[id]; exists {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, teams.ErrTeamExists)
		}
		created.ID, created.UUID = id, uuid.NewString()
		created.CreatedAt = utils.NowUTC()
		created.UpdatedAt = created.CreatedAt
		if err := created.Validate(); err != nil {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, err)
		}
		r.teams[id] = created
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return created.Clone(), nil
}

func (s *store) UpdateTeam(ctx context.Context, id string, apply func(*teams.Team) error, ev audit.Event) (*teams.Team, error) {
	var next *teams.Team
	err := s.commit(ctx, teamRecords, func(r *records) (audit.Event, error) {
		existing, ok := r.teams[id]
		if !ok {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
		}
		next = existing.Clone()
		if err := apply(next); err != nil {
			return audit.Event{}, err
		}
		next = next.Clone()
		next.ID, next.UUID, next.Kind = id, existing.UUID, existing.Kind
		next.CreatedAt, next.CreatedBy = existing.CreatedAt, existing.CreatedBy
		next.UpdatedAt = utils.NowUTC()
		if err := next.Validate(); err != nil {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, err)
		}
		r.teams[id] = next
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return next.Clone(), nil
}

func (s *store) DeleteTeam(ctx context.Context, id string, ev audit.Event) error {
	return s.commit(ctx, teamRecords, func(r *records) (audit.Event, error) {
		if _, ok := r.teams[id]; !ok {
			return audit.Event{}, fmt.Errorf("team %q: %w", id, teams.ErrTeamNotFound)
		}
		delete(r.teams, id)
		return ev, nil
	})
}

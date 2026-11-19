package people

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
	"golang.org/x/crypto/argon2"
)

const passwordPrefix = "$argon2id$v=19$m=65536,t=3,p=1$" //nolint:gosec // Public Argon2 format/parameters, not a credential.

type loginAttempt struct {
	Count int
	Until time.Time
}

func passwordInput(password string) bool {
	return utf8.ValidString(password) && len(password) <= 512 && utf8.RuneCountInString(password) >= 12 && utf8.RuneCountInString(password) <= 128
}

func parsePasswordHash(encoded string) ([]byte, []byte, bool) {
	if len(encoded) > 256 || !strings.HasPrefix(encoded, passwordPrefix) {
		return nil, nil, false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordPrefix), "$")
	if len(parts) != 2 {
		return nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return nil, nil, false
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil || len(hash) != 32 {
		return nil, nil, false
	}
	return salt, hash, true
}

func validPasswordHash(encoded string) bool { _, _, valid := parsePasswordHash(encoded); return valid }

func (s *Store) passwordWork(ctx context.Context, work func() bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	select {
	case s.passwords <- struct{}{}:
	default:
		return false, ErrLimited
	}
	defer func() { <-s.passwords }()
	valid := work()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return valid, nil
}

func (s *Store) hashPassword(ctx context.Context, password string) (string, error) {
	if !passwordInput(password) {
		return "", fmt.Errorf("%w: use a password with 12 to 128 characters", ErrInvalidInput)
	}
	var encoded string
	_, err := s.passwordWork(ctx, func() bool {
		salt := randomBytes(16)
		hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
		encoded = passwordPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)
		return true
	})
	return encoded, err
}

func (s *Store) verifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	if !passwordInput(password) {
		return false, nil
	}
	salt, expected, valid := parsePasswordHash(encoded)
	if !valid {
		salt = make([]byte, 16)
		expected = make([]byte, 32)
	}
	return s.passwordWork(ctx, func() bool {
		actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
		return subtle.ConstantTimeCompare(actual, expected) == 1 && valid
	})
}

func (s *Store) admitLogin(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := utils.NowUTC()
	for key, attempt := range s.attempts {
		if !now.Before(attempt.Until) {
			delete(s.attempts, key)
		}
	}
	key := digest(username)
	attempt := s.attempts[key]
	if attempt.Count >= 10 || attempt.Count == 0 && len(s.attempts) >= 2048 {
		return ErrLimited
	}
	if attempt.Count == 0 {
		attempt.Until = now.Add(time.Minute)
	}
	attempt.Count++
	s.attempts[key] = attempt
	return nil
}

// Authenticate verifies credentials outside any writer gate, then checks the
// same revision again so a concurrent reset cannot authenticate an old password.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*Person, error) {
	canonical, err := normalizeUsername(username)
	if err != nil {
		canonical = strings.ToLower(strings.TrimSpace(username))
		if len(canonical) > 256 {
			return nil, ErrInvalidCredentials
		}
	}
	if err := s.admitLogin(canonical); err != nil {
		return nil, err
	}
	var candidate PersonRecord
	before, _ := s.view()
	for _, record := range before.people {
		if record.Username == canonical {
			candidate = record
			break
		}
	}
	valid, err := s.verifyPassword(ctx, password, candidate.PasswordHash)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInvalidCredentials
	}
	state, teams := s.view()
	current, ok := state.people[candidate.ID]
	if !ok || current.Revision != candidate.Revision || current.Suspended {
		return nil, ErrInvalidCredentials
	}
	if err := teams(current.TeamID); err != nil {
		return nil, ErrInvalidCredentials
	}
	p := current.Person
	return &p, nil
}

// ChangePassword verifies the existing password and invalidates prior revisions.
func (s *Store) ChangePassword(ctx context.Context, id, revision, currentPassword, newPassword string) (*Person, error) {
	state, _ := s.view()
	p := state.Person(id)
	if p == nil || p.Revision != revision {
		return nil, ErrInvalidCredentials
	}
	verified, err := s.Authenticate(ctx, p.Username, currentPassword)
	if err != nil || verified.Revision != revision {
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidCredentials
	}
	hash, err := s.hashPassword(ctx, newPassword)
	if err != nil {
		return nil, err
	}
	committed, err := s.options.Mutate(ctx, func(state *State, _ TeamCheck) (audit.Event, error) {
		record, ok := state.people[id]
		if !ok || record.Suspended || record.Revision != revision {
			return audit.Event{}, ErrInvalidCredentials
		}
		record.PasswordHash = hash
		record.Revision = randomValue("")
		record.UpdatedAt = utils.NowUTC()
		state.people[id] = record
		invalidatePersonGrants(state, id)
		return audit.Event{Action: audit.ActionPasswordChanged, Actor: id, Subject: id, Metadata: map[string]any{"authority_invalidated": true}}, nil
	})
	if err != nil {
		return nil, err
	}
	return committed.Person(id), nil
}

// RevokeSessions advances the current revision without changing its password.
func (s *Store) RevokeSessions(ctx context.Context, id, revision string) (*Person, error) {
	committed, err := s.options.Mutate(ctx, func(state *State, _ TeamCheck) (audit.Event, error) {
		record, ok := state.people[id]
		if !ok || record.Suspended || record.Revision != revision {
			return audit.Event{}, ErrInvalidCredentials
		}
		record.Revision = randomValue("")
		record.UpdatedAt = utils.NowUTC()
		state.people[id] = record
		invalidatePersonGrants(state, id)
		return audit.Event{Action: audit.ActionSessionsRevoked, Actor: id, Subject: id, Metadata: map[string]any{"authority_invalidated": true}}, nil
	})
	if err != nil {
		return nil, err
	}
	return committed.Person(id), nil
}

// Restaged is a copy of r as a restore publishes it: every person gets a new
// credential revision, as RevokeSessions gives, so sessions and delegated
// grants issued before stop resolving, and every pending grant expires now.
// Password hashes and used, revoked or expired grants are kept.
func Restaged(r Records) Records {
	r.People, r.Grants = maps.Clone(r.People), maps.Clone(r.Grants)
	for id, record := range r.People {
		record.Revision = randomValue("")
		r.People[id] = record
	}
	now := utils.NowUTC()
	for hash, grant := range r.Grants {
		if active(grant.Grant) {
			grant.ExpiresAt = now
			r.Grants[hash] = grant
		}
	}
	return r
}

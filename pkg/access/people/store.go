package people

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	maxPeople     = 10000
	maxGrants     = 4096
	maxStateBytes = 8 << 20
)

type personRecord struct {
	Person
	PasswordHash string `json:"password_hash"`
}

type grantRecord struct {
	Grant
	Issuer      Issuer `json:"issuer"`
	BrowserHash string `json:"browser_hash,omitempty"`
}

type snapshot struct {
	Version     int                     `json:"version"`
	Initialized bool                    `json:"initialized"`
	OwnerID     string                  `json:"owner_id,omitempty"`
	People      map[string]personRecord `json:"people"`
	Grants      map[string]grantRecord  `json:"grants"`
	Progress    Progress                `json:"progress"`
}

// Store serializes identity mutations and owns its snapshot for the process lifetime.
// Lock order: server team mutation gate, Store.mu, read-only key/team stores.
// Neither callbacks nor mutations may acquire the browser session authority lock.
type Store struct {
	mu        sync.Mutex
	path      string
	lock      *os.File
	state     snapshot
	options   Options
	audit     audit.Sink
	closed    bool
	passwords chan struct{}
	attempts  map[string]loginAttempt
	write     func(string, []byte, os.FileMode) error
}

// Open locks and loads one installation's people snapshot. A missing file is unclaimed.
func Open(ctx context.Context, path string, options Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" || options.ValidateExternalIssuer == nil || options.ValidateTeam == nil {
		return nil, fmt.Errorf("people store requires a path and authority validators")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create people directory: %w", err)
	}
	if err := security.RestrictDirectoryPermissions(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("restrict people directory: %w", err)
	}
	lock, err := atomicfile.Lock(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock people snapshot: %w", err)
	}
	s := &Store{path: path, lock: lock, options: options, passwords: make(chan struct{}, 2), attempts: make(map[string]loginAttempt), write: atomicfile.WriteFile,
		state: snapshot{Version: 1, People: make(map[string]personRecord), Grants: make(map[string]grantRecord)}}
	if err := s.load(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open people snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return fmt.Errorf("read people snapshot: %w", err)
	}
	s.state, err = decodeSnapshot(data)
	return err
}

// DecodeSnapshot validates persisted account state and returns safe person
// projections so recovery can check references without copying the wire format.
func DecodeSnapshot(data []byte) ([]Person, error) {
	state, err := decodeSnapshot(data)
	if err != nil {
		return nil, err
	}
	result := make([]Person, 0, len(state.People))
	for _, record := range state.People {
		result = append(result, record.Person)
	}
	return result, nil
}

func decodeSnapshot(data []byte) (snapshot, error) {
	if len(data) > maxStateBytes {
		return snapshot{}, fmt.Errorf("people snapshot exceeds size limit")
	}
	var state snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return snapshot{}, fmt.Errorf("decode people snapshot: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return snapshot{}, fmt.Errorf("people snapshot has trailing data")
	}
	if err := validateSnapshot(state); err != nil {
		return snapshot{}, err
	}
	return state, nil
}

func validateSnapshot(state snapshot) error {
	if state.Version != 1 || state.People == nil || state.Grants == nil || len(state.People) > maxPeople || len(state.Grants) > maxGrants {
		return fmt.Errorf("invalid people snapshot structure")
	}
	if state.Initialized != (state.OwnerID != "") {
		return fmt.Errorf("invalid people ownership marker")
	}
	if err := validateSnapshotPeople(state); err != nil {
		return err
	}
	for hash, grant := range state.Grants {
		if len(hash) != 64 || grant.ID == "" || !validPurpose(grant.Purpose) || grant.ExpiresAt.IsZero() || !validIssuer(grant.Issuer) {
			return fmt.Errorf("invalid people grant snapshot")
		}
	}
	return nil
}

func validateSnapshotPeople(state snapshot) error {
	owners := 0
	usernames := make(map[string]bool, len(state.People))
	for id, record := range state.People {
		p := record.Person
		if id != p.ID || validatePerson(p) != nil || usernames[p.Username] || !validPasswordHash(record.PasswordHash) {
			return fmt.Errorf("invalid people snapshot account")
		}
		usernames[p.Username] = true
		if p.Owner {
			owners++
			if id != state.OwnerID || p.Role != "admin" || p.Suspended {
				return fmt.Errorf("invalid owner account")
			}
		}
	}
	if state.Initialized && owners != 1 || !state.Initialized && len(state.People) != 0 {
		return fmt.Errorf("invalid people ownership state")
	}
	return nil
}

// Close releases ownership. Closed stores cannot authenticate or mutate state.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

func (s *Store) mutate(ctx context.Context, event *audit.Event, change func(*snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := s.state
	next.People = maps.Clone(s.state.People)
	next.Grants = maps.Clone(s.state.Grants)
	if err := change(&next); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode people snapshot: %w", err)
	}
	if len(data) > maxStateBytes {
		return ErrCapacity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = s.write(s.path, data, 0o600); err != nil && !errors.Is(err, atomicfile.ErrPostRenameDurability) {
		return fmt.Errorf("save people snapshot: %w", err)
	}
	s.state = next
	if event != nil && event.Action != "" {
		audit.Log(ctx, s.audit, *event)
	}
	if err != nil {
		slog.Warn("People snapshot committed with uncertain directory durability")
	}
	return nil
}

// SetAuditSink shares the server-owned sink without taking ownership of its lifetime.
func (s *Store) SetAuditSink(sink audit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

// Initialized reports the permanent owner-claim marker.
func (s *Store) Initialized() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Initialized }

// Get returns a safe copy, or nil for an absent account or closed store.
func (s *Store) Get(id string) *Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	record, ok := s.state.People[id]
	if !ok {
		return nil
	}
	p := record.Person
	return &p
}

// Owner returns the immutable initial owner, if claimed.
func (s *Store) Owner() *Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.state.Initialized {
		return nil
	}
	p := s.state.People[s.state.OwnerID].Person
	return &p
}

// List returns public account snapshots ordered by username.
func (s *Store) List() []Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Person, 0, len(s.state.People))
	if !s.closed {
		for _, record := range s.state.People {
			items = append(items, record.Person)
		}
	}
	slices.SortFunc(items, func(a, b Person) int { return strings.Compare(a.Username, b.Username) })
	return items
}

// TeamDependencies is the durable membership and pending enrollment impact.
// Suspended people still belong to their team. Pending grants are counted by
// lifecycle, even if their issuer can no longer authorize redemption.
type TeamDependencies struct {
	People           int  `json:"people"`
	Invitations      int  `json:"invitations"`
	EnrollmentGrants int  `json:"enrollment_grants"`
	OwnerProtected   bool `json:"owner_protected"`
}

// HasMembers reports whether stored people or pending grants depend on this team.
func (d TeamDependencies) HasMembers() bool {
	return d.People+d.Invitations+d.EnrollmentGrants > 0
}

// TeamImpact is the canonical projection used by both access UI and deletion.
func (s *Store) TeamImpact(id string) TeamDependencies {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result TeamDependencies
	for _, record := range s.state.People {
		if record.TeamID == id {
			result.People++
			result.OwnerProtected = result.OwnerProtected || record.Owner
		}
	}
	for _, grant := range s.state.Grants {
		if grant.TeamID == id && active(grant.Grant) {
			if grant.Purpose == PurposeInvitation {
				result.Invitations++
			} else {
				result.EnrollmentGrants++
			}
		}
	}
	return result
}

// ReferencedTeam prevents removal while identities or pending grants use a team.
func (s *Store) ReferencedTeam(id string) bool { return s.TeamImpact(id).HasMembers() }

// OwnerTeam reports whether suspension or deletion would lock out the owner.
func (s *Store) OwnerTeam(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Initialized && s.state.People[s.state.OwnerID].TeamID == id
}

func randomValue(prefix string) string {
	return prefix + base64.RawURLEncoding.EncodeToString(randomBytes(32))
}
func randomBytes(size int) []byte { b := make([]byte, size); _, _ = rand.Read(b); return b }
func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func normalizeUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 64 {
		return "", fmt.Errorf("%w: username must contain 3 to 64 letters, digits, dots, underscores or hyphens", ErrInvalidInput)
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-') {
			return "", fmt.Errorf("%w: username must contain letters, digits, dots, underscores or hyphens", ErrInvalidInput)
		}
	}
	return value, nil
}

func validatePerson(p Person) error {
	username, err := normalizeUsername(p.Username)
	if err != nil || username != p.Username || p.ID == "" || p.Revision == "" || !validRole(p.Role) || p.TeamID == "" || !validName(p.Name) {
		return ErrInvalidInput
	}
	return nil
}

func validName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 120
}
func validRole(role string) bool { return role == "admin" || role == "user" }
func validPurpose(p Purpose) bool {
	return p == PurposeSetup || p == PurposeSession || p == PurposeInvitation || p == PurposeRecovery
}
func active(grant Grant) bool {
	return grant.UsedAt == nil && grant.RevokedAt == nil && utils.NowUTC().Before(grant.ExpiresAt)
}

func publicGrant(record grantRecord) Grant {
	g := record.Grant
	if g.UsedAt != nil {
		value := *g.UsedAt
		g.UsedAt = &value
	}
	if g.RevokedAt != nil {
		value := *g.RevokedAt
		g.RevokedAt = &value
	}
	switch {
	case g.UsedAt != nil:
		g.Status = "accepted"
	case g.RevokedAt != nil:
		g.Status = "revoked"
	case !utils.NowUTC().Before(g.ExpiresAt):
		g.Status = "expired"
	default:
		g.Status = "pending"
	}
	return g
}

// Progress returns the persisted onboarding state.
func (s *Store) Progress() Progress { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Progress }

// UpdateProgress saves navigation choices without accepting success evidence.
func (s *Store) UpdateProgress(ctx context.Context, update ProgressUpdate) error {
	return s.mutate(ctx, nil, func(state *snapshot) error {
		if update.Step != nil {
			switch *update.Step {
			case "", "owner", "provider", "model", "test", "app", "complete":
				state.Progress.Step = *update.Step
			default:
				return ErrInvalidInput
			}
		}
		if update.Dismissed != nil {
			state.Progress.Dismissed = *update.Dismissed
		}
		state.Progress.UpdatedAt = utils.NowUTC()
		return nil
	})
}

// MarkResponse records successful routed inference by the initial owner.
func (s *Store) MarkResponse(ctx context.Context, personID string) error {
	return s.mutate(ctx, nil, func(state *snapshot) error {
		if !state.Initialized || state.OwnerID != personID {
			return ErrNotFound
		}
		state.Progress.ResponseVerified = true
		state.Progress.UpdatedAt = utils.NowUTC()
		return nil
	})
}

// MarkAppConnected records a successful inference request from an application key.
func (s *Store) MarkAppConnected(ctx context.Context) error {
	return s.mutate(ctx, nil, func(state *snapshot) error {
		state.Progress.AppConnected = true
		state.Progress.UpdatedAt = utils.NowUTC()
		return nil
	})
}
